// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"fmt"
	"testing"
)

// Window functions are computed by the engine over the row stream the
// storage layer produces; this locks in that they work end to end.
func TestWindowFunctions(t *testing.T) {
	h := newHarness(t)
	h.exec(t, `CREATE TABLE w (id BIGINT PRIMARY KEY, grp BIGINT, amount BIGINT)`)
	for i := 0; i < 10; i++ {
		h.exec(t, fmt.Sprintf(`INSERT INTO w VALUES (%d, %d, %d)`, i, i%3, i*10))
	}

	// Top row per group via ROW_NUMBER in a CTE: groups 0/1/2 have
	// max amounts at ids 9/7/8.
	rows := h.exec(t, `WITH ranked AS (
		SELECT id, ROW_NUMBER() OVER (PARTITION BY grp ORDER BY amount DESC) rn FROM w
	) SELECT id FROM ranked WHERE rn = 1 ORDER BY id`)
	if len(rows) != 3 || fmt.Sprint(rows[0][0], rows[1][0], rows[2][0]) != "7 8 9" {
		t.Fatalf("top-per-group: %v", rows)
	}

	// Partitioned aggregate: group 0 holds ids 0,3,6,9 -> sum 180.
	rows = h.exec(t, `SELECT id, SUM(amount) OVER (PARTITION BY grp) s FROM w ORDER BY id`)
	if len(rows) != 10 || fmt.Sprint(rows[0][1]) != "180" {
		t.Fatalf("partitioned sum: %v", rows)
	}

	// Sliding frame: at id=5, SUM over (1 PRECEDING, CURRENT) = 40+50.
	rows = h.exec(t, `SELECT id, SUM(amount) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) s FROM w ORDER BY id`)
	if fmt.Sprint(rows[5][1]) != "90" {
		t.Fatalf("sliding frame: %v", rows)
	}

	// LAG/LEAD: first row has NULL lag; lead of id=0 is 10.
	rows = h.exec(t, `SELECT LAG(amount) OVER (ORDER BY id), LEAD(amount) OVER (ORDER BY id) FROM w ORDER BY id`)
	if rows[0][0] != nil || fmt.Sprint(rows[0][1]) != "10" {
		t.Fatalf("lag/lead: %v", rows[0])
	}

	// Ranking family parses and runs.
	for _, q := range []string{
		`SELECT RANK() OVER (ORDER BY grp) FROM w`,
		`SELECT DENSE_RANK() OVER (ORDER BY grp) FROM w`,
		`SELECT NTILE(3) OVER (ORDER BY id) FROM w`,
		`SELECT PERCENT_RANK() OVER (ORDER BY amount) FROM w`,
		`SELECT FIRST_VALUE(amount) OVER (PARTITION BY grp ORDER BY id) FROM w`,
	} {
		if rows = h.exec(t, q); len(rows) != 10 {
			t.Fatalf("%s: %v", q, rows)
		}
	}
}
