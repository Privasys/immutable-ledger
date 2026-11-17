# Benchmarks

Measured 2026-11-17 at `v1.0.0` (commit `d72b970`) on a GCE
`n2-standard-8` (Intel Xeon @ 2.80GHz, 8 vCPUs, `pd-ssd` boot disk),
Debian 12, Go 1.27.4, built with `-tags=gms_pure_go`. All figures are
**single-goroutine**: one writer, one reader, no concurrency. They
measure per-operation cost, not saturated throughput.

The "Before 1.0.0" column is the same suite on the same machine type
at commit `e231492` (2026-08-27, Go 1.24.5), before the node cache was
reworked.

## Core store (in-memory backend: CPU and codec cost only)

| Benchmark | v1.0.0 | Before 1.0.0 |
| --- | --- | --- |
| `PutBatch`, 100-row batches at 100k keys | 4.86 ms/batch ≈ 20,560 rows/s | 14.4 ms ≈ 6,950 rows/s |
| `Get`, warm cache at 100k keys | 37 µs | 85 µs |
| `Prove` at 100k keys | 59 µs | 107 µs |
| `Verify` a proof (pure function) | 11.4 µs | 11.4 µs |

## Ledger over Pebble (disk, synced batches unless noted)

| Benchmark | v1.0.0 | Before 1.0.0 |
| --- | --- | --- |
| `PutBatch`, 1 row per commit | 680 µs ≈ 1,470 commits/s | 852 µs ≈ 1,170 commits/s |
| `PutBatch`, 100-row batches | 9.15 ms ≈ 10,930 rows/s | 19.5 ms ≈ 5,100 rows/s |
| `PutBatch`, 1000-row batches | 73.1 ms ≈ 13,680 rows/s | 151 ms ≈ 6,640 rows/s |
| `PutBatch`, 100-row batches, unsynced | 8.25 ms ≈ 12,120 rows/s | 18.3 ms ≈ 5,470 rows/s |
| `Get`, warm cache at 100k keys | 83 µs | 143 µs |
| `Get`, cache disabled at 100k keys | 127 µs | 123 µs |
| `Prove` at 100k keys | 88 µs | 150 µs |

## SQL stack (engine + ledger + Pebble on disk, synced commits)

| Benchmark | v1.0.0 | Before 1.0.0 |
| --- | --- | --- |
| `INSERT`, single statement | 1.97 ms ≈ 510 statements/s | 2.11 ms ≈ 470 statements/s |
| `INSERT`, 500-row statement | 45.8 ms ≈ 10,920 rows/s | 66.7 ms ≈ 7,490 rows/s |
| Point `SELECT` by primary key | 216 µs ≈ 4,640 queries/s | 281 µs ≈ 3,560 queries/s |
| `UPDATE` by primary key | 1.32 ms | 1.63 ms |
| `COUNT(*)` over a 1,000-row index range | 93.5 ms ≈ 10,690 rows/s scanned | 149 ms ≈ 6,700 rows/s |
| `VerifiedGet` (row + inclusion proof + verification) | 166 µs | 224 µs |

## Reading the numbers

- **Batched writes are CPU-bound, not disk-bound.** At batch sizes of
  100 and above, disabling the WAL fsync barely moves the figure: the
  ceiling is commitment hashing (keyed hashes plus tree rebuild along
  the touched paths), around 11,000 to 14,000 rows/s per core on disk.
  Larger batches amortise better.
- **Per-statement writes are fsync-bound.** Every commit is one synced
  atomic batch; a single-row commit costs a WAL fsync (about 1,500
  commits/s on this disk). Batch your writes.
- **Range scans pay for verification.** Every row returned by a scan
  is re-read from the ledger and verified against the root (about
  94 µs/row here); the derived index keyspace is never trusted for row
  content. That is a design decision, not an accident.
- Reads and proofs sit in the 40 to 220 µs band through every layer,
  including the full SQL engine.
- **The 1.0.0 node cache** (constant-time LRU, 16,384 nodes by
  default) is where most of the gain over the earlier figures comes
  from: the upper levels of the tree stay in memory, so commits and
  reads rebuild and verify paths without going back to the backend.
  `Get` with the cache disabled is unchanged, as expected.

## Reproducing

The benchmarks live in the repository and run with the standard Go
tooling on any Linux machine with Go 1.27.3 or later:

```bash
git clone https://github.com/Privasys/immutable-ledger.git
cd immutable-ledger
export GOFLAGS=-tags=gms_pure_go

go test -bench=. -benchtime=2s -run=NONE ./ledger/           # core, in-memory
go test -bench=. -benchtime=2s -run=NONE ./backend/pebble/   # ledger on disk
go test -bench=. -benchtime=2s -timeout=60m -run=NONE ./sqlledger/  # SQL stack
```

Notes for comparable numbers:

- Use a machine with an SSD and idle CPUs; the disk-backed suites
  create their stores under `TMPDIR`, so point it at the disk you mean
  to measure.
- The reference machine above was created with:

  ```bash
  gcloud compute instances create ledger-bench \
      --machine-type=n2-standard-8 \
      --image-family=debian-12 --image-project=debian-cloud \
      --boot-disk-size=50GB --boot-disk-type=pd-ssd
  ```

  and deleted afterwards. If you drive a headless VM through a
  startup script, send `go test`'s stdout and stderr to separate
  files: Pebble logs to stderr mid-line, so with the two streams
  merged, line-based filters (e.g. `grep Benchmark`) can drop the
  result lines.
- Benchmarks report wall-clock per operation from a single goroutine.
  For saturated-throughput figures, run multiple readers against one
  store (reads take a shared lock); writes are serialised by design.
