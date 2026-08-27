// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dolthub/go-mysql-server/sql"

	ledger "github.com/Privasys/immutable-ledger/ledger"
)

// newTxnHarness sets up a store with one table, ten committed rows and
// a secondary index on grp (unique index on tag).
func newTxnHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.exec(t, `CREATE TABLE t (id BIGINT PRIMARY KEY, grp BIGINT, amount BIGINT, tag VARCHAR(32))`)
	h.exec(t, `CREATE INDEX idx_grp ON t (grp)`)
	h.exec(t, `CREATE UNIQUE INDEX idx_tag ON t (tag)`)
	for i := 0; i < 10; i++ {
		h.exec(t, fmt.Sprintf(`INSERT INTO t VALUES (%d, %d, %d, 'tag-%d')`, i, i%3, i*10, i))
	}
	return h
}

// otherCtx opens a second session on the same engine.
func (h *harness) otherCtx() *sql.Context {
	return h.eng.NewContext(context.Background())
}

func (h *harness) execCtx(t *testing.T, ctx *sql.Context, q string) []sql.Row {
	t.Helper()
	rows, err := h.eng.Exec(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return rows
}

func (h *harness) one(t *testing.T, ctx *sql.Context, q string) string {
	t.Helper()
	rows := h.execCtx(t, ctx, q)
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: expected one value, got %v", q, rows)
	}
	return fmt.Sprint(rows[0][0])
}

func TestTxnCommitVisibilityAndAtomicity(t *testing.T) {
	h := newTxnHarness(t)
	other := h.otherCtx()
	_, before := h.led.Root()

	h.exec(t, `BEGIN`)
	h.exec(t, `INSERT INTO t VALUES (100, 7, 1000, 'tag-100')`)
	h.exec(t, `UPDATE t SET amount = 999 WHERE id = 0`)
	h.exec(t, `DELETE FROM t WHERE id = 9`)

	// The transaction sees its own writes, through every read path.
	if got := h.one(t, h.ctx, `SELECT amount FROM t WHERE id = 100`); got != "1000" {
		t.Fatalf("point read of buffered insert: %s", got)
	}
	if got := h.one(t, h.ctx, `SELECT amount FROM t WHERE id = 0`); got != "999" {
		t.Fatalf("point read of buffered update: %s", got)
	}
	if got := h.one(t, h.ctx, `SELECT COUNT(*) FROM t`); got != "10" {
		t.Fatalf("full scan count in txn: %s", got)
	}
	if got := h.one(t, h.ctx, `SELECT COUNT(*) FROM t WHERE grp = 7`); got != "1" {
		t.Fatalf("index range over buffered insert: %s", got)
	}
	if got := h.one(t, h.ctx, `SELECT MAX(id) FROM t`); got != "100" {
		t.Fatalf("reverse lookup over buffered insert: %s", got)
	}
	if got := h.one(t, h.ctx, `SELECT COUNT(*) FROM t WHERE id = 9`); got != "0" {
		t.Fatalf("buffered delete not masked: %s", got)
	}

	// Another session sees none of it, and the ledger has not moved.
	if got := h.one(t, other, `SELECT COUNT(*) FROM t`); got != "10" {
		t.Fatalf("other session count during txn: %s", got)
	}
	if got := h.one(t, other, `SELECT amount FROM t WHERE id = 0`); got != "0" {
		t.Fatalf("other session sees buffered update: %s", got)
	}
	if _, v := h.led.Root(); v != before {
		t.Fatalf("ledger moved during open transaction: %d != %d", v, before)
	}

	h.exec(t, `COMMIT`)

	// One ledger version for the whole transaction; both sessions agree.
	if _, v := h.led.Root(); v != before+1 {
		t.Fatalf("transaction versions: %d != %d", v, before+1)
	}
	for _, ctx := range []*sql.Context{h.ctx, other} {
		if got := h.one(t, ctx, `SELECT amount FROM t WHERE id = 0`); got != "999" {
			t.Fatalf("committed update: %s", got)
		}
		if got := h.one(t, ctx, `SELECT COUNT(*) FROM t`); got != "10" {
			t.Fatalf("committed count: %s", got)
		}
	}
}

