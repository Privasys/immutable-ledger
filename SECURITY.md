# Security Policy

## Reporting a vulnerability

Please report suspected vulnerabilities privately to
**security@privasys.org**. Do not open public issues for security
reports. We aim to acknowledge reports within 48 hours and to keep you
informed of progress; coordinated disclosure timelines are agreed per
report.

Please include what you can: affected commit, storage mode
(plaintext or `WithStorageKey`), backend, reproduction steps, and
impact assessment.

## Scope

Of particular interest, given the threat model (storage serves the
data; the store must never return unverified state):

- any path where a read returns data that did not verify against the
  in-memory root (stale records, dropped keys, resurrected deletes,
  swapped values);
- proof soundness: an inclusion or absence proof accepted by the pure
  verifier for a statement that is false at that root;
- root malleability: two different logical states producing the same
  root, or the same state producing different roots across
  implementations, batch shapes or storage modes;
- checkpoint forgery: opening a store from a checkpoint the store did
  not write (outside the documented whole-store replay residual);
- SQL-layer escapes: query results inconsistent with the ledger's
  authenticated rows (the derived index keyspace is a materialisation
  and must never be the source of row content).

Out of scope: denial of service through resource exhaustion on the
embedding application, and the documented restart-replay residual
(replaying a complete old store together with its matching
checkpoint), which external root anchoring addresses.

## Supported versions

The `main` branch. There are no maintained release branches yet.
