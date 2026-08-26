// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"context"
	"fmt"
	"testing"

	"github.com/dolthub/go-mysql-server/sql"

	ledger "github.com/Privasys/immutable-ledger/ledger"
)

var testCK = [ledger.KeySize]byte{0x10, 0x20, 0x30}

type harness struct {
	backend *ledger.MemBackend
	led     *ledger.Store
	store   *Store
	eng     *Engine
	ctx     *sql.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	backend := ledger.NewMemBackend()
	led, err := ledger.Create(backend, testCK)
	if err != nil {
		t.Fatal(err)
	}
	return attach(t, backend, led)
}

func attach(t *testing.T, backend *ledger.MemBackend, led *ledger.Store) *harness {
	t.Helper()
	store, err := Open(led, backend, "app")
	if err != nil {
		t.Fatalf("open sql store: %v", err)
	}
	eng := NewEngine(store)
	return &harness{
		backend: backend,
		led:     led,
		store:   store,
		eng:     eng,
		ctx:     eng.NewContext(context.Background()),
	}
}

func (h *harness) exec(t *testing.T, q string) []sql.Row {
	t.Helper()
	rows, err := h.eng.Exec(h.ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return rows
}

func (h *harness) execErr(t *testing.T, q string) error {
	t.Helper()
	_, err := h.eng.Exec(h.ctx, q)
	if err == nil {
		t.Fatalf("%s: expected an error", q)
	}
	return err
}

func TestCRUDAndTypes(t *testing.T) {
	h := newHarness(t)
	h.exec(t, `CREATE TABLE accounts (
		id BIGINT PRIMARY KEY,
		name VARCHAR(64) NOT NULL,
		balance DOUBLE,
		notes TEXT,
		payload VARBINARY(100),
		created DATETIME(6),
		active TINYINT
	)`)
	h.exec(t, `INSERT INTO accounts VALUES
		(1, 'alice', 100.5, 'first', NULL, '2026-08-26 10:00:00', 1),
		(2, 'bob', -3.25, NULL, x'DEADBEEF', '2026-08-26 11:00:00', 0),
		(3, 'carol', 0, 'third', x'00FF00', NULL, 1)`)

	rows := h.exec(t, `SELECT id, name, balance FROM accounts ORDER BY id`)
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0][1] != "alice" || rows[1][1] != "bob" || rows[2][1] != "carol" {
		t.Fatalf("names = %v %v %v", rows[0][1], rows[1][1], rows[2][1])
	}
	if rows[1][2] != float64(-3.25) {
		t.Fatalf("bob balance = %v (%T)", rows[1][2], rows[1][2])
	}

	// Point select by primary key (served by the PK index).
	rows = h.exec(t, `SELECT name FROM accounts WHERE id = 2`)
	if len(rows) != 1 || rows[0][0] != "bob" {
		t.Fatalf("point select = %v", rows)
	}

	// NULL round trip.
	rows = h.exec(t, `SELECT notes, payload, created FROM accounts WHERE id = 2`)
	if rows[0][0] != nil {
		t.Fatalf("bob notes = %v", rows[0][0])
	}
	if b, ok := rows[0][1].([]byte); !ok || len(b) != 4 {
		t.Fatalf("bob payload = %v (%T)", rows[0][1], rows[0][1])
	}

	// UPDATE and DELETE.
	h.exec(t, `UPDATE accounts SET balance = balance + 10 WHERE id = 1`)
	rows = h.exec(t, `SELECT balance FROM accounts WHERE id = 1`)
	if rows[0][0] != float64(110.5) {
		t.Fatalf("alice after update = %v", rows[0][0])
	}
	h.exec(t, `DELETE FROM accounts WHERE id = 3`)
	rows = h.exec(t, `SELECT COUNT(*) FROM accounts`)
	if fmt.Sprint(rows[0][0]) != "2" {
		t.Fatalf("count = %v", rows[0][0])
	}

	// Duplicate primary key fails.
	h.execErr(t, `INSERT INTO accounts (id, name) VALUES (1, 'dup')`)

	// Aggregation and expressions.
	rows = h.exec(t, `SELECT SUM(balance), MAX(name) FROM accounts`)
	if rows[0][0] != float64(107.25) {
		t.Fatalf("sum = %v", rows[0][0])
	}
}

