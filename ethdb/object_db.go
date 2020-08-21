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

// Package ethdb defines the interfaces for an Ethereum data store.
package ethdb

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/common/debug"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/ledgerwatch/turbo-geth/metrics"
)

var (
	dbGetTimer = metrics.NewRegisteredTimer("db/get", nil)
	dbPutTimer = metrics.NewRegisteredTimer("db/put", nil)
)

// ObjectDatabase - is an object-style interface of DB accessing
type ObjectDatabase struct {
	kv  KV
	log log.Logger
	id  uint64
}

// NewObjectDatabase returns a AbstractDB wrapper.
func NewObjectDatabase(kv KV) *ObjectDatabase {
	logger := log.New("database", "object")
	return &ObjectDatabase{
		kv:  kv,
		log: logger,
		id:  id(),
	}
}

func MustOpen(path string) *ObjectDatabase {
	db, err := Open(path)
	if err != nil {
		panic(err)
	}
	return db
}

// Open - main method to open database. Choosing driver based on path suffix.
// If env TEST_DB provided - choose driver based on it. Some test using this method to open non-in-memory db
func Open(path string) (*ObjectDatabase, error) {
	var kv KV
	var err error
	testDB := debug.TestDB()
	switch true {
	case testDB == "lmdb" || strings.HasSuffix(path, "_lmdb"):
		kv, err = NewLMDB().Path(path).Open()
	case testDB == "bolt" || strings.HasSuffix(path, "_bolt"):
		kv, err = NewBolt().Path(path).Open()
	default:
		kv, err = NewLMDB().Path(path).Open()
	}
	if err != nil {
		return nil, err
	}
	return NewObjectDatabase(kv), nil
}

// Put inserts or updates a single entry.
func (db *ObjectDatabase) Put(bucket string, key []byte, value []byte) error {
	if metrics.Enabled {
		defer dbPutTimer.UpdateSince(time.Now())
	}

	err := db.kv.Update(context.Background(), func(tx Tx) error {
		return tx.Cursor(bucket).Put(key, value)
	})
	return err
}

// MultiPut - requirements: input must be sorted and without duplicates
func (db *ObjectDatabase) MultiPut(tuples ...[]byte) (uint64, error) {
	err := db.kv.Update(context.Background(), func(tx Tx) error {
		return MultiPut(tx, tuples...)
	})
	if err != nil {
		return 0, err
	}
	return 0, nil
}

func (db *ObjectDatabase) Has(bucket string, key []byte) (bool, error) {
	var has bool
	err := db.kv.View(context.Background(), func(tx Tx) error {
		v, err := tx.Get(bucket, key)
		if err != nil {
			return err
		}
		has = v != nil
		return nil
	})
	return has, err
}

func (db *ObjectDatabase) DiskSize(ctx context.Context) (uint64, error) {
	casted, ok := db.kv.(HasStats)
	if !ok {
		return 0, nil
	}
	return casted.DiskSize(ctx)
}

