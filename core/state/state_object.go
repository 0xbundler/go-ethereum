// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package state

import (
	"bytes"
	"fmt"
	"github.com/ethereum/go-ethereum/core/state/snapshot"
	"io"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie/trienode"
)

type Code []byte

func (c Code) String() string {
	return string(c) //strings.Join(Disassemble(c), " ")
}

type Storage map[common.Hash]common.Hash

func (s Storage) String() (str string) {
	for key, value := range s {
		str += fmt.Sprintf("%X : %X\n", key, value)
	}
	return
}

func (s Storage) Copy() Storage {
	cpy := make(Storage, len(s))
	for key, value := range s {
		cpy[key] = value
	}
	return cpy
}

// stateObject represents an Ethereum account which is being modified.
//
// The usage pattern is as follows:
// - First you need to obtain a state object.
// - Account values as well as storages can be accessed and modified through the object.
// - Finally, call commit to return the changes of storage trie and update account data.
type stateObject struct {
	db       *StateDB
	address  common.Address      // address of ethereum account
	addrHash common.Hash         // hash of ethereum address of the account
	origin   *types.StateAccount // Account original data without any change applied, nil means it was not existent
	data     types.StateAccount  // Account data with all mutations applied in the scope of block

	// Write caches.
	trie Trie // storage trie, which becomes non-nil on first access, it's committed trie
	code Code // contract bytecode, which gets set when code is loaded

	originStorage  Storage // Storage cache of original entries to dedup rewrites
	pendingStorage Storage // Storage entries that need to be flushed to disk, at the end of an entire block
	dirtyStorage   Storage // Storage entries that have been modified in the current transaction execution, reset for every transaction

	// for state expiry feature
	pendingReviveTrie    Trie                             // pendingReviveTrie it contains pending revive trie nodes, could update & commit later
	pendingReviveState   map[string]common.Hash           // pendingReviveState for block, when R&W, access revive state first
	pendingAccessedState map[common.Hash]int              // pendingAccessedState record which state is accessed(only read now, update/delete/insert will auto update epoch), it will update epoch index late
	originStorageEpoch   map[common.Hash]types.StateEpoch // originStorageEpoch record origin state epoch, prevent frequency epoch update

	// Cache flags.
	dirtyCode bool // true if the code was updated

	// Flag whether the account was marked as self-destructed. The self-destructed account
	// is still accessible in the scope of same transaction.
	selfDestructed bool

	// Flag whether the account was marked as deleted. A self-destructed account
	// or an account that is considered as empty will be marked as deleted at
	// the end of transaction and no longer accessible anymore.
	deleted bool

	// Flag whether the object was created in the current transaction
	created bool
}

// empty returns whether the account is considered empty.
func (s *stateObject) empty() bool {
	return s.data.Nonce == 0 && s.data.Balance.Sign() == 0 && bytes.Equal(s.data.CodeHash, types.EmptyCodeHash.Bytes())
}

// newObject creates a state object.
func newObject(db *StateDB, address common.Address, acct *types.StateAccount) *stateObject {
	origin := acct
	if acct == nil {
		acct = types.NewEmptyStateAccount()
	}
	return &stateObject{
		db:                   db,
		address:              address,
		addrHash:             crypto.Keccak256Hash(address[:]),
		origin:               origin,
		data:                 *acct,
		originStorage:        make(Storage),
		pendingStorage:       make(Storage),
		dirtyStorage:         make(Storage),
		pendingReviveState:   make(map[string]common.Hash),
		pendingAccessedState: make(map[common.Hash]int),
		originStorageEpoch:   make(map[common.Hash]types.StateEpoch),
	}
}

// EncodeRLP implements rlp.Encoder.
func (s *stateObject) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &s.data)
}

func (s *stateObject) markSelfdestructed() {
	s.selfDestructed = true
}

func (s *stateObject) touch() {
	s.db.journal.append(touchChange{
		account: &s.address,
	})
	if s.address == ripemd {
		// Explicitly put it in the dirty-cache, which is otherwise generated from
		// flattened journals.
		s.db.journal.dirty(s.address)
	}
}