func TestAutoIncrement(t *testing.T) {
	h := newHarness(t)
	h.exec(t, `CREATE TABLE logs (
		id BIGINT UNSIGNED PRIMARY KEY AUTO_INCREMENT,
		msg VARCHAR(100) NOT NULL
	)`)
	h.exec(t, `INSERT INTO logs (msg) VALUES ('one'), ('two')`)
	h.exec(t, `INSERT INTO logs (id, msg) VALUES (10, 'ten')`)
	h.exec(t, `INSERT INTO logs (msg) VALUES ('eleven')`)
	rows := h.exec(t, `SELECT id, msg FROM logs ORDER BY id`)
	got := fmt.Sprint(rows)
	want := "[[1 one] [2 two] [10 ten] [11 eleven]]"
	if got != want {
		t.Fatalf("auto-inc rows = %s, want %s", got, want)
	}
}

func TestSecondaryIndexAndUnique(t *testing.T) {
	h := newHarness(t)
	h.exec(t, `CREATE TABLE users (
		id BIGINT PRIMARY KEY,
		email VARCHAR(128),
		age BIGINT
	)`)
	for i := 1; i <= 50; i++ {
		h.exec(t, fmt.Sprintf(
			`INSERT INTO users VALUES (%d, 'u%02d@example.com', %d)`, i, i, 20+i%10))
	}
	h.exec(t, `CREATE UNIQUE INDEX idx_email ON users (email)`)
	h.exec(t, `CREATE INDEX idx_age ON users (age)`)

	// Unique index enforced for new writes.
	h.execErr(t, `INSERT INTO users VALUES (99, 'u07@example.com', 30)`)
	// NULLs are exempt from uniqueness.
	h.exec(t, `INSERT INTO users VALUES (100, NULL, 41)`)
	h.exec(t, `INSERT INTO users VALUES (101, NULL, 42)`)

	// Point and range queries through the secondary indexes.
	rows := h.exec(t, `SELECT id FROM users WHERE email = 'u33@example.com'`)
	if len(rows) != 1 || fmt.Sprint(rows[0][0]) != "33" {
		t.Fatalf("email point = %v", rows)
	}
	// Ages 28 and 29 appear five times each among ids 1..50, and the
	// two NULL-email rows carry 41 and 42.
	rows = h.exec(t, `SELECT COUNT(*) FROM users WHERE age > 27`)
	if fmt.Sprint(rows[0][0]) != "12" {
		t.Fatalf("age range count = %v", rows[0][0])
	}
	rows = h.exec(t, `SELECT COUNT(*) FROM users WHERE age BETWEEN 25 AND 27`)
	if fmt.Sprint(rows[0][0]) != "15" {
		t.Fatalf("between count = %v", rows[0][0])
	}
	rows = h.exec(t, `SELECT id FROM users WHERE email IS NULL ORDER BY id`)
	if len(rows) != 2 {
		t.Fatalf("is null = %v", rows)
	}

	// Updates maintain the index.
	h.exec(t, `UPDATE users SET age = 99 WHERE id = 7`)
	rows = h.exec(t, `SELECT id FROM users WHERE age = 99`)
	if len(rows) != 1 || fmt.Sprint(rows[0][0]) != "7" {
		t.Fatalf("post-update index = %v", rows)
	}
	h.exec(t, `DELETE FROM users WHERE id = 7`)
	rows = h.exec(t, `SELECT id FROM users WHERE age = 99`)
	if len(rows) != 0 {
		t.Fatalf("post-delete index = %v", rows)
	}

	// Unique creation over conflicting data fails.
	h.exec(t, `UPDATE users SET age = 55`)
	h.execErr(t, `CREATE UNIQUE INDEX idx_age_u ON users (age)`)

	h.exec(t, `DROP INDEX idx_age ON users`)
	rows = h.exec(t, `SELECT COUNT(*) FROM users WHERE age = 55`)
	if fmt.Sprint(rows[0][0]) != "51" {
		t.Fatalf("post-drop scan = %v", rows[0][0])
	}
}

