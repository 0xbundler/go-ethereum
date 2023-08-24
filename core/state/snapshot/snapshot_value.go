package snapshot

import (
	"bytes"
	"errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	RawValueType            = iota // simple value, cannot exceed 32 bytes
	ValueWithEpochType             // value add epoch meta
	ShrinkNodeWithEpochType        // the expired shrink KV with epoch meta
)

var (
	ErrSnapValueNotSupport = errors.New("the snapshot type not support now")
)

type SnapValue interface {
	GetType() byte
	GetEpoch() types.StateEpoch
	GetVal() common.Hash // may cannot provide val in some value types
}

type RawValue []byte

func NewRawValue(val []byte) SnapValue {
	value := RawValue(val)
	return &value
}

func (v *RawValue) GetType() byte {
	return RawValueType
}

func (v *RawValue) GetEpoch() types.StateEpoch {
	return types.StateEpoch0
}

func (v *RawValue) GetVal() common.Hash {
	return common.BytesToHash(*v)
}

type ValueWithEpoch struct {
	Epoch types.StateEpoch // kv's epoch meta
	Val   common.Hash      // TODO(0xbundler): if val is empty hash, just save as empty string
}

func NewValueWithEpoch(epoch types.StateEpoch, val common.Hash) SnapValue {
	return &ValueWithEpoch{
		Epoch: epoch,
		Val:   val,
	}
}

func (v *ValueWithEpoch) GetType() byte {
	return ValueWithEpochType
}

func (v *ValueWithEpoch) GetEpoch() types.StateEpoch {
	return v.Epoch
}

func (v *ValueWithEpoch) GetVal() common.Hash {
	return v.Val
}

type ShrinkNodeWithEpoch struct {
	Epoch types.StateEpoch // if it's a shrink key, just save its epoch
	Hash  common.Hash      // the hash of state trie node
}

func NewShrinkNodeWithEpoch(epoch types.StateEpoch, hash common.Hash) SnapValue {
	return &ShrinkNodeWithEpoch{
		Epoch: epoch,
		Hash:  hash,
	}
}

func (v *ShrinkNodeWithEpoch) GetType() byte {
	return ShrinkNodeWithEpochType
}

func (v *ShrinkNodeWithEpoch) GetEpoch() types.StateEpoch {
	return v.Epoch
}

func (v *ShrinkNodeWithEpoch) GetVal() common.Hash {
	return common.Hash{}
}

func EncodeValueToRLPBytes(val SnapValue) ([]byte, error) {
	switch raw := val.(type) {
	case *RawValue:
		return rlp.EncodeToBytes(raw)
	default:
		return encodeValTyped(val)
	}
}

func DecodeValueFromRLPBytes(b []byte) (SnapValue, error) {
	if len(b) > 0 && b[0] > 0x7f {
		var data RawValue
		_, data, _, err := rlp.Split(b)
		if err != nil {
			return nil, err
		}
		return &data, nil
	}
	return decodeValTyped(b)
}

func GetValueTypeFromRLPBytes(b []byte) byte {
	if len(b) > 0 && b[0] > 0x7f {
		return RawValueType
	}
	return b[0]
}

func decodeValTyped(b []byte) (SnapValue, error) {
	switch b[0] {
	case ValueWithEpochType:
		var data ValueWithEpoch
		if err := rlp.DecodeBytes(b[1:], &data); err != nil {
			return nil, err
		}
		return &data, nil
	case ShrinkNodeWithEpochType:
		var data ShrinkNodeWithEpoch
		if err := rlp.DecodeBytes(b[1:], &data); err != nil {
			return nil, err
		}
		return &data, nil
	default:
		return nil, ErrSnapValueNotSupport
	}
}

func encodeValTyped(val SnapValue) ([]byte, error) {
	buf := bytes.NewBuffer(make([]byte, 0, 40))
	buf.WriteByte(val.GetType())
	if err := rlp.Encode(buf, val); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