// getTrie returns the associated storage trie. The trie will be opened
// if it's not loaded previously. An error will be returned if trie can't
// be loaded.
func (s *stateObject) getTrie() (Trie, error) {
	if s.trie == nil {
		// Try fetching from prefetcher first
		if s.data.Root != types.EmptyRootHash && s.db.prefetcher != nil {
			// When the miner is creating the pending state, there is no prefetcher
			s.trie = s.db.prefetcher.trie(s.addrHash, s.data.Root)
		}
		if s.trie == nil {
			// TODO(0xbundler): if any change to open a storage trie in state expiry feature?
			tr, err := s.db.db.OpenStorageTrie(s.db.originalRoot, s.address, s.data.Root)
			if err != nil {
				return nil, err
			}
			s.trie = tr
		}
	}
	return s.trie, nil
}

func (s *stateObject) getPendingReviveTrie() (Trie, error) {
	if s.pendingReviveTrie == nil {
		src, err := s.getTrie()
		if err != nil {
			return nil, err
		}
		s.pendingReviveTrie = s.db.db.CopyTrie(src)
	}
	return s.pendingReviveTrie, nil
}

// GetState retrieves a value from the account storage trie.
func (s *stateObject) GetState(key common.Hash) common.Hash {
	// If we have a dirty value for this state entry, return it
	value, dirty := s.dirtyStorage[key]
	if dirty {
		return value
	}
	// Otherwise return the entry's original value
	value = s.GetCommittedState(key)
	if value != (common.Hash{}) {
		s.accessState(key)
	}
	return value
}

// GetCommittedState retrieves a value from the committed account storage trie.
func (s *stateObject) GetCommittedState(key common.Hash) common.Hash {
	// If we have a pending write or clean cached, return that
	if value, pending := s.pendingStorage[key]; pending {
		return value
	}

	if s.db.EnableExpire() {
		if revived, revive := s.queryFromReviveState(s.pendingReviveState, key); revive {
			return revived
		}
	}

	if value, cached := s.originStorage[key]; cached {
		return value
	}

	// If the object was destructed in *this* block (and potentially resurrected),
	// the storage has been cleared out, and we should *not* consult the previous
	// database about any storage values. The only possible alternatives are:
	//   1) resurrect happened, and new slot values were set -- those should
	//      have been handles via pendingStorage above.
	//   2) we don't have new values, and can deliver empty response back
	if _, destructed := s.db.stateObjectsDestruct[s.address]; destructed {
		return common.Hash{}
	}
	// If no live objects are available, attempt to use snapshots
	var (
		enc   []byte
		err   error
		value common.Hash
	)
	if s.db.snap != nil {
		start := time.Now()
		// handle state expiry situation
		if s.db.EnableExpire() {
			enc, err = s.db.snap.Storage(s.addrHash, crypto.Keccak256Hash(key.Bytes()))
			if err == snapshot.ErrStorageExpired {
				// TODO(0xbundler): request from remote node;
				// TODO: query from dirty revive trie, got the newest expired info
				// TODO: handle from remoteDB, if got err just setError, just return to revert in consensus version .
				//_, err = s.getDirtyReviveTrie(db).TryGet(key.Bytes())
				//if enErr, ok := err.(*trie.ExpiredNodeError); ok {
				//	return common.Hash{}, NewExpiredStateError(s.address, key, enErr).Reason("snap query")
				//}
				// proof, remoteErr := request()
				// if remoteErr != nil {
				//		s.db.setError(remoteErr)
				// return common.hash{}
				// }
				// TODO: add to revive trie with new epoch, add pending state for snapshot
				err = nil
			}
			if len(enc) > 0 {
				var sv snapshot.SnapValue
				sv, err = snapshot.DecodeValueFromRLPBytes(enc)
				if err == nil {
					value.SetBytes(sv.GetVal())
					s.originStorageEpoch[key] = sv.GetEpoch()
				} else {
					enc = []byte{}
				}
			}
		} else {
			enc, err = s.db.snap.Storage(s.addrHash, crypto.Keccak256Hash(key.Bytes()))
			if len(enc) > 0 {
				_, content, _, err := rlp.Split(enc)
				if err != nil {
					s.db.setError(err)
				}
				value.SetBytes(content)
			}
		}
		if metrics.EnabledExpensive {
			s.db.SnapshotStorageReads += time.Since(start)
		}
	}

	// If the snapshot is unavailable or reading from it fails, load from the database.
	if s.needLoadFromTrie(enc, err) {
		start := time.Now()
		tr, err := s.getTrie()
		if err != nil {
			s.db.setError(err)
			return common.Hash{}
		}
		val, err := tr.GetStorage(s.address, key.Bytes())
		if metrics.EnabledExpensive {
			s.db.StorageReads += time.Since(start)
		}
		// handle state expiry situation
		if s.db.EnableExpire() {
			// TODO(0xbundler): using trie expired error
			if err == snapshot.ErrStorageExpired {
				// TODO(0xbundler): request from remote node;
				// TODO: query from dirty revive trie, got the newest expired info
				// TODO: handle from remoteDB, if got err just setError, just return to revert in consensus version .
				//_, err = s.getDirtyReviveTrie(db).TryGet(key.Bytes())
				//if enErr, ok := err.(*trie.ExpiredNodeError); ok {
				//	return common.Hash{}, NewExpiredStateError(s.address, key, enErr).Reason("snap query")
				//}
				// proof, remoteErr := request()
				// if remoteErr != nil {
				//		s.db.setError(remoteErr)
				// }
				// TODO: add to revive trie with new epoch
				err = nil
			}
			//val = queried
			// TODO(0xbundler): add epoch record cache for prevent frequency access epoch update, may implement later
			//s.originStorageEpoch[key] = epoch
		}
		if err != nil {
			s.db.setError(err)
			return common.Hash{}
		}
		value.SetBytes(val)
	}
	s.originStorage[key] = value
	return value
}