func TestJoin(t *testing.T) {
	h := newHarness(t)
	h.exec(t, `CREATE TABLE customers (id BIGINT PRIMARY KEY, name VARCHAR(50) NOT NULL)`)
	h.exec(t, `CREATE TABLE orders (
		id BIGINT PRIMARY KEY,
		customer_id BIGINT NOT NULL,
		total DOUBLE NOT NULL
	)`)
	h.exec(t, `INSERT INTO customers VALUES (1, 'acme'), (2, 'globex')`)
	h.exec(t, `INSERT INTO orders VALUES (10, 1, 5.0), (11, 1, 7.5), (12, 2, 3.0)`)
	h.exec(t, `CREATE INDEX idx_cust ON orders (customer_id)`)

	rows := h.exec(t, `
		SELECT c.name, SUM(o.total)
		FROM customers c JOIN orders o ON o.customer_id = c.id
		GROUP BY c.name ORDER BY c.name`)
	got := fmt.Sprint(rows)
	if got != "[[acme 12.5] [globex 3]]" {
		t.Fatalf("join = %s", got)
	}
}

func TestRootReflectsSQLStateAndIsDeterministic(t *testing.T) {
	run := func() ledger.Hash {
		h := newHarness(t)
		h.exec(t, `CREATE TABLE t (id BIGINT PRIMARY KEY, v VARCHAR(20))`)
		h.exec(t, `INSERT INTO t VALUES (1,'a'), (2,'b'), (3,'c')`)
		h.exec(t, `UPDATE t SET v = 'z' WHERE id = 2`)
		h.exec(t, `DELETE FROM t WHERE id = 3`)
		root, _ := h.led.Root()
		return root
	}
	r1, r2 := run(), run()
	if r1 != r2 {
		t.Fatalf("same SQL history, different roots: %x vs %x", r1, r2)
	}

	// And a divergent history diverges.
	h := newHarness(t)
	h.exec(t, `CREATE TABLE t (id BIGINT PRIMARY KEY, v VARCHAR(20))`)
	h.exec(t, `INSERT INTO t VALUES (1,'a'), (2,'b'), (3,'c')`)
	h.exec(t, `UPDATE t SET v = 'DIFFERENT' WHERE id = 2`)
	h.exec(t, `DELETE FROM t WHERE id = 3`)
	r3, _ := h.led.Root()
	if r3 == r1 {
		t.Fatal("different SQL state, same root")
	}
}

func TestRestartAndRebuild(t *testing.T) {
	backend := ledger.NewMemBackend()
	led, err := ledger.Create(backend, testCK)
	if err != nil {
		t.Fatal(err)
	}
	h := attach(t, backend, led)
	h.exec(t, `CREATE TABLE kv (k VARCHAR(50) PRIMARY KEY, v BIGINT)`)
	h.exec(t, `INSERT INTO kv VALUES ('x', 1), ('y', 2), ('z', 3)`)
	h.exec(t, `CREATE INDEX idx_v ON kv (v)`)
	rootBefore, _ := led.Root()

	// "Restart": reopen the ledger from its checkpoint and reattach.
	led2, err := ledger.OpenLatest(backend, testCK)
	if err != nil {
		t.Fatal(err)
	}
	h2 := attach(t, backend, led2)
	rows := h2.exec(t, `SELECT v FROM kv WHERE k = 'y'`)
	if fmt.Sprint(rows[0][0]) != "2" {
		t.Fatalf("after restart = %v", rows)
	}

	// Crash simulation: derived keyspace lost entirely. Reattach must
	// rebuild it from the ledger and answer identically.
	for _, key := range backend.Keys() {
		if key[0] == scanPrefix || key[0] == secondaryPrefix || key[0] == scanMetaKey {
			backend.Remove(key)
		}
	}
	led3, err := ledger.OpenLatest(backend, testCK)
	if err != nil {
		t.Fatal(err)
	}
	h3 := attach(t, backend, led3)
	rows = h3.exec(t, `SELECT k FROM kv WHERE v > 1 ORDER BY k`)
	if fmt.Sprint(rows) != "[[y] [z]]" {
		t.Fatalf("after rebuild = %v", rows)
	}
	rootAfter, _ := led3.Root()
	if rootAfter != rootBefore {
		t.Fatalf("rebuild changed the root: %x vs %x", rootAfter, rootBefore)
	}
}