func TestTxnRollback(t *testing.T) {
	h := newTxnHarness(t)
	rootBefore, vBefore := h.led.Root()

	h.exec(t, `BEGIN`)
	h.exec(t, `INSERT INTO t VALUES (100, 7, 1000, 'tag-100')`)
	h.exec(t, `DELETE FROM t WHERE id < 5`)
	h.exec(t, `ROLLBACK`)

	root, v := h.led.Root()
	if root != rootBefore || v != vBefore {
		t.Fatalf("rollback moved the ledger")
	}
	if got := h.one(t, h.ctx, `SELECT COUNT(*) FROM t`); got != "10" {
		t.Fatalf("state after rollback: %s", got)
	}
}

func TestTxnInsertDeleteNetNoop(t *testing.T) {
	h := newTxnHarness(t)
	_, vBefore := h.led.Root()

	h.exec(t, `BEGIN`)
	h.exec(t, `INSERT INTO t VALUES (100, 7, 1000, 'tag-100')`)
	h.exec(t, `DELETE FROM t WHERE id = 100`)
	// Delete then re-insert an existing key across statements.
	h.exec(t, `DELETE FROM t WHERE id = 3`)
	h.exec(t, `INSERT INTO t VALUES (3, 0, 333, 'tag-3b')`)
	h.exec(t, `COMMIT`)

	if _, v := h.led.Root(); v != vBefore+1 {
		t.Fatalf("expected one version, got %d -> %d", vBefore, v)
	}
	if got := h.one(t, h.ctx, `SELECT COUNT(*) FROM t WHERE id = 100`); got != "0" {
		t.Fatalf("net-noop insert survived: %s", got)
	}
	if got := h.one(t, h.ctx, `SELECT amount FROM t WHERE id = 3`); got != "333" {
		t.Fatalf("delete+reinsert: %s", got)
	}
	if got := h.one(t, h.ctx, `SELECT id FROM t WHERE tag = 'tag-3b'`); got != "3" {
		t.Fatalf("index after delete+reinsert: %s", got)
	}
}

func TestTxnStatementAtomicity(t *testing.T) {
	h := newTxnHarness(t)
	h.exec(t, `BEGIN`)
	h.exec(t, `INSERT INTO t VALUES (100, 7, 1000, 'tag-100')`)
	// Fails on the duplicate PK in the second value; the whole
	// statement must be discarded, the earlier one kept.
	h.execErr(t, `INSERT INTO t VALUES (101, 7, 1, 'tag-101'), (0, 0, 0, 'tag-x')`)
	if got := h.one(t, h.ctx, `SELECT COUNT(*) FROM t WHERE id IN (100, 101)`); got != "1" {
		t.Fatalf("failed statement leaked or earlier statement lost: %s", got)
	}
	h.exec(t, `COMMIT`)
	if got := h.one(t, h.ctx, `SELECT COUNT(*) FROM t WHERE id IN (100, 101)`); got != "1" {
		t.Fatalf("after commit: %s", got)
	}
}

func TestTxnUniqueAcrossStatements(t *testing.T) {
	h := newTxnHarness(t)
	h.exec(t, `BEGIN`)
	h.exec(t, `INSERT INTO t VALUES (100, 7, 1000, 'tag-100')`)
	// Same unique tag as the buffered insert: must fail already.
	h.execErr(t, `INSERT INTO t VALUES (101, 7, 1, 'tag-100')`)
	// Freeing a committed tag inside the transaction makes it usable.
	h.exec(t, `UPDATE t SET tag = 'tag-5b' WHERE id = 5`)
	h.exec(t, `INSERT INTO t VALUES (102, 7, 1, 'tag-5')`)
	h.exec(t, `COMMIT`)
	if got := h.one(t, h.ctx, `SELECT id FROM t WHERE tag = 'tag-5'`); got != "102" {
		t.Fatalf("reused freed tag: %s", got)
	}
}

