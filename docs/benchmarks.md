# Benchmarks

Measured 2026-08-27 at commit `e231492` on a GCE `n2-standard-8`
(Intel Xeon @ 2.80GHz, 8 vCPUs, `pd-ssd` boot disk), Debian 12,
Go 1.24.5. All figures are **single-goroutine**: one writer, one
reader, no concurrency — they measure per-operation cost, not
saturated throughput.

## Core store (in-memory backend — CPU and codec cost only)

| Benchmark | Result |
| --- | --- |
| `PutBatch`, 100-row batches at 100k keys | 14.4 ms/batch ≈ 6,950 rows/s |
| `Get`, warm cache at 100k keys | 85 µs |
| `Prove` at 100k keys | 107 µs |
| `Verify` a proof (pure function) | 11.4 µs |

## Ledger over Pebble (disk, synced batches unless noted)

| Benchmark | Result |
| --- | --- |
| `PutBatch`, 1 row per commit | 852 µs ≈ 1,170 commits/s |
| `PutBatch`, 100-row batches | 19.5 ms ≈ 5,100 rows/s |
| `PutBatch`, 1000-row batches | 151 ms ≈ 6,640 rows/s |
| `PutBatch`, 100-row batches, unsynced | 18.3 ms ≈ 5,470 rows/s |
| `Get`, warm cache at 100k keys | 143 µs |
| `Get`, cache disabled at 100k keys | 123 µs |
| `Prove` at 100k keys | 150 µs |

## SQL stack (engine + ledger + Pebble on disk, synced commits)

| Benchmark | Result |
| --- | --- |
| `INSERT`, single statement | 2.11 ms ≈ 470 statements/s |
| `INSERT`, 500-row statement | 66.7 ms ≈ 7,490 rows/s |
| Point `SELECT` by primary key | 281 µs ≈ 3,560 queries/s |
| `UPDATE` by primary key | 1.63 ms |
| `COUNT(*)` over a 1,000-row index range | 149 ms ≈ 6,700 rows/s scanned |
| `VerifiedGet` (row + inclusion proof + verification) | 224 µs |

## Reading the numbers

- **Batched writes are CPU-bound, not disk-bound.** At batch sizes of
  100 and above, disabling the WAL fsync barely moves the figure: the
  ceiling is commitment hashing (keyed hashes plus tree rebuild along
  the touched paths), around 5–7k rows/s per core. Larger batches
  amortise better.
- **Per-statement writes are fsync-bound.** Every commit is one synced
  atomic batch; a single-row commit costs a WAL fsync
  (~0.5–1.2k commits/s depending on the disk). Batch your writes.
- **Range scans pay for verification.** Every row returned by a scan
  is re-read from the ledger and verified against the root
  (~150 µs/row here); the derived index keyspace is never trusted for
  row content. That is a design decision, not an accident.
- Reads and proofs sit in the 100–300 µs band through every layer,
  including the full SQL engine.

## Reproducing

The benchmarks live in the repository and run with the standard Go
tooling — any Linux machine with Go 1.24+:

```bash
git clone https://github.com/Privasys/immutable-ledger.git
cd immutable-ledger

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
  startup script, capture the raw `go test` output: Pebble logs to
  stderr mid-line, so line-based filters (e.g. `grep Benchmark`) can
  drop the result lines.
- Benchmarks report wall-clock per operation from a single goroutine.
  For saturated-throughput figures, run multiple readers against one
  store (reads take a shared lock); writes are serialised by design.
