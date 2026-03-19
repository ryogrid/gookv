package server

import (
	"errors"

	"github.com/ryogrid/gookv/internal/engine/traits"
	"github.com/ryogrid/gookv/internal/storage/mvcc"
	"github.com/ryogrid/gookv/pkg/cfnames"
)

// KvPair holds a key-value pair for raw KV operations.
type KvPair struct {
	Key   []byte
	Value []byte
}

// RawStorage provides non-transactional key-value operations
// directly against the storage engine, bypassing MVCC.
type RawStorage struct {
	engine traits.KvEngine
}

// NewRawStorage creates a new RawStorage backed by the given engine.
func NewRawStorage(engine traits.KvEngine) *RawStorage {
	return &RawStorage{engine: engine}
}

func (rs *RawStorage) resolveCF(cf string) string {
	if cf == "" {
		return cfnames.CFDefault
	}
	return cf
}

// Get retrieves a value by key. Returns (nil, nil) if not found.
func (rs *RawStorage) Get(cf string, key []byte) ([]byte, error) {
	cf = rs.resolveCF(cf)
	value, err := rs.engine.Get(cf, key)
	if err != nil {
		if errors.Is(err, traits.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return value, nil
}

// Put stores a key-value pair.
func (rs *RawStorage) Put(cf string, key, value []byte) error {
	cf = rs.resolveCF(cf)
	return rs.engine.Put(cf, key, value)
}

// Delete removes a key.
func (rs *RawStorage) Delete(cf string, key []byte) error {
	cf = rs.resolveCF(cf)
	return rs.engine.Delete(cf, key)
}

// BatchGet reads multiple keys from a consistent snapshot.
func (rs *RawStorage) BatchGet(cf string, keys [][]byte) ([]KvPair, error) {
	cf = rs.resolveCF(cf)
	snap := rs.engine.NewSnapshot()
	defer snap.Close()

	pairs := make([]KvPair, 0, len(keys))
	for _, key := range keys {
		value, err := snap.Get(cf, key)
		if err != nil {
			if errors.Is(err, traits.ErrNotFound) {
				continue
			}
			return nil, err
		}
		pairs = append(pairs, KvPair{Key: key, Value: value})
	}
	return pairs, nil
}

// BatchPut atomically writes multiple key-value pairs.
func (rs *RawStorage) BatchPut(cf string, pairs []KvPair) error {
	cf = rs.resolveCF(cf)
	wb := rs.engine.NewWriteBatch()
	for _, p := range pairs {
		if err := wb.Put(cf, p.Key, p.Value); err != nil {
			return err
		}
	}
	return wb.Commit()
}

// BatchDelete atomically deletes multiple keys.
func (rs *RawStorage) BatchDelete(cf string, keys [][]byte) error {
	cf = rs.resolveCF(cf)
	wb := rs.engine.NewWriteBatch()
	for _, key := range keys {
		if err := wb.Delete(cf, key); err != nil {
			return err
		}
	}
	return wb.Commit()
}

// DeleteRange removes all keys in [startKey, endKey).
func (rs *RawStorage) DeleteRange(cf string, startKey, endKey []byte) error {
	cf = rs.resolveCF(cf)
	return rs.engine.DeleteRange(cf, startKey, endKey)
}

// Scan iterates over keys in [startKey, endKey) with a limit.
func (rs *RawStorage) Scan(cf string, startKey, endKey []byte, limit uint32, keyOnly bool, reverse bool) ([]KvPair, error) {
	cf = rs.resolveCF(cf)
	snap := rs.engine.NewSnapshot()
	defer snap.Close()

	opts := traits.IterOptions{}
	if !reverse {
		opts.LowerBound = startKey
		if len(endKey) > 0 {
			opts.UpperBound = endKey
		}
	} else {
		if len(endKey) > 0 {
			opts.LowerBound = endKey
		}
		opts.UpperBound = startKey
	}

	iter := snap.NewIterator(cf, opts)
	defer iter.Close()

	if reverse {
		iter.SeekToLast()
	} else {
		iter.SeekToFirst()
	}

	var pairs []KvPair
	for iter.Valid() && (limit == 0 || uint32(len(pairs)) < limit) {
		key := append([]byte(nil), iter.Key()...)
		var value []byte
		if !keyOnly {
			value = append([]byte(nil), iter.Value()...)
		}
		pairs = append(pairs, KvPair{Key: key, Value: value})

		if reverse {
			iter.Prev()
		} else {
			iter.Next()
		}
	}

	if err := iter.Error(); err != nil {
		return nil, err
	}
	return pairs, nil
}

// PutModify returns a Modify for use with Raft proposal (cluster mode).
func (rs *RawStorage) PutModify(cf string, key, value []byte) mvcc.Modify {
	cf = rs.resolveCF(cf)
	return mvcc.Modify{
		Type:  mvcc.ModifyTypePut,
		CF:    cf,
		Key:   key,
		Value: value,
	}
}

// DeleteModify returns a Modify for use with Raft proposal (cluster mode).
func (rs *RawStorage) DeleteModify(cf string, key []byte) mvcc.Modify {
	cf = rs.resolveCF(cf)
	return mvcc.Modify{
		Type: mvcc.ModifyTypeDelete,
		CF:   cf,
		Key:  key,
	}
}