func TestTxnConflictOnConcurrentWrite(t *testing.T) {
	h := newTxnHarness(t)
	other := h.otherCtx()

	h.exec(t, `BEGIN`)
	h.exec(t, `UPDATE t SET amount = amount + 1 WHERE id = 0`)

	// Another session commits to the same row first.
	h.execCtx(t, other, `UPDATE t SET amount = 500 WHERE id = 0`)

	_, err := h.eng.Exec(h.ctx, `COMMIT`)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("expected ErrTxnConflict, got %v", err)
	}
	// The losing transaction is rolled back; the winner's value stands.
	if got := h.one(t, h.ctx, `SELECT amount FROM t WHERE id = 0`); got != "500" {
		t.Fatalf("after conflict: %s", got)
	}
}

func TestTxnNoConflictOnDisjointWrite(t *testing.T) {
	h := newTxnHarness(t)
	other := h.otherCtx()

	h.exec(t, `BEGIN`)
	h.exec(t, `UPDATE t SET amount = 111 WHERE id = 1`)
	h.execCtx(t, other, `UPDATE t SET amount = 222 WHERE id = 2`)
	h.exec(t, `COMMIT`)

	if got := h.one(t, h.ctx, `SELECT amount FROM t WHERE id = 1`); got != "111" {
		t.Fatalf("disjoint txn write lost: %s", got)
	}
	if got := h.one(t, h.ctx, `SELECT amount FROM t WHERE id = 2`); got != "222" {
		t.Fatalf("concurrent write lost: %s", got)
	}
}

func TestTxnConflictOnConcurrentUnique(t *testing.T) {
	h := newTxnHarness(t)
	other := h.otherCtx()

	h.exec(t, `BEGIN`)
	h.exec(t, `INSERT INTO t VALUES (100, 7, 1, 'tag-new')`)
	h.execCtx(t, other, `INSERT INTO t VALUES (200, 8, 2, 'tag-new')`)

	_, err := h.eng.Exec(h.ctx, `COMMIT`)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("expected ErrTxnConflict on unique collision, got %v", err)
	}
	if got := h.one(t, h.ctx, `SELECT id FROM t WHERE tag = 'tag-new'`); got != "200" {
		t.Fatalf("winner row: %s", got)
	}
}

func TestTxnSavepoints(t *testing.T) {
	h := newTxnHarness(t)
	h.exec(t, `BEGIN`)
	h.exec(t, `INSERT INTO t VALUES (100, 7, 1, 'tag-100')`)
	h.exec(t, `SAVEPOINT sp1`)
	h.exec(t, `INSERT INTO t VALUES (101, 7, 2, 'tag-101')`)
	h.exec(t, `UPDATE t SET amount = 999 WHERE id = 0`)
	h.exec(t, `ROLLBACK TO SAVEPOINT sp1`)

	if got := h.one(t, h.ctx, `SELECT COUNT(*) FROM t WHERE id = 101`); got != "0" {
		t.Fatalf("rollback-to-savepoint kept later insert: %s", got)
	}
	if got := h.one(t, h.ctx, `SELECT amount FROM t WHERE id = 0`); got != "0" {
		t.Fatalf("rollback-to-savepoint kept later update: %s", got)
	}
	if got := h.one(t, h.ctx, `SELECT COUNT(*) FROM t WHERE id = 100`); got != "1" {
		t.Fatalf("rollback-to-savepoint dropped earlier insert: %s", got)
	}
	// The savepoint survives a rollback to it.
	h.exec(t, `INSERT INTO t VALUES (102, 7, 3, 'tag-102')`)
	h.exec(t, `ROLLBACK TO SAVEPOINT sp1`)
	if got := h.one(t, h.ctx, `SELECT COUNT(*) FROM t WHERE id = 102`); got != "0" {
		t.Fatalf("second rollback to savepoint: %s", got)
	}
	h.exec(t, `RELEASE SAVEPOINT sp1`)
	h.execErr(t, `ROLLBACK TO SAVEPOINT sp1`)
	h.exec(t, `COMMIT`)
	if got := h.one(t, h.ctx, `SELECT COUNT(*) FROM t WHERE id = 100`); got != "1" {
		t.Fatalf("committed state: %s", got)
	}
}