// needLoadFromTrie If not found in snap when EnableExpire(), need check insert duplication from trie.
func (s *stateObject) needLoadFromTrie(enc []byte, err error) bool {
	if s.db.snap == nil {
		return true
	}
	if !s.db.EnableExpire() {
		return err != nil
	}

	if err != nil || len(enc) == 0 {
		return true
	}

	return false
}

// SetState updates a value in account storage.
func (s *stateObject) SetState(key, value common.Hash) {
	// If the new value is the same as old, don't set
	prev := s.GetState(key)
	if prev == value {
		return
	}
	// New value is different, update and journal the change
	s.db.journal.append(storageChange{
		account:  &s.address,
		key:      key,
		prevalue: prev,
	})
	s.setState(key, value)
}

func (s *stateObject) setState(key, value common.Hash) {
	s.dirtyStorage[key] = value
}

// finalise moves all dirty storage slots into the pending area to be hashed or
// committed later. It is invoked at the end of every transaction.
func (s *stateObject) finalise(prefetch bool) {
	slotsToPrefetch := make([][]byte, 0, len(s.dirtyStorage))
	for key, value := range s.dirtyStorage {
		s.pendingStorage[key] = value
		if value != s.originStorage[key] {
			slotsToPrefetch = append(slotsToPrefetch, common.CopyBytes(key[:])) // Copy needed for closure
		}
	}
	if s.db.prefetcher != nil && prefetch && len(slotsToPrefetch) > 0 && s.data.Root != types.EmptyRootHash {
		s.db.prefetcher.prefetch(s.addrHash, s.data.Root, s.address, slotsToPrefetch)
	}
	if len(s.dirtyStorage) > 0 {
		s.dirtyStorage = make(Storage)
	}
}

