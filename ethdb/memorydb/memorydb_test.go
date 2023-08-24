// Copyright 2018 The go-ethereum Authors
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

package memorydb

import (
	"github.com/stretchr/testify/assert"
	"testing"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/dbtest"
)

func TestMemoryDB(t *testing.T) {
	t.Run("DatabaseSuite", func(t *testing.T) {
		dbtest.TestDatabaseSuite(t, func() ethdb.KeyValueStore {
			return New()
		})
	})
}

func TestMemoryDBSeeker(t *testing.T) {
	db := New()

	db.Put([]byte("key1111"), []byte("val1"))
	db.Put([]byte("key1112"), []byte("val2"))
	db.Put([]byte("key1121"), []byte("val3"))
	db.Put([]byte("key1211"), []byte("val4"))
	db.Put([]byte("key122"), []byte("val5"))
	db.Put([]byte("key122F"), []byte("val6"))

	// seek from large scope
	iter := db.NewIterator([]byte("key1"), nil)
	seeker := iter.(ethdb.IteratorSeeker)
	assert.Equal(t, true, seeker.First())
	assert.Equal(t, []byte("key1111"), seeker.Key())
	assert.Equal(t, []byte("val1"), seeker.Value())
	assert.Equal(t, true, seeker.Last())
	assert.Equal(t, []byte("key122F"), seeker.Key())
	assert.Equal(t, []byte("val6"), seeker.Value())
	assert.Equal(t, true, seeker.Seek([]byte("key1221")))
	assert.Equal(t, []byte("key122F"), seeker.Key())
	assert.Equal(t, []byte("val6"), seeker.Value())
	assert.Equal(t, true, seeker.Prev())
	assert.Equal(t, []byte("key122"), seeker.Key())
	assert.Equal(t, []byte("val5"), seeker.Value())

	// seek from target
	iter = db.NewIterator([]byte("key1"), []byte("221"))
	seeker = iter.(ethdb.IteratorSeeker)
	assert.Equal(t, true, seeker.Next())
	assert.Equal(t, []byte("key122F"), seeker.Key())
	assert.Equal(t, []byte("val6"), seeker.Value())
	assert.Equal(t, true, seeker.Last())
	assert.Equal(t, []byte("key122F"), seeker.Key())
	assert.Equal(t, []byte("val6"), seeker.Value())
}
