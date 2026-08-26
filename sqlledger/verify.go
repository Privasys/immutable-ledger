// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"fmt"
	"strings"

	"github.com/dolthub/go-mysql-server/sql"

	ledger "github.com/Privasys/immutable-ledger"
)

// Verified reads: because every row is one ledger entry, a row can be
// returned together with the ledger's inclusion proof and the
// (root, version) it was read at. The proof verifies offline with the
// pure ledger verifier and the commitment key; combined with a root
// obtained through an attested channel, it demonstrates that this
// exact row is part of this exact authenticated state.

// VerifiedRow is a row with its authenticity evidence.
type VerifiedRow struct {
	// Row holds the decoded column values (schema order), nil when the
	// primary key is absent (Proof is then an absence proof).
	Row sql.Row
	// Key is the row's ledger key: prove/verify against it.
	Key []byte
	// Value is the row's raw ledger value (the proven plaintext).
	Value []byte
	// Proof is the ledger inclusion (or absence) proof for Key.
	Proof *ledger.Proof
	// Root and Version identify the authenticated state the proof
	// holds against.
	Root    ledger.Hash
	Version uint64
}

// VerifiedGet reads one row by primary key with proof. pkValues are
// the primary-key column values in key order, as Go values matching
// the column types.
func (s *Store) VerifiedGet(table string, pkValues ...interface{}) (*VerifiedRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	def, ok := s.tables[strings.ToLower(table)]
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", table)
	}
	if len(pkValues) != len(def.PKOrds) {
		return nil, fmt.Errorf("table %q has %d primary-key columns, got %d values",
			table, len(def.PKOrds), len(pkValues))
	}
	var pkEnc []byte
	for i, ord := range def.PKOrds {
		kind, err := def.Columns[ord].kind()
		if err != nil {
			return nil, err
		}
		pkEnc, err = encodeKeyCol(pkEnc, kind, pkValues[i])
		if err != nil {
			return nil, fmt.Errorf("primary-key column %q: %w", def.Columns[ord].Name, err)
		}
	}
	key := rowKey(def.ID, pkEnc)

	root, version := s.led.Root()
	proof, err := s.led.Prove(key)
	if err != nil {
		return nil, err
	}
	out := &VerifiedRow{Key: key, Proof: proof, Root: root, Version: version}

	raw, ok, err := s.led.Get(key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return out, nil // absence, with its proof
	}
	if len(raw) < 9 || raw[0] != tagRow {
		return nil, fmt.Errorf("table %q: row entry is corrupt", table)
	}
	out.Value = raw
	out.Row, err = decodeRow(def.Columns, raw[9:])
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Verify checks the evidence against a root the caller trusts (e.g.
// one read from an attested channel or an external anchor): the proof
// must hold for Key at that root, and, for a present row, the proven
// commitment must match Value. It uses the store's commitment key.
func (s *Store) Verify(vr *VerifiedRow, root *ledger.Hash) (bool, error) {
	if vr.Value == nil {
		return s.led.VerifyAbsent(root, vr.Key, vr.Proof)
	}
	return s.led.VerifyValue(root, vr.Key, vr.Value, vr.Proof)
}