func TestTxnAutocommitOff(t *testing.T) {
	h := newTxnHarness(t)
	other := h.otherCtx()

	h.exec(t, `SET autocommit = 0`)
	h.exec(t, `INSERT INTO t VALUES (100, 7, 1, 'tag-100')`)
	if got := h.one(t, other, `SELECT COUNT(*) FROM t WHERE id = 100`); got != "0" {
		t.Fatalf("autocommit=0 leaked before COMMIT: %s", got)
	}
	h.exec(t, `COMMIT`)
	if got := h.one(t, other, `SELECT COUNT(*) FROM t WHERE id = 100`); got != "1" {
		t.Fatalf("autocommit=0 commit: %s", got)
	}
}

func TestTxnDDLImpliesCommit(t *testing.T) {
	h := newTxnHarness(t)
	other := h.otherCtx()

	h.exec(t, `BEGIN`)
	h.exec(t, `INSERT INTO t VALUES (100, 7, 1, 'tag-100')`)
	h.exec(t, `CREATE INDEX idx_amount ON t (amount)`)

	// The DDL ended the transaction: the buffered insert is committed
	// and visible to other sessions, and indexed by the new index.
	if got := h.one(t, other, `SELECT COUNT(*) FROM t WHERE id = 100`); got != "1" {
		t.Fatalf("DDL implicit commit: %s", got)
	}
	if got := h.one(t, other, `SELECT id FROM t WHERE amount = 1`); got != "100" {
		t.Fatalf("new index covers txn row: %s", got)
	}
}

func TestTxnCrashLosesNothingCommitted(t *testing.T) {
	backend := ledger.NewMemBackend()
	led, err := ledger.Create(backend, testCK)
	if err != nil {
		t.Fatal(err)
	}
	h := attach(t, backend, led)
	h.exec(t, `CREATE TABLE t (id BIGINT PRIMARY KEY, v BIGINT)`)
	h.exec(t, `INSERT INTO t VALUES (1, 10)`)
	rootBefore, vBefore := led.Root()

	h.exec(t, `BEGIN`)
	h.exec(t, `INSERT INTO t VALUES (2, 20)`)
	h.exec(t, `UPDATE t SET v = 99 WHERE id = 1`)
	// Crash: the session vanishes without COMMIT. Reopen from storage.
	led2, err := ledger.OpenLatest(backend, testCK)
	if err != nil {
		t.Fatal(err)
	}
	root2, v2 := led2.Root()
	if root2 != rootBefore || v2 != vBefore {
		t.Fatalf("uncommitted transaction reached storage")
	}
	h2 := attach(t, backend, led2)
	if got := h2.one(t, h2.ctx, `SELECT v FROM t WHERE id = 1`); got != "10" {
		t.Fatalf("after crash: %s", got)
	}
}