// Get returns the value for a given key if it's present.
func (db *ObjectDatabase) Get(bucket string, key []byte) ([]byte, error) {
	if metrics.EnabledExpensive {
		defer dbGetTimer.UpdateSince(time.Now())
	}

	var dat []byte
	if err := db.kv.View(context.Background(), func(tx Tx) error {
		v, err := tx.Get(bucket, key)
		if err != nil {
			return err
		}
		if v != nil {
			dat = make([]byte, len(v))
			copy(dat, v)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if dat == nil {
		return nil, ErrKeyNotFound
	}
	return dat, nil
}

func (db *ObjectDatabase) Last(bucket string) ([]byte, []byte, error) {
	var key, value []byte
	if err := db.kv.View(context.Background(), func(tx Tx) error {
		k, v, err := tx.Cursor(bucket).Last()
		if err != nil {
			return err
		}
		if k != nil {
			key, value = common.CopyBytes(k), common.CopyBytes(v)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return key, value, nil
}

// GetIndexChunk returns proper index chunk or return error if index is not created.
// key must contain inverted block number in the end
func (db *ObjectDatabase) GetIndexChunk(bucket string, key []byte, timestamp uint64) ([]byte, error) {
	var dat []byte
	err := db.kv.View(context.Background(), func(tx Tx) error {
		c := tx.Cursor(bucket)
		k, v, err := c.Seek(dbutils.IndexChunkKey(key, timestamp))
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(k, dbutils.CompositeKeyWithoutIncarnation(key)) {
			return ErrKeyNotFound
		}
		dat = make([]byte, len(v))
		copy(dat, v)
		return nil
	})
	if dat == nil {
		return nil, ErrKeyNotFound
	}
	return dat, err
}

// getChangeSetByBlockNoLock returns changeset by block and dbi
func (db *ObjectDatabase) GetChangeSetByBlock(storage bool, timestamp uint64) ([]byte, error) {
	key := dbutils.EncodeTimestamp(timestamp)

	var dat []byte
	err := db.kv.View(context.Background(), func(tx Tx) error {
		v, err := tx.Get(dbutils.ChangeSetByIndexBucket(true /* plain */, storage), key)
		if err != nil {
			return err
		}
		if v != nil {
			dat = make([]byte, len(v))
			copy(dat, v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dat, nil
}

func (db *ObjectDatabase) Walk(bucket string, startkey []byte, fixedbits int, walker func(k, v []byte) (bool, error)) error {
	err := db.kv.View(context.Background(), func(tx Tx) error {
		return Walk(tx.Cursor(bucket), startkey, fixedbits, walker)
	})
	return err
}

func (db *ObjectDatabase) MultiWalk(bucket string, startkeys [][]byte, fixedbits []int, walker func(int, []byte, []byte) error) error {
	return db.kv.View(context.Background(), func(tx Tx) error {
		return MultiWalk(tx.Cursor(bucket), startkeys, fixedbits, walker)
	})
}

// Delete deletes the key from the queue and database
func (db *ObjectDatabase) Delete(bucket string, key []byte) error {
	// Execute the actual operation
	err := db.kv.Update(context.Background(), func(tx Tx) error {
		return tx.Cursor(bucket).Delete(key)
	})
	return err
}

func (db *ObjectDatabase) BucketExists(name string) (bool, error) {
	exists := false
	if err := db.kv.View(context.Background(), func(tx Tx) error {
		migrator, ok := tx.(BucketMigrator)
		if !ok {
			return fmt.Errorf("%T doesn't implement ethdb.TxMigrator interface", db.kv)
		}
		exists = migrator.ExistsBucket(name)
		return nil
	}); err != nil {
		return false, err
	}
	return exists, nil
}

func (db *ObjectDatabase) ClearBuckets(buckets ...string) error {
	for i := range buckets {
		name := buckets[i]
		if err := db.kv.Update(context.Background(), func(tx Tx) error {
			migrator, ok := tx.(BucketMigrator)
			if !ok {
				return fmt.Errorf("%T doesn't implement ethdb.TxMigrator interface", db.kv)
			}
			if err := migrator.ClearBucket(name); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

func (db *ObjectDatabase) DropBuckets(buckets ...string) error {
	for i := range buckets {
		name := buckets[i]
		log.Info("Dropping bucket", "name", name)
		if err := db.kv.Update(context.Background(), func(tx Tx) error {
			migrator, ok := tx.(BucketMigrator)
			if !ok {
				return fmt.Errorf("%T doesn't implement ethdb.TxMigrator interface", db.kv)
			}
			if err := migrator.DropBucket(name); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (db *ObjectDatabase) Close() {
	db.kv.Close()
}

func (db *ObjectDatabase) Keys() ([][]byte, error) {
	var keys [][]byte
	err := db.kv.View(context.Background(), func(tx Tx) error {
		for _, name := range dbutils.Buckets {
			var nameCopy = make([]byte, len(name))
			copy(nameCopy, name)
			return tx.Cursor(name).Walk(func(k, _ []byte) (bool, error) {
				var kCopy = make([]byte, len(k))
				copy(kCopy, k)
				keys = append(append(keys, nameCopy), kCopy)
				return true, nil
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, err
}

func (db *ObjectDatabase) KV() KV {
	return db.kv
}

func (db *ObjectDatabase) MemCopy() *ObjectDatabase {
	var mem *ObjectDatabase
	// Open the db and recover any potential corruptions
	switch db.kv.(type) {
	case *LmdbKV:
		mem = NewObjectDatabase(NewLMDB().InMem().MustOpen())
	case *BoltKV:
		mem = NewObjectDatabase(NewBolt().InMem().MustOpen())
	}

	if err := db.kv.View(context.Background(), func(readTx Tx) error {
		for _, name := range dbutils.Buckets {
			name := name
			if err := mem.kv.Update(context.Background(), func(writeTx Tx) error {
				newBucketToWrite := writeTx.Cursor(name)
				return readTx.Cursor(name).Walk(func(k, v []byte) (bool, error) {
					if err := newBucketToWrite.Put(common.CopyBytes(k), common.CopyBytes(v)); err != nil {
						return false, err
					}
					return true, nil
				})
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		panic(err)
	}

	return mem
}

func (db *ObjectDatabase) NewBatch() DbWithPendingMutations {
	m := &mutation{
		db:   db,
		puts: newPuts(),
	}
	return m
}

func (db *ObjectDatabase) Begin() (DbWithPendingMutations, error) {
	batch := &TxDb{db: db, cursors: map[string]*LmdbCursor{}}
	if err := batch.begin(nil); err != nil {
		panic(err)
	}
	return batch, nil
}

// IdealBatchSize defines the size of the data batches should ideally add in one write.
func (db *ObjectDatabase) IdealBatchSize() int {
	return db.kv.IdealBatchSize()
}

// [TURBO-GETH] Freezer support (not implemented yet)
// Ancients returns an error as we don't have a backing chain freezer.
func (db *ObjectDatabase) Ancients() (uint64, error) {
	return 0, errNotSupported
}

// TruncateAncients returns an error as we don't have a backing chain freezer.
func (db *ObjectDatabase) TruncateAncients(items uint64) error {
	return errNotSupported
}