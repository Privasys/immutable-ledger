// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"context"
	"fmt"
	"testing"

	ledger "github.com/Privasys/immutable-ledger"
	pebbleback "github.com/Privasys/immutable-ledger/backend/pebble"
)

// The full stack on disk: SQL over the ledger over Pebble, across a
// process "restart", with pruning running underneath.
func TestSQLOverPebbleWithRestartAndPruning(t *testing.T) {
	dir := t.TempDir()
	var root ledger.Hash

	{
		b, err := pebbleback.Open(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		led, err := ledger.Create(b, testCK)
		if err != nil {
			t.Fatal(err)
		}
		store, err := Open(led, b, "app")
		if err != nil {
			t.Fatal(err)
		}
		eng := NewEngine(store)
		ctx := eng.NewContext(context.Background())
		mustExec := func(q string) {
			if _, err := eng.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		mustExec(`CREATE TABLE events (
			id BIGINT UNSIGNED PRIMARY KEY AUTO_INCREMENT,
			kind VARCHAR(30) NOT NULL,
			at DATETIME
		)`)
		mustExec(`CREATE INDEX idx_kind ON events (kind)`)
		for i := 0; i < 300; i++ {
			mustExec(fmt.Sprintf(
				`INSERT INTO events (kind, at) VALUES ('k%d', '2026-08-26 12:00:00')`, i%3))
		}
		mustExec(`DELETE FROM events WHERE id <= 30`)

		// Prune retention under live SQL state: the current version
		// must stay fully served.
		if _, err := store.Ledger().RetainRecent(2); err != nil {
			t.Fatalf("prune: %v", err)
		}
		rows, err := eng.Exec(ctx, `SELECT COUNT(*) FROM events WHERE kind = 'k1'`)
		if err != nil || fmt.Sprint(rows[0][0]) != "90" {
			t.Fatalf("post-prune count = %v (%v)", rows, err)
		}
		root, _ = led.Root()
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Restart from disk.
	b, err := pebbleback.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	led, err := ledger.OpenLatest(b, testCK)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := led.Root(); r != root {
		t.Fatalf("restart root %x, want %x", r, root)
	}
	store, err := Open(led, b, "app")
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(store)
	ctx := eng.NewContext(context.Background())

	rows, err := eng.Exec(ctx, `SELECT COUNT(*), MIN(id), MAX(id) FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rows[0]) != "[270 31 300]" {
		t.Fatalf("after restart = %v", rows[0])
	}
	// AUTO_INCREMENT resumes past the persisted counter.
	if _, err := eng.Exec(ctx, `INSERT INTO events (kind) VALUES ('k9')`); err != nil {
		t.Fatal(err)
	}
	rows, _ = eng.Exec(ctx, `SELECT MAX(id) FROM events`)
	if fmt.Sprint(rows[0][0]) != "301" {
		t.Fatalf("auto-inc after restart = %v", rows[0][0])
	}
	// Verified read over the disk-backed stack.
	vr, err := store.VerifiedGet("events", uint64(301))
	if err != nil || vr.Row == nil {
		t.Fatalf("verified get: %v %v", vr, err)
	}
	if ok, err := store.Verify(vr, &vr.Root); err != nil || !ok {
		t.Fatalf("verify: %v %v", ok, err)
	}
}