// updateTrie writes cached storage modifications into the object's storage trie.
// It will return nil if the trie has not been loaded and no changes have been
// made. An error will be returned if the trie can't be loaded/updated correctly.
func (s *stateObject) updateTrie() (Trie, error) {
	// Make sure all dirty slots are finalized into the pending storage area
	s.finalise(false) // Don't prefetch anymore, pull directly if need be
	if !s.needUpdateTrie() {
		return s.trie, nil
	}
	// Track the amount of time wasted on updating the storage trie
	if metrics.EnabledExpensive {
		defer func(start time.Time) { s.db.StorageUpdates += time.Since(start) }(time.Now())
	}
	// The snapshot storage map for the object
	var (
		storage map[common.Hash][]byte
		origin  map[common.Hash][]byte
		hasher  = s.db.hasher
		tr      Trie
		err     error
	)
	if s.db.EnableExpire() {
		tr, err = s.getPendingReviveTrie()
	} else {
		tr, err = s.getTrie()
	}
	if err != nil {
		s.db.setError(err)
		return nil, err
	}
	// Insert all the pending updates into the trie
	usedStorage := make([][]byte, 0, len(s.pendingStorage))
	accessStorage := make(map[common.Hash]struct{}, len(s.pendingAccessedState))
	if s.db.EnableExpire() {
		for k := range s.pendingAccessedState {
			accessStorage[k] = struct{}{}
		}
	}
	for key, value := range s.pendingStorage {
		// Skip noop changes, persist actual changes
		if value == s.originStorage[key] {
			continue
		}
		prev := s.originStorage[key]
		s.originStorage[key] = value
		if s.db.EnableExpire() {
			delete(accessStorage, key)
		}
		// rlp-encoded value to be used by the snapshot
		var snapshotVal []byte
		if (value == common.Hash{}) {
			if err := tr.DeleteStorage(s.address, key[:]); err != nil {
				s.db.setError(err)
				return nil, err
			}
			s.db.StorageDeleted += 1
		} else {
			trimmedVal := common.TrimLeftZeroes(value[:])
			// Encoding []byte cannot fail, ok to ignore the error.
			if s.db.EnableExpire() {
				snapshotVal, _ = snapshot.EncodeValueToRLPBytes(snapshot.NewValueWithEpoch(s.db.epoch, trimmedVal))
			} else {
				snapshotVal, _ = rlp.EncodeToBytes(trimmedVal)
			}
			if err := tr.UpdateStorage(s.address, key[:], trimmedVal); err != nil {
				s.db.setError(err)
				return nil, err
			}
			s.db.StorageUpdated += 1
		}
		// Cache the mutated storage slots until commit
		if storage == nil {
			if storage = s.db.storages[s.addrHash]; storage == nil {
				storage = make(map[common.Hash][]byte)
				s.db.storages[s.addrHash] = storage
			}
		}
		khash := crypto.HashData(hasher, key[:])
		storage[khash] = snapshotVal // snapshotVal will be nil if it's deleted

		// Cache the original value of mutated storage slots
		if origin == nil {
			if origin = s.db.storagesOrigin[s.address]; origin == nil {
				origin = make(map[common.Hash][]byte)
				s.db.storagesOrigin[s.address] = origin
			}
		}
		// Track the original value of slot only if it's mutated first time
		if _, ok := origin[khash]; !ok {
			if prev == (common.Hash{}) {
				origin[khash] = nil // nil if it was not present previously
			} else {
				// Encoding []byte cannot fail, ok to ignore the error.
				b, _ := rlp.EncodeToBytes(common.TrimLeftZeroes(prev[:]))
				origin[khash] = b
			}
		}
		// Cache the items for preloading
		usedStorage = append(usedStorage, common.CopyBytes(key[:])) // Copy needed for closure
	}

	// update accessed slots epoch
	if s.db.EnableExpire() {
		for key, _ := range accessStorage {
			// it must hit in cache
			value := s.GetState(key)
			trimmedVal := common.TrimLeftZeroes(value[:])
			snapshotVal, _ := snapshot.EncodeValueToRLPBytes(snapshot.NewValueWithEpoch(s.db.epoch, trimmedVal))
			if err := tr.UpdateStorage(s.address, key[:], trimmedVal); err != nil {
				s.db.setError(err)
				return nil, err
			}
			s.db.StorageUpdated += 1
			// Cache the mutated storage slots until commit
			if storage == nil {
				if storage = s.db.storages[s.addrHash]; storage == nil {
					storage = make(map[common.Hash][]byte)
					s.db.storages[s.addrHash] = storage
				}
			}
			khash := crypto.HashData(hasher, key[:])
			storage[khash] = snapshotVal                                // snapshotVal will be nil if it's deleted
			usedStorage = append(usedStorage, common.CopyBytes(key[:])) // Copy needed for closure
		}
	}

	if s.db.prefetcher != nil {
		s.db.prefetcher.used(s.addrHash, s.data.Root, usedStorage)
	}
	if len(s.pendingStorage) > 0 {
		s.pendingStorage = make(Storage)
	}
	if s.db.EnableExpire() {
		if len(s.pendingReviveState) > 0 {
			s.pendingReviveState = make(map[string]common.Hash)
		}
		if len(s.pendingAccessedState) > 0 {
			s.pendingAccessedState = make(map[common.Hash]int)
		}
		if len(s.originStorageEpoch) > 0 {
			s.originStorageEpoch = make(map[common.Hash]types.StateEpoch)
		}
		if s.pendingReviveTrie != nil {
			s.pendingReviveTrie = nil
		}
		// reset trie as pending trie, will commit later
		if tr != nil {
			s.trie = s.db.db.CopyTrie(tr)
		}
	}
	return tr, nil
}