func TestTxnDeterministicRoots(t *testing.T) {
	run := func() ledger.Hash {
		backend := ledger.NewMemBackend()
		led, err := ledger.Create(backend, testCK)
		if err != nil {
			t.Fatal(err)
		}
		h := attach(t, backend, led)
		h.exec(t, `CREATE TABLE t (id BIGINT PRIMARY KEY, v BIGINT)`)
		h.exec(t, `INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)`)
		h.exec(t, `BEGIN`)
		h.exec(t, `UPDATE t SET v = v * 2 WHERE id < 3`)
		h.exec(t, `DELETE FROM t WHERE id = 3`)
		h.exec(t, `ROLLBACK`)
		h.exec(t, `BEGIN`)
		h.exec(t, `UPDATE t SET v = v + 5`)
		h.exec(t, `INSERT INTO t VALUES (4, 40)`)
		h.exec(t, `COMMIT`)
		root, _ := led.Root()
		return root
	}
	if run() != run() {
		t.Fatal("identical transactional histories produced different roots")
	}
}

func TestTxnReadOnlyRefusesWrites(t *testing.T) {
	h := newTxnHarness(t)
	h.exec(t, `START TRANSACTION READ ONLY`)
	if _, err := h.eng.Exec(h.ctx, `INSERT INTO t VALUES (100, 7, 1, 'tag-100')`); err == nil {
		t.Fatal("write in read-only transaction succeeded")
	}
	h.exec(t, `ROLLBACK`)
}

func TestTxnCrossTableAtomic(t *testing.T) {
	h := newHarness(t)
	h.exec(t, `CREATE TABLE a (id BIGINT PRIMARY KEY, v BIGINT)`)
	h.exec(t, `CREATE TABLE b (id BIGINT PRIMARY KEY, v BIGINT)`)
	h.exec(t, `INSERT INTO a VALUES (1, 100)`)
	h.exec(t, `INSERT INTO b VALUES (1, 0)`)
	_, vBefore := h.led.Root()

	h.exec(t, `BEGIN`)
	h.exec(t, `UPDATE a SET v = v - 40 WHERE id = 1`)
	h.exec(t, `UPDATE b SET v = v + 40 WHERE id = 1`)
	h.exec(t, `COMMIT`)

	if _, v := h.led.Root(); v != vBefore+1 {
		t.Fatalf("cross-table transaction took %d versions", v-vBefore)
	}
	if got := h.one(t, h.ctx, `SELECT v FROM a WHERE id = 1`); got != "60" {
		t.Fatalf("table a: %s", got)
	}
	if got := h.one(t, h.ctx, `SELECT v FROM b WHERE id = 1`); got != "40" {
		t.Fatalf("table b: %s", got)
	}
}

// A chained ledger under the SQL layer: transactions extend the chain
// one link per commit, the lineage verifies, and identical SQL
// histories still produce identical roots and heads.
func TestTxnWithHistoryChain(t *testing.T) {
	run := func() (*harness, ledger.Hash, ledger.Hash) {
		backend := ledger.NewMemBackend()
		led, err := ledger.Create(backend, testCK, ledger.WithHistoryChain())
		if err != nil {
			t.Fatal(err)
		}
		h := attach(t, backend, led)
		h.exec(t, `CREATE TABLE t (id BIGINT PRIMARY KEY, v BIGINT)`)
		h.exec(t, `INSERT INTO t VALUES (1, 10), (2, 20)`)
		h.exec(t, `BEGIN`)
		h.exec(t, `UPDATE t SET v = v + 1 WHERE id = 1`)
		h.exec(t, `DELETE FROM t WHERE id = 2`)
		h.exec(t, `COMMIT`)
		root, _ := led.Root()
		head, _, err := led.HistoryHead()
		if err != nil {
			t.Fatal(err)
		}
		return h, root, head
	}
	h1, r1, hd1 := run()
	_, r2, hd2 := run()
	if r1 != r2 || hd1 != hd2 {
		t.Fatal("identical SQL histories diverged under the chain")
	}
	if err := h1.led.VerifyHistory(0, ledger.Hash{}); err != nil {
		t.Fatalf("verify SQL-driven chain: %v", err)
	}
	// The whole transaction is one chain link.
	if _, v := h1.led.Root(); v != 3 {
		t.Fatalf("expected 3 versions (create, insert, txn), got %d", v)
	}
}
