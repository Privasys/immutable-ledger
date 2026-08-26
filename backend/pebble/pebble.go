// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

// Package pebbleback adapts a Pebble database to the ledger's Backend
// interface: point gets, atomic write batches, and ordered range scans.
//
// Pebble (CockroachDB's storage engine) is pure Go, so a container
// image needs no C toolchain or native library. One Pebble database
// can back exactly one ledger; open separate databases (or prefix
// keyspaces at the application layer) for separate stores.
package pebbleback

import (
	"errors"

	"github.com/cockroachdb/pebble/v2"

	ledger "github.com/Privasys/immutable-ledger"
)

// Backend is a ledger.Backend over a Pebble database.
type Backend struct {
	db *pebble.DB
	// sync forces a WAL fsync on every write batch (default true —
	// the ledger's crash-safety story assumes confirmed batches are
	// durable).
	sync bool
}

// Options configures Open.
type Options struct {
	// NoSync disables per-batch WAL fsync. Faster, but a machine crash
	// can lose the latest commits (the store reopens consistently at
	// an earlier checkpoint). Leave false unless the volume's
	// durability is handled elsewhere.
	NoSync bool
	// Pebble exposes further tuning through this field; nil uses
	// Pebble's defaults.
	Pebble *pebble.Options
}

// Open opens (or creates) a Pebble database at dir as a ledger backend.
func Open(dir string, opts *Options) (*Backend, error) {
	var o Options
	if opts != nil {
		o = *opts
	}
	po := o.Pebble
	if po == nil {
		po = &pebble.Options{}
	}
	db, err := pebble.Open(dir, po)
	if err != nil {
		return nil, err
	}
	return &Backend{db: db, sync: !o.NoSync}, nil
}

// Wrap adapts an already-open Pebble database (sync writes on).
func Wrap(db *pebble.DB) *Backend {
	return &Backend{db: db, sync: true}
}

// Close closes the underlying database.
func (b *Backend) Close() error { return b.db.Close() }

// DB exposes the underlying Pebble database (e.g. to share it with an
// index keyspace).
func (b *Backend) DB() *pebble.DB { return b.db }

// Get implements ledger.Backend.
func (b *Backend) Get(key []byte) ([]byte, bool, error) {
	v, closer, err := b.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := append([]byte(nil), v...)
	if err := closer.Close(); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// WriteBatch implements ledger.Backend: all ops land atomically.
func (b *Backend) WriteBatch(ops []ledger.BatchOp) error {
	batch := b.db.NewBatch()
	defer batch.Close()
	for _, op := range ops {
		var err error
		if op.Delete {
			err = batch.Delete(op.Key, nil)
		} else {
			err = batch.Set(op.Key, op.Value, nil)
		}
		if err != nil {
			return err
		}
	}
	if b.sync {
		return batch.Commit(pebble.Sync)
	}
	return batch.Commit(pebble.NoSync)
}

// Scan implements ledger.Backend: ascending over [start, end), at most
// limit entries (0 = unlimited); empty end = unbounded.
func (b *Backend) Scan(start, end []byte, limit uint32) ([]ledger.KV, error) {
	io := &pebble.IterOptions{LowerBound: start}
	if len(end) > 0 {
		io.UpperBound = end
	}
	it, err := b.db.NewIter(io)
	if err != nil {
		return nil, err
	}
	defer it.Close()
	max := int(limit)
	if limit == 0 {
		max = -1
	}
	var out []ledger.KV
	for valid := it.First(); valid; valid = it.Next() {
		v, err := it.ValueAndErr()
		if err != nil {
			return nil, err
		}
		out = append(out, ledger.KV{
			Key:   append([]byte(nil), it.Key()...),
			Value: append([]byte(nil), v...),
		})
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out, it.Error()
}