func (s *stateObject) needUpdateTrie() bool {
	if !s.db.EnableExpire() {
		return len(s.pendingStorage) > 0
	}

	return len(s.pendingStorage) > 0 || len(s.pendingReviveState) > 0 ||
		len(s.pendingAccessedState) > 0
}

// UpdateRoot sets the trie root to the current root hash of. An error
// will be returned if trie root hash is not computed correctly.
func (s *stateObject) updateRoot() {
	tr, err := s.updateTrie()
	if err != nil {
		return
	}
	// If nothing changed, don't bother with hashing anything
	if tr == nil {
		return
	}
	// Track the amount of time wasted on hashing the storage trie
	if metrics.EnabledExpensive {
		defer func(start time.Time) { s.db.StorageHashes += time.Since(start) }(time.Now())
	}
	s.data.Root = tr.Hash()
}

// commit returns the changes made in storage trie and updates the account data.
func (s *stateObject) commit() (*trienode.NodeSet, error) {
	tr, err := s.updateTrie()
	if err != nil {
		return nil, err
	}
	// If nothing changed, don't bother with committing anything
	if tr == nil {
		s.origin = s.data.Copy()
		return nil, nil
	}
	// Track the amount of time wasted on committing the storage trie
	if metrics.EnabledExpensive {
		defer func(start time.Time) { s.db.StorageCommits += time.Since(start) }(time.Now())
	}
	root, nodes, err := tr.Commit(false)
	if err != nil {
		return nil, err
	}
	s.data.Root = root

	// Update original account data after commit
	s.origin = s.data.Copy()
	return nodes, nil
}

// AddBalance adds amount to s's balance.
// It is used to add funds to the destination account of a transfer.
func (s *stateObject) AddBalance(amount *big.Int) {
	// EIP161: We must check emptiness for the objects such that the account
	// clearing (0,0,0 objects) can take effect.
	if amount.Sign() == 0 {
		if s.empty() {
			s.touch()
		}
		return
	}
	s.SetBalance(new(big.Int).Add(s.Balance(), amount))
}

// SubBalance removes amount from s's balance.
// It is used to remove funds from the origin account of a transfer.
func (s *stateObject) SubBalance(amount *big.Int) {
	if amount.Sign() == 0 {
		return
	}
	s.SetBalance(new(big.Int).Sub(s.Balance(), amount))
}

func (s *stateObject) SetBalance(amount *big.Int) {
	s.db.journal.append(balanceChange{
		account: &s.address,
		prev:    new(big.Int).Set(s.data.Balance),
	})
	s.setBalance(amount)
}

func (s *stateObject) setBalance(amount *big.Int) {
	s.data.Balance = amount
}

func (s *stateObject) deepCopy(db *StateDB) *stateObject {
	obj := &stateObject{
		db:       db,
		address:  s.address,
		addrHash: s.addrHash,
		origin:   s.origin,
		data:     s.data,
	}
	if s.trie != nil {
		obj.trie = db.db.CopyTrie(s.trie)
	}
	obj.code = s.code
	obj.dirtyStorage = s.dirtyStorage.Copy()
	obj.originStorage = s.originStorage.Copy()
	obj.pendingStorage = s.pendingStorage.Copy()
	obj.selfDestructed = s.selfDestructed
	obj.dirtyCode = s.dirtyCode
	obj.deleted = s.deleted

	if s.db.EnableExpire() {
		if s.pendingReviveTrie != nil {
			obj.pendingReviveTrie = db.db.CopyTrie(s.pendingReviveTrie)
		}
		obj.pendingReviveState = make(map[string]common.Hash, len(s.pendingReviveState))
		for k, v := range s.pendingReviveState {
			obj.pendingReviveState[k] = v
		}
		obj.pendingAccessedState = make(map[common.Hash]int, len(s.pendingAccessedState))
		for k, v := range s.pendingAccessedState {
			obj.pendingAccessedState[k] = v
		}
		obj.originStorageEpoch = make(map[common.Hash]types.StateEpoch, len(s.originStorageEpoch))
		for k, v := range s.originStorageEpoch {
			obj.originStorageEpoch[k] = v
		}
	}
	return obj
}

