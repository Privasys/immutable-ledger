# Contributing

Thank you for your interest in immutable-ledger.

## Layout

| Package | Purpose |
| --- | --- |
| [`ledger`](ledger) | The core authenticated store: versioned sparse Merkle tree, proofs, pruning, forks, leaf export/restore. Standard library only. |
| [`backend/pebble`](backend/pebble) | Production storage adapter over [Pebble](https://github.com/cockroachdb/pebble). |
| [`sqlledger`](sqlledger) | MySQL-dialect SQL over the ledger via [go-mysql-server](https://github.com/dolthub/go-mysql-server), embedded in-process. |

## Building and testing

```bash
git clone https://github.com/Privasys/immutable-ledger.git
cd immutable-ledger
go vet ./...
go test ./...
go test -race ./...
```

CI runs exactly these on every push and pull request.

## Compatibility invariants

Two properties are contracts, enforced by tests, and must never change
silently:

- **Cross-implementation roots and proofs.** The commitment scheme,
  node encodings and proof format are byte-identical to the reference
  Rust implementation in
  [enclave-os-mini](https://github.com/Privasys/enclave-os-mini)
  (`crates/enclave-os-merkle`). `ledger/compat_test.go` pins golden
  vectors generated from it. A change that breaks this suite is a
  synchronised format bump across both implementations or it is a bug.
- **The root is a pure function of logical content and the commitment
  key.** No change may make the root depend on batch boundaries,
  insertion order, storage mode, or anything at rest.

## Changes we welcome

Bug reports and fixes, additional storage backends, SQL surface
improvements within the documented model (the engine is
go-mysql-server; the ledger stays the sole authenticated record;
indexes stay derived state), documentation, and benchmarks. For larger
changes, please open an issue describing the design first.

## Licence

By contributing you agree that your contributions are licensed under
the [AGPL-3.0](LICENSE).
