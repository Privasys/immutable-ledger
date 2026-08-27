# Auditable history

The ledger's audit model is **state attestation with a committed
lineage**, not a replayable operation log. Each commit produces a root
that attests the entire logical state; the optional history chain
makes the current root additionally commit to the whole sequence of
roots before it. Auditors verify state and lineage directly; history
is retained only until an audit vouches for it, then physically
pruned — which is what lets "deleted" data actually disappear.

## The history chain

Enable at creation:

```go
store, err := ledger.Create(backend, ck, ledger.WithHistoryChain())
```

The choice is persisted (authenticated checkpoint flag, cross-checked
against the chain leaf itself at open) and cannot be toggled later,
because the chain changes what roots a given history produces.

Every effective commit writes one reserved leaf, `ledger.HistoryKey`
(`"\x00immutable-ledger:history-head"`), holding the chain head:

```
head_v = SHA-256("immutable-ledger:history:v1"
                 ‖ head_{v-1}          32 bytes
                 ‖ root_{v-1}          32 bytes
                 ‖ v                   u64 big-endian)
head_0 = 32 zero bytes
```

(`ledger.HistoryLink` is this function.) Because the head is an
ordinary leaf, `root_v` commits to `head_v`, which commits to every
earlier root: **the live root pins the entire root lineage**.
Fabricating an alternative history that ends at the same head is a
preimage attack. The head is provable state like any other key —
`Prove(HistoryKey)` binds it to an anchored root with no trust in the
server.

The reserved namespace `"\x00immutable-ledger:"` is not writable by
user batches in any mode.

## The audit workflow

Audits are owner-side or delegated: verifying content requires the
commitment key `ck`. Third parties without it can check root lineage
(heads and roots are not secret) but not contents.

From the previous audit's signed anchor `(version_A, root_A, head_A)`
— or `(0, emptyRoot, zeros)` for the first audit:

```go
// 1. Lineage: the recorded roots from the anchor are the unique
//    sequence ending at the live, root-bound head.
err := store.VerifyHistory(versionA, headA)

// 2. Content, as deeply as the audit requires:
changes, err := store.ChangesAt(v)     // what version v changed
value, ok, err := store.GetAt(v, key)  // verified historical reads
proof, err := store.Prove(key)         // per-key proofs vs any root

// 3. Sign the new anchor.
head, version, err := store.HistoryHead()
root, _ := store.Root()
// ... sign (version, root, head) with the audit key, store the record.

// 4. Prune everything the signature now vouches for.
stats, err := store.Prune(version)
```

After step 4, all superseded and deleted values in the audited range —
including the pruned chain segment — are physically gone. The signed
anchor stands in for them: trust rolls forward from audit to audit,
and a later `VerifyHistory(version_B, head_B)` verifies from the new
anchor without the pruned history.

Two consequences worth stating in a data-protection policy:

- **Erasure latency equals the audit cadence.** Data deleted just
  after an audit persists (in prunable history) until the next audit's
  prune. A missed audit extends the window, and storage carries the
  full inter-audit history.
- **Verification cost sits with the auditor.** `VerifyHistory` itself
  only walks stored roots (fast); replaying or reviewing content via
  `ChangesAt` costs work proportional to what changed.

`ChangesAt` reports leaf-level differences: the tree position (the
keyed hash of the logical key — logical keys are not recoverable from
the ledger) and the plaintext value for puts, or a deletion marker.
The reserved history leaf is omitted; `VerifyHistory` covers it.

## Interactions

- **Transactions.** A SQL transaction (or a sealed fork) is one
  commit, hence exactly one chain link; `(root_before, write_set,
  root_after)` from a sealed fork is independently verifiable.
- **Snapshot transfer.** Exported leaves include the head; a restored
  and `StampVersion`-ed replica continues the chain seamlessly, and
  `VerifyHistory` works from the transferred anchor onwards.
- **No-op batches** commit nothing and add no link.
- **Roots differ from a chainless store** over the same data (the
  chain leaf is state). Replicas must agree on the mode; the
  cross-implementation compatibility contract covers chainless stores
  today, and a port maintaining the chain must reproduce the link
  function and reserved key byte-for-byte.

## Threat model notes

- The chain narrows the documented restart-replay residual: replaying
  an old checkpoint with a matching old store now also has to discard
  a chain suffix, which the first comparison against any newer
  anchored head exposes.
- A stripped or replayed checkpoint cannot silently disable the chain:
  at open, the mode is cross-checked against the head leaf, which the
  (anchored) root authenticates.
- What the chain cannot do: prove that history *before the earliest
  retained anchor* looked one way or another — that is exactly what
  the audit signature vouches for after pruning.