//
// Attribute accessors
//

// Address returns the address of the contract/account
func (s *stateObject) Address() common.Address {
	return s.address
}

// Code returns the contract code associated with this object, if any.
func (s *stateObject) Code() []byte {
	if s.code != nil {
		return s.code
	}
	if bytes.Equal(s.CodeHash(), types.EmptyCodeHash.Bytes()) {
		return nil
	}
	code, err := s.db.db.ContractCode(s.address, common.BytesToHash(s.CodeHash()))
	if err != nil {
		s.db.setError(fmt.Errorf("can't load code hash %x: %v", s.CodeHash(), err))
	}
	s.code = code
	return code
}

// CodeSize returns the size of the contract code associated with this object,
// or zero if none. This method is an almost mirror of Code, but uses a cache
// inside the database to avoid loading codes seen recently.
func (s *stateObject) CodeSize() int {
	if s.code != nil {
		return len(s.code)
	}
	if bytes.Equal(s.CodeHash(), types.EmptyCodeHash.Bytes()) {
		return 0
	}
	size, err := s.db.db.ContractCodeSize(s.address, common.BytesToHash(s.CodeHash()))
	if err != nil {
		s.db.setError(fmt.Errorf("can't load code size %x: %v", s.CodeHash(), err))
	}
	return size
}

func (s *stateObject) SetCode(codeHash common.Hash, code []byte) {
	prevcode := s.Code()
	s.db.journal.append(codeChange{
		account:  &s.address,
		prevhash: s.CodeHash(),
		prevcode: prevcode,
	})
	s.setCode(codeHash, code)
}

func (s *stateObject) setCode(codeHash common.Hash, code []byte) {
	s.code = code
	s.data.CodeHash = codeHash[:]
	s.dirtyCode = true
}

func (s *stateObject) SetNonce(nonce uint64) {
	s.db.journal.append(nonceChange{
		account: &s.address,
		prev:    s.data.Nonce,
	})
	s.setNonce(nonce)
}

func (s *stateObject) setNonce(nonce uint64) {
	s.data.Nonce = nonce
}

func (s *stateObject) CodeHash() []byte {
	return s.data.CodeHash
}

func (s *stateObject) Balance() *big.Int {
	return s.data.Balance
}

func (s *stateObject) Nonce() uint64 {
	return s.data.Nonce
}

func (s *stateObject) ReviveStorageTrie(proof types.ReviveStorageProof) error {
	dr, err := s.getPendingReviveTrie()
	if err != nil {
		return err
	}

	key := common.Hex2Bytes(proof.Key)
	prefixKey := common.Hex2Bytes(proof.PrefixKey)
	proofList := make([][]byte, 0, len(proof.Proof))

	for _, p := range proof.Proof {
		proofList = append(proofList, common.Hex2Bytes(p))
	}

	// TODO(asyukii): support proofs merge, revive in nubs
	err = dr.ReviveTrie(crypto.Keccak256(key), prefixKey, proofList)
	if err != nil {
		return fmt.Errorf("revive storage trie failed, err: %v", err)
	}

	// Update pending revive state
	val, err := dr.GetStorage(s.address, key) // TODO(asyukii): may optimize this, return value when revive trie
	if err != nil {
		return fmt.Errorf("get storage value failed, err: %v", err)
	}

	s.pendingReviveState[proof.Key] = common.BytesToHash(val)
	return nil
}

// accessState record all access states, now in pendingAccessedStateEpoch without consensus
func (s *stateObject) accessState(key common.Hash) {
	if !s.db.EnableExpire() {
		return
	}

	if s.db.epoch > s.originStorageEpoch[key] {
		count := s.pendingAccessedState[key]
		s.pendingAccessedState[key] = count + 1
	}
}

// TODO(0xbundler): add hash key cache later
func (s *stateObject) queryFromReviveState(reviveState map[string]common.Hash, key common.Hash) (common.Hash, bool) {
	khash := crypto.HashData(s.db.hasher, key[:])
	val, ok := reviveState[string(khash[:])]
	return val, ok
}
