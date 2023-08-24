package snapshot

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

// ShrinkExpiredSubTree tool function for snapshot prune and rebuild shrink kv
func ShrinkExpiredSubTree(db ethdb.KeyValueStore, accountHash common.Hash, path []byte, epoch types.StateEpoch, nodeHash common.Hash) error {
	batch := db.NewBatch()
	shrinkNode := NewShrinkNodeWithEpoch(epoch, nodeHash)
	enc, err := EncodeValueToRLPBytes(shrinkNode)
	if err != nil {
		return err
	}
	rawdb.WriteStorageSnapshot(batch, accountHash, common.BytesToHashLeft(path), enc)

	seeker := rawdb.StorageSnapshotSeeker(db, accountHash, path)
	defer seeker.Release()
	// contains historic shrink kvs or flatten kvs
	for seeker.Next() {
		if err = batch.Delete(seeker.Key()); err != nil {
			return err
		}
	}
	return batch.Write()
}

// ShrinkExpiredLeaf tool function for snapshot kv prune
func ShrinkExpiredLeaf(db ethdb.KeyValueStore, accountHash common.Hash, storageHash common.Hash, epoch types.StateEpoch) error {
	valWithEpoch := NewValueWithEpoch(epoch, common.Hash{})
	enc, err := EncodeValueToRLPBytes(valWithEpoch)
	if err != nil {
		return err
	}
	rawdb.WriteStorageSnapshot(db, accountHash, storageHash, enc)
	return nil
}