func TestDDL(t *testing.T) {
	h := newHarness(t)
	h.exec(t, `CREATE TABLE a (id BIGINT PRIMARY KEY, v VARCHAR(10))`)
	h.exec(t, `INSERT INTO a VALUES (1, 'x')`)
	h.exec(t, `RENAME TABLE a TO b`)
	rows := h.exec(t, `SELECT v FROM b WHERE id = 1`)
	if rows[0][0] != "x" {
		t.Fatalf("after rename = %v", rows)
	}
	h.execErr(t, `SELECT * FROM a`)

	h.exec(t, `TRUNCATE TABLE b`)
	rows = h.exec(t, `SELECT COUNT(*) FROM b`)
	if fmt.Sprint(rows[0][0]) != "0" {
		t.Fatalf("after truncate = %v", rows[0][0])
	}

	h.exec(t, `DROP TABLE b`)
	h.execErr(t, `SELECT * FROM b`)

	// Keyless tables are rejected with a clear error.
	h.execErr(t, `CREATE TABLE nopk (v BIGINT)`)

	// SHOW surfaces the catalog.
	h.exec(t, `CREATE TABLE c (id BIGINT PRIMARY KEY)`)
	rows = h.exec(t, `SHOW TABLES`)
	if len(rows) != 1 || rows[0][0] != "c" {
		t.Fatalf("show tables = %v", rows)
	}
}

func TestVerifiedReads(t *testing.T) {
	h := newHarness(t)
	h.exec(t, `CREATE TABLE docs (id BIGINT PRIMARY KEY, body TEXT)`)
	h.exec(t, `INSERT INTO docs VALUES (7, 'hello world')`)

	vr, err := h.store.VerifiedGet("docs", int64(7))
	if err != nil {
		t.Fatal(err)
	}
	if vr.Row == nil || vr.Row[1] != "hello world" {
		t.Fatalf("verified row = %v", vr.Row)
	}
	ok, err := h.store.Verify(vr, &vr.Root)
	if err != nil || !ok {
		t.Fatalf("verify: %v %v", ok, err)
	}
	// The proof does not hold against a different (later) root.
	h.exec(t, `INSERT INTO docs VALUES (8, 'later')`)
	newRoot, _ := h.led.Root()
	if ok, _ := h.store.Verify(vr, &newRoot); ok {
		t.Fatal("stale proof verified against the new root")
	}

	// Absence with proof.
	vrAbs, err := h.store.VerifiedGet("docs", int64(999))
	if err != nil {
		t.Fatal(err)
	}
	if vrAbs.Row != nil {
		t.Fatalf("absent row = %v", vrAbs.Row)
	}
	ok, err = h.store.Verify(vrAbs, &vrAbs.Root)
	if err != nil || !ok {
		t.Fatalf("absence verify: %v %v", ok, err)
	}
}

func TestPlainKVCoexists(t *testing.T) {
	// SQL state and plain KV entries share one ledger without
	// interfering; the root covers both.
	h := newHarness(t)
	h.exec(t, `CREATE TABLE t (id BIGINT PRIMARY KEY)`)
	h.exec(t, `INSERT INTO t VALUES (1)`)
	if _, _, err := h.led.PutBatch([]ledger.Op{ledger.Put([]byte("app-config"), []byte("v1"))}); err != nil {
		t.Fatal(err)
	}
	rows := h.exec(t, `SELECT COUNT(*) FROM t`)
	if fmt.Sprint(rows[0][0]) != "1" {
		t.Fatalf("count = %v", rows[0][0])
	}
	v, ok, err := h.led.Get([]byte("app-config"))
	if err != nil || !ok || string(v) != "v1" {
		t.Fatalf("plain kv = %q %v %v", v, ok, err)
	}
}
