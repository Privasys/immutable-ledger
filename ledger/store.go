// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

// The versioned sparse Merkle store.
//
// # Commitments (encryption-independent root)
//
//   - path = HMAC-SHA256(ck, "p" ‖ key) — 256-bit tree position.
//   - vh   = HMAC-SHA256(ck, "v" ‖ path ‖ plaintext) — value commitment.
//   - The root is a pure function of the logical state and ck: replicas
//     sharing ck but holding different storage keys produce identical
//     roots and compare state as (version, root).
//
// Value bytes at rest are plaintext by default (the volume's encryption
// is the confidentiality layer — a LUKS data partition inside a TD,
// typically) or AES-256-GCM ciphertext when a per-machine storage key
// is configured (WithStorageKey). Nodes are plaintext (hashes only) in
// both modes, and integrity never depends on this layer: every read
// re-derives the keyed commitment.
//
// # Records (single backend keyspace, 1-byte prefix)
//
//	prefix | key                              | value
//	 'n'   | nodeKey                          | node encoding
//	 'v'   | vh ‖ value_version u64 BE        | value bytes (or GCM ciphertext)
//	 's'   | stale_since u64 BE ‖ target key  | empty
//	 'r'   | version u64 BE                   | root hash
//	 'c'   | (fixed)                          | authenticated (root, version)
//	                                            checkpoint (HMAC under a
//	                                            ck-derived key, or GCM under
//	                                            the storage key), written
//	                                            atomically inside every commit
//
// Node and value records are both versioned and immutable: a record is
// written by exactly one commit and never again (re-inserting a value
// after deleting it writes a new value record under the new version).
// The pruner therefore deletes stale targets blindly.
//
// A commit lands all of the above in one atomic WriteBatch; only after
// the backend confirms does the in-memory (root, version) swing.
//
// # Freshness
//
// Get verifies every node against the in-memory root, so storage cannot
// roll live reads back. GetAt authenticates content against the stored
// root record for that version — the version→root binding for history
// is backend-held.

const (
	recNode       = 'n'
	recValue      = 'v'
	recStale      = 's'
	recRoot       = 'r'
	recCheckpoint = 'c'
)

var (
	hmacTagPath   = []byte("p")
	hmacTagValue  = []byte("v")
	valueAADTag   = []byte("enclave_os_merkle_val")
	checkpointAAD = []byte("enclave_os_merkle_ckpt")
	ckptMACLabel  = []byte("immutable-ledger:ckpt-mac:v1")
)

func nodeRecordKey(nk *nodeKey) []byte {
	return append([]byte{recNode}, nk.encode()...)
}

func valueRecordKey(vh *Hash, valueVersion uint64) []byte {
	k := make([]byte, 0, 1+HashSize+8)
	k = append(k, recValue)
	k = append(k, vh[:]...)
	return binary.BigEndian.AppendUint64(k, valueVersion)
}

func staleRecordKey(staleSince uint64, targetRecordKey []byte) []byte {
	k := make([]byte, 0, 9+len(targetRecordKey))
	k = append(k, recStale)
	k = binary.BigEndian.AppendUint64(k, staleSince)
	return append(k, targetRecordKey...)
}

func rootRecordKey(version uint64) []byte {
	k := make([]byte, 0, 9)
	k = append(k, recRoot)
	return binary.BigEndian.AppendUint64(k, version)
}

func checkpointRecordKey() []byte { return []byte{recCheckpoint} }

func valueAAD(path *Hash) []byte {
	aad := make([]byte, 0, len(valueAADTag)+HashSize)
	aad = append(aad, valueAADTag...)
	return append(aad, path[:]...)
}

// Op is one operation of a PutBatch: a put (Value, possibly empty) or a
// delete. Use Put and Del to construct.
type Op struct {
	Key    []byte
	Value  []byte
	Delete bool
}

// Put builds an insert/overwrite op.
func Put(key, value []byte) Op { return Op{Key: key, Value: value} }

// Del builds a delete op.
func Del(key []byte) Op { return Op{Key: key, Delete: true} }

// PruneStats reports what a Prune call removed.
type PruneStats struct {
	// StaleEntries is the count of stale-index entries processed (and removed).
	StaleEntries int
	// RecordsDeleted is the count of node + value records deleted.
	RecordsDeleted int
	// RootRecordsDeleted is the count of root records deleted.
	RootRecordsDeleted int
}

// defaultCacheCapacity keeps ~10³ immutable node records (≈ a few
// hundred KiB) so the top tree levels stay permanently warm.
const defaultCacheCapacity = 1024

// pruneChunk is the record count per prune scan/delete round trip.
const pruneChunk = 512

// Store is the authenticated KV store. Single writer: it is not safe
// for concurrent use; wrap it in a mutex at the application layer.
type Store struct {
	backend Backend
	ck      []byte
	// cipher encrypts value records and the checkpoint when a storage
	// key was supplied (WithStorageKey). nil = plaintext values: the
	// deployment relies on volume encryption (TD attestation + LUKS)
	// for confidentiality at rest, and the checkpoint is authenticated
	// with ckptMAC instead. Integrity is identical in both modes —
	// every read still verifies the value commitment against the root.
	cipher *aeadCipher
	// ckptMAC keys the checkpoint HMAC in plaintext mode (derived from
	// ck; the root is not secret — root records store it plaintext —
	// only checkpoint authenticity matters).
	ckptMAC []byte
	root    Hash
	version uint64
	// rootChild is the current root's child reference (nil = empty tree).
	rootChild *child
	cache     *nodeCache
	// snapshotPin caps pruning while a snapshot streams from a version.
	snapshotPin *uint64
}

// Option configures a store at construction.
type Option func(*storeOptions)

type storeOptions struct {
	sk *[KeySize]byte
}

// WithStorageKey enables at-rest encryption of value records and the
// checkpoint under a per-machine storage key (AES-256-GCM), as a
// second layer on top of whatever encrypts the volume. Without it,
// value bytes are stored plaintext in the backend and confidentiality
// at rest is the volume's job. The root is a pure function of the
// logical data and the commitment key in BOTH modes, so replicas and
// proofs are unaffected by this choice — but the two modes write
// different value-record bytes, so one store's backend must always be
// opened in the mode that wrote it.
func WithStorageKey(sk [KeySize]byte) Option {
	return func(o *storeOptions) { o.sk = &sk }
}

// update is one deduplicated change: vh non-nil = insert/overwrite,
// nil = delete.
type update struct {
	path Hash
	vh   *Hash
}

type pendingNode struct {
	nk nodeKey
	n  *node
}

// commitAcc is the work accumulated while applying a batch, flushed
// atomically.
type commitAcc struct {
	newVersion uint64
	// pending holds the nodes written this commit (may be revised by
	// leaf collapse), keyed by encoded nodeKey.
	pending map[string]pendingNode
	// stale holds the record keys superseded this commit.
	stale [][]byte
	// insertCTs holds the stored-form value bytes for this batch's
	// inserts, by path; consumed when the insert actually lands as a
	// new leaf.
	insertCTs map[Hash][]byte
	// values holds the value records to write this commit: vh →
	// stored form (all at newVersion).
	values map[Hash][]byte
}

// Create builds a fresh, empty store (version 0, placeholder root).
func Create(backend Backend, commitmentKey [KeySize]byte, opts ...Option) (*Store, error) {
	s, err := newStore(backend, commitmentKey, opts)
	if err != nil {
		return nil, err
	}
	ckpt, err := s.sealCheckpoint(&placeholderHash, 0)
	if err != nil {
		return nil, err
	}
	if err := backend.WriteBatch([]BatchOp{
		{Key: rootRecordKey(0), Value: placeholderHash[:]},
		{Key: checkpointRecordKey(), Value: ckpt},
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// Open opens an existing store at a trusted (root, version) checkpoint
// (e.g. one anchored externally). It verifies the backend actually
// holds that root before returning; fails closed otherwise.
func Open(backend Backend, commitmentKey [KeySize]byte, root Hash, version uint64, opts ...Option) (*Store, error) {
	s, err := newStore(backend, commitmentKey, opts)
	if err != nil {
		return nil, err
	}
	s.root, s.version = root, version
	s.rootChild, err = s.loadRootChild(root, version)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// OpenLatest opens at the checkpoint record the store itself maintains,
// written atomically inside every commit batch and authenticated:
// AES-256-GCM under the storage key when one is configured, otherwise
// an HMAC under a key derived from the commitment key. Storage cannot
// forge it — at worst it can replay an old checkpoint together with a
// matching old store, the documented restart-replay residual. Prefer
// anchoring Root() externally when an extra anchor is available.
func OpenLatest(backend Backend, commitmentKey [KeySize]byte, opts ...Option) (*Store, error) {
	s, err := newStore(backend, commitmentKey, opts)
	if err != nil {
		return nil, err
	}
	record, ok, err := backend.Get(checkpointRecordKey())
	if err != nil {
		return nil, errBackend(err)
	}
	if !ok {
		return nil, errMissing("checkpoint record")
	}
	root, version, err := s.openCheckpoint(record)
	if err != nil {
		return nil, err
	}
	s.root, s.version = root, version
	s.rootChild, err = s.loadRootChild(root, version)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// OpenOrCreate opens an existing store, or creates a fresh one if no
// checkpoint exists yet.
func OpenOrCreate(backend Backend, commitmentKey [KeySize]byte, opts ...Option) (*Store, error) {
	_, ok, err := backend.Get(checkpointRecordKey())
	if err != nil {
		return nil, errBackend(err)
	}
	if ok {
		return OpenLatest(backend, commitmentKey, opts...)
	}
	return Create(backend, commitmentKey, opts...)
}

func newStore(backend Backend, commitmentKey [KeySize]byte, opts []Option) (*Store, error) {
	var o storeOptions
	for _, opt := range opts {
		opt(&o)
	}
	s := &Store{
		backend: backend,
		ck:      append([]byte(nil), commitmentKey[:]...),
		root:    placeholderHash,
		version: 0,
		cache:   newNodeCache(defaultCacheCapacity),
	}
	if o.sk != nil {
		cipher, err := newAEAD(*o.sk)
		if err != nil {
			return nil, err
		}
		s.cipher = cipher
	} else {
		mac := hmac.New(sha256.New, s.ck)
		mac.Write(ckptMACLabel)
		s.ckptMAC = mac.Sum(nil)
	}
	return s, nil
}

// -- value records (mode-dependent at-rest form) --------------------------

// sealValue produces the stored form of a value: ciphertext under the
// storage key when configured, the plaintext bytes otherwise. Either
// way the read path re-derives the keyed commitment, so integrity does
// not depend on this layer.
func (s *Store) sealValue(path *Hash, plaintext []byte) ([]byte, error) {
	if s.cipher != nil {
		return s.cipher.encrypt(plaintext, valueAAD(path))
	}
	return append([]byte(nil), plaintext...), nil
}

// openValue recovers the plaintext from a stored value record.
func (s *Store) openValue(path *Hash, record []byte) ([]byte, error) {
	if s.cipher != nil {
		pt, err := s.cipher.decrypt(record, valueAAD(path))
		if err != nil {
			return nil, errCorruptedf("value decrypt: %v", err)
		}
		return pt, nil
	}
	return record, nil
}

// -- checkpoint (mode-dependent authentication) ---------------------------

func (s *Store) sealCheckpoint(root *Hash, version uint64) ([]byte, error) {
	payload := make([]byte, 0, HashSize+8)
	payload = append(payload, root[:]...)
	payload = binary.BigEndian.AppendUint64(payload, version)
	if s.cipher != nil {
		return s.cipher.encrypt(payload, checkpointAAD)
	}
	mac := hmac.New(sha256.New, s.ckptMAC)
	mac.Write(payload)
	return mac.Sum(payload), nil
}

func (s *Store) openCheckpoint(record []byte) (Hash, uint64, error) {
	var payload []byte
	if s.cipher != nil {
		pt, err := s.cipher.decrypt(record, checkpointAAD)
		if err != nil {
			return Hash{}, 0, errCorruptedf("checkpoint decrypt: %v", err)
		}
		payload = pt
	} else {
		if len(record) != HashSize+8+sha256.Size {
			return Hash{}, 0, errCorrupted("checkpoint bad length")
		}
		payload = record[:HashSize+8]
		mac := hmac.New(sha256.New, s.ckptMAC)
		mac.Write(payload)
		if !hmac.Equal(mac.Sum(nil), record[HashSize+8:]) {
			return Hash{}, 0, errCorrupted("checkpoint authentication failed")
		}
	}
	if len(payload) != HashSize+8 {
		return Hash{}, 0, errCorrupted("checkpoint bad length")
	}
	var root Hash
	copy(root[:], payload[:HashSize])
	return root, binary.BigEndian.Uint64(payload[HashSize:]), nil
}

// Root returns the current (root, version). Anchor this pair externally
// after every commit when an anchor is available.
func (s *Store) Root() (Hash, uint64) { return s.root, s.version }

// Backend exposes the underlying backend (tests, diagnostics).
func (s *Store) Backend() Backend { return s.backend }

// SetCacheCapacity resizes the node cache (0 disables). Clears current
// contents.
func (s *Store) SetCacheCapacity(capacity int) { s.cache.setCapacity(capacity) }

// -- reads ----------------------------------------------------------------

// Get returns the value for key at the current version. ok reports
// whether the key is present (an empty value is a value).
func (s *Store) Get(key []byte) (value []byte, ok bool, err error) {
	path := s.pathOf(key)
	return s.walk(s.rootChild, &path)
}

// RootAt returns the root hash recorded for a historical version (same
// freshness caveat as GetAt).
func (s *Store) RootAt(version uint64) (Hash, error) {
	if version > s.version {
		return Hash{}, errInvalidf("version %d is in the future (current %d)", version, s.version)
	}
	return s.loadRootRecord(version)
}

// GetAt returns the value for key at a historical version.
//
// Content is authenticated against the stored root record for that
// version; the version→root binding for history is backend-held, so
// history is strongest for roots the caller pinned externally.
func (s *Store) GetAt(version uint64, key []byte) (value []byte, ok bool, err error) {
	if version > s.version {
		return nil, false, errInvalidf("version %d is in the future (current %d)", version, s.version)
	}
	root, err := s.loadRootRecord(version)
	if err != nil {
		return nil, false, err
	}
	rootChild, err := s.loadRootChild(root, version)
	if err != nil {
		return nil, false, err
	}
	path := s.pathOf(key)
	return s.walk(rootChild, &path)
}

// -- writes ---------------------------------------------------------------

// computeBatch is the pure computation shared by PutBatch and
// PreviewBatch: dedupe, commit/encrypt values, fold the updates into
// the tree. Returns nil for a no-op batch. Never mutates the store —
// all effects live in the returned accumulator.
func (s *Store) computeBatch(ops []Op) (*commitAcc, *child, Hash, error) {
	newVersion := s.version + 1

	// Deduplicate (last wins), then sort by path.
	deduped := make(map[Hash]*[]byte, len(ops))
	for i := range ops {
		path := s.pathOf(ops[i].Key)
		if ops[i].Delete {
			deduped[path] = nil
		} else {
			v := ops[i].Value
			deduped[path] = &v
		}
	}
	if len(deduped) == 0 {
		return nil, nil, Hash{}, nil
	}

	acc := &commitAcc{
		newVersion: newVersion,
		pending:    make(map[string]pendingNode),
		insertCTs:  make(map[Hash][]byte),
		values:     make(map[Hash][]byte),
	}

	// Precompute commitments and ciphertexts for inserts. Value records
	// are only written when an insert actually lands as a new leaf (see
	// putLeaf), so no-op overwrites leave no orphaned records behind.
	updates := make([]update, 0, len(deduped))
	for path, value := range deduped {
		if value == nil {
			updates = append(updates, update{path: path})
			continue
		}
		vh := s.vhOf(&path, *value)
		ct, err := s.sealValue(&path, *value)
		if err != nil {
			return nil, nil, Hash{}, err
		}
		acc.insertCTs[path] = ct
		vhCopy := vh
		updates = append(updates, update{path: path, vh: &vhCopy})
	}
	sort.Slice(updates, func(i, j int) bool {
		return bytes.Compare(updates[i].path[:], updates[j].path[:]) < 0
	})

	prefix := make([]uint8, 0, nibbles)
	newRootChild, err := s.applySubtree(acc, s.rootChild, &prefix, updates)
	if err != nil {
		return nil, nil, Hash{}, err
	}

	if childEq(newRootChild, s.rootChild) {
		return nil, nil, Hash{}, nil // no-op batch
	}

	newRoot := placeholderHash
	if newRootChild != nil {
		newRoot = newRootChild.hash
	}
	return acc, newRootChild, newRoot, nil
}

// PreviewBatch computes the (root, version) this batch WOULD produce,
// without committing anything. The tree math is pure, so a later
// PutBatch with the same ops from the same state produces exactly this
// result. This is what a transaction Fork uses to seal rootAfter.
func (s *Store) PreviewBatch(ops []Op) (Hash, uint64, error) {
	acc, _, newRoot, err := s.computeBatch(ops)
	if err != nil {
		return Hash{}, 0, err
	}
	if acc == nil {
		return s.root, s.version, nil
	}
	return newRoot, s.version + 1, nil
}

// PutBatch applies a batch of operations as one commit. Later ops win
// over earlier ops on the same key. Returns the new (root, version).
//
// If the batch changes nothing (deletes of absent keys, overwrites
// with identical values), no commit happens and the current
// (root, version) is returned unchanged.
func (s *Store) PutBatch(ops []Op) (Hash, uint64, error) {
	acc, newRootChild, newRoot, err := s.computeBatch(ops)
	if err != nil {
		return Hash{}, 0, err
	}
	if acc == nil {
		return s.root, s.version, nil
	}
	return s.commitAcc(acc, newRootChild, newRoot)
}

// commitAcc is the atomic tail shared by every commit path: write the
// batch, then swing the in-memory state and warm the cache.
func (s *Store) commitAcc(acc *commitAcc, newRootChild *child, newRoot Hash) (Hash, uint64, error) {
	newVersion := acc.newVersion

	batch := make([]BatchOp, 0, len(acc.pending)+len(acc.values)+len(acc.stale)+2)
	for _, pn := range acc.pending {
		batch = append(batch, BatchOp{Key: nodeRecordKey(&pn.nk), Value: pn.n.encode()})
	}
	for vh, ct := range acc.values {
		vhCopy := vh
		batch = append(batch, BatchOp{Key: valueRecordKey(&vhCopy, newVersion), Value: ct})
	}
	for _, target := range acc.stale {
		batch = append(batch, BatchOp{Key: staleRecordKey(newVersion, target)})
	}
	batch = append(batch, BatchOp{Key: rootRecordKey(newVersion), Value: newRoot[:]})
	ckpt, err := s.sealCheckpoint(&newRoot, newVersion)
	if err != nil {
		return Hash{}, 0, err
	}
	batch = append(batch, BatchOp{Key: checkpointRecordKey(), Value: ckpt})

	if err := s.backend.WriteBatch(batch); err != nil {
		return Hash{}, 0, errBackend(err)
	}

	// Only after the backend confirmed: swing the in-memory state and
	// warm the cache with this commit's nodes (root and top levels are
	// the hottest records in the store).
	for _, pn := range acc.pending {
		nk := pn.nk
		s.cache.put(&nk, pn.n)
	}
	s.root = newRoot
	s.version = newVersion
	s.rootChild = newRootChild
	return s.root, s.version, nil
}

// -- pruning --------------------------------------------------------------

// Prune deletes storage needed only by versions strictly before
// beforeVersion: stale records that died at or before it, and root
// records below it. Afterwards GetAt/ProveAt keep working for every
// version >= beforeVersion and fail with a KindMissing error below it.
// The live tree is never touched — both node and value records are
// versioned and never rewritten, so stale targets are deleted blindly.
//
// Costs are proportional to accumulated garbage, not store size.
// Idempotent; safe to re-run after a partial failure.
func (s *Store) Prune(beforeVersion uint64) (PruneStats, error) {
	// A pinned version (snapshot being served) caps the horizon.
	if s.snapshotPin != nil && *s.snapshotPin < beforeVersion {
		beforeVersion = *s.snapshotPin
	}
	if beforeVersion > s.version {
		return PruneStats{}, errInvalidf(
			"cannot prune to future version %d (current %d)", beforeVersion, s.version)
	}
	var stats PruneStats

	// Deleted history must not linger in the cache.
	s.cache.clear()

	// Stale entries with stale_since <= beforeVersion. Each entry's key
	// embeds the target record key; both die in one batch.
	start := []byte{recStale}
	var end []byte
	if beforeVersion == ^uint64(0) {
		end = []byte{recStale + 1}
	} else {
		end = binary.BigEndian.AppendUint64([]byte{recStale}, beforeVersion+1)
	}
	for {
		entries, err := s.backend.Scan(start, end, pruneChunk)
		if err != nil {
			return stats, errBackend(err)
		}
		if len(entries) == 0 {
			break
		}
		fullChunk := len(entries) == pruneChunk
		batch := make([]BatchOp, 0, len(entries)*2)
		for _, e := range entries {
			if len(e.Key) < 10 {
				return stats, errCorrupted("stale entry key too short")
			}
			batch = append(batch,
				BatchOp{Key: append([]byte(nil), e.Key[9:]...), Delete: true},
				BatchOp{Key: e.Key, Delete: true},
			)
			stats.StaleEntries++
			stats.RecordsDeleted++
		}
		if err := s.backend.WriteBatch(batch); err != nil {
			return stats, errBackend(err)
		}
		if !fullChunk {
			break
		}
	}

	// Root records below the horizon.
	start = []byte{recRoot}
	end = binary.BigEndian.AppendUint64([]byte{recRoot}, beforeVersion)
	for {
		entries, err := s.backend.Scan(start, end, pruneChunk)
		if err != nil {
			return stats, errBackend(err)
		}
		if len(entries) == 0 {
			break
		}
		fullChunk := len(entries) == pruneChunk
		batch := make([]BatchOp, 0, len(entries))
		for _, e := range entries {
			batch = append(batch, BatchOp{Key: e.Key, Delete: true})
			stats.RootRecordsDeleted++
		}
		if err := s.backend.WriteBatch(batch); err != nil {
			return stats, errBackend(err)
		}
		if !fullChunk {
			break
		}
	}

	return stats, nil
}

// RetainRecent prunes so that (at least) the last window versions stay
// readable: Prune(version - window), clamped at zero.
func (s *Store) RetainRecent(window uint64) (PruneStats, error) {
	if window > s.version {
		return s.Prune(0)
	}
	return s.Prune(s.version - window)
}

// -- internals: commitments -----------------------------------------------

func (s *Store) pathOf(key []byte) Hash {
	mac := hmac.New(sha256.New, s.ck)
	mac.Write(hmacTagPath)
	mac.Write(key)
	var out Hash
	copy(out[:], mac.Sum(nil))
	return out
}

func (s *Store) vhOf(path *Hash, plaintext []byte) Hash {
	mac := hmac.New(sha256.New, s.ck)
	mac.Write(hmacTagValue)
	mac.Write(path[:])
	mac.Write(plaintext)
	var out Hash
	copy(out[:], mac.Sum(nil))
	return out
}

// -- internals: loading ---------------------------------------------------

func (s *Store) loadNode(nk *nodeKey, expectedHash *Hash) (*node, error) {
	// The cache removes backend I/O only — cached nodes are re-verified
	// against the caller's expected hash exactly like loaded ones.
	if n := s.cache.get(nk); n != nil {
		if n.hash() != *expectedHash {
			return nil, errCorruptedf("node %v hash mismatch", nk)
		}
		return n, nil
	}
	record, ok, err := s.backend.Get(nodeRecordKey(nk))
	if err != nil {
		return nil, errBackend(err)
	}
	if !ok {
		return nil, errMissingf("node %v", nk)
	}
	n, err := decodeNode(record)
	if err != nil {
		return nil, err
	}
	if n.hash() != *expectedHash {
		return nil, errCorruptedf("node %v hash mismatch", nk)
	}
	nkCopy := nodeKey{version: nk.version, prefix: append([]uint8(nil), nk.prefix...)}
	s.cache.put(&nkCopy, n)
	return n, nil
}

func (s *Store) loadRootRecord(version uint64) (Hash, error) {
	rec, ok, err := s.backend.Get(rootRecordKey(version))
	if err != nil {
		return Hash{}, errBackend(err)
	}
	if !ok {
		return Hash{}, errMissingf("root record v%d", version)
	}
	if len(rec) != HashSize {
		return Hash{}, errCorruptedf("root record v%d bad length", version)
	}
	var out Hash
	copy(out[:], rec)
	return out, nil
}

// loadRootChild resolves the root child for a (root, version) pair,
// verifying the root node exists and hashes correctly. nil for the
// empty tree.
func (s *Store) loadRootChild(root Hash, version uint64) (*child, error) {
	if root == placeholderHash {
		return nil, nil
	}
	nk := rootNodeKey(version)
	n, err := s.loadNode(&nk, &root)
	if err != nil {
		return nil, err
	}
	return &child{version: version, hash: root, isLeaf: n.isLeaf()}, nil
}

func (s *Store) readValue(path, vh *Hash, valueVersion uint64) ([]byte, error) {
	ct, ok, err := s.backend.Get(valueRecordKey(vh, valueVersion))
	if err != nil {
		return nil, errBackend(err)
	}
	if !ok {
		return nil, errMissing("value record")
	}
	pt, err := s.openValue(path, ct)
	if err != nil {
		return nil, err
	}
	if s.vhOf(path, pt) != *vh {
		return nil, errCorrupted("value commitment mismatch")
	}
	return pt, nil
}

func (s *Store) walk(rootChild *child, path *Hash) ([]byte, bool, error) {
	c := rootChild
	if c == nil {
		return nil, false, nil
	}
	prefix := make([]uint8, 0, nibbles)
	for {
		nk := nodeKey{version: c.version, prefix: prefix}
		n, err := s.loadNode(&nk, &c.hash)
		if err != nil {
			return nil, false, err
		}
		if n.isLeaf() != c.isLeaf {
			return nil, false, errCorruptedf("node %v kind mismatch", &nk)
		}
		if l := n.leaf; l != nil {
			if l.path == *path {
				v, err := s.readValue(path, &l.vh, l.valueVersion)
				if err != nil {
					return nil, false, err
				}
				return v, true, nil
			}
			return nil, false, nil
		}
		nib := nibble(path, len(prefix))
		next := n.internal.children[nib]
		if next == nil {
			return nil, false, nil
		}
		prefix = append(prefix, nib)
		c = next
	}
}

// -- internals: batch apply -----------------------------------------------

// childEq compares two child references by value (both nil = equal).
func childEq(a, b *child) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.version == b.version && a.hash == b.hash && a.isLeaf == b.isLeaf
}

// applySubtree applies the (sorted) updates that fall inside the
// subtree at prefix, returning the subtree's new child reference (nil =
// empty). Returns old untouched when nothing changed.
func (s *Store) applySubtree(acc *commitAcc, old *child, prefix *[]uint8, updates []update) (*child, error) {
	if len(updates) == 0 {
		return old, nil
	}

	if old == nil {
		// Empty subtree: only inserts materialise anything.
		pairs := make([]leafNode, 0, len(updates))
		for _, u := range updates {
			if u.vh != nil {
				pairs = append(pairs, leafNode{path: u.path, vh: *u.vh, valueVersion: acc.newVersion})
			}
		}
		return s.buildFromPairs(acc, prefix, pairs)
	}

	nk := nodeKey{version: old.version, prefix: append([]uint8(nil), *prefix...)}
	n, err := s.loadNode(&nk, &old.hash)
	if err != nil {
		return nil, err
	}
	if n.isLeaf() != old.isLeaf {
		return nil, errCorruptedf("node %v kind mismatch", &nk)
	}

	if leaf := n.leaf; leaf != nil {
		// Merge the resident leaf with the updates. Entries are
		// (vh, valueVersion): an overwrite with the identical value
		// keeps the old record, anything else routes to a record
		// written this commit.
		type entry struct {
			vh Hash
			vv uint64
		}
		merged := map[Hash]entry{leaf.path: {leaf.vh, leaf.valueVersion}}
		for _, u := range updates {
			if u.vh != nil {
				vv := acc.newVersion
				if u.path == leaf.path && *u.vh == leaf.vh {
					vv = leaf.valueVersion
				}
				merged[u.path] = entry{*u.vh, vv}
			} else {
				delete(merged, u.path)
			}
		}
		if len(merged) == 1 {
			if e, ok := merged[leaf.path]; ok && e.vh == leaf.vh && e.vv == leaf.valueVersion {
				return old, nil // nothing changed
			}
		}
		// The resident leaf node is superseded in every changed case.
		acc.stale = append(acc.stale, nodeRecordKey(&nk))
		if e, ok := merged[leaf.path]; !ok || e.vh != leaf.vh {
			acc.stale = append(acc.stale, valueRecordKey(&leaf.vh, leaf.valueVersion))
		}
		pairs := make([]leafNode, 0, len(merged))
		for p, e := range merged {
			pairs = append(pairs, leafNode{path: p, vh: e.vh, valueVersion: e.vv})
		}
		sort.Slice(pairs, func(i, j int) bool {
			return bytes.Compare(pairs[i].path[:], pairs[j].path[:]) < 0
		})
		return s.buildFromPairs(acc, prefix, pairs)
	}

	depth := len(*prefix)
	children := n.internal.children // array copy
	changed := false

	// Updates are sorted by path, so nibble groups are contiguous slices.
	for i := 0; i < len(updates); {
		nib := nibble(&updates[i].path, depth)
		j := i + 1
		for j < len(updates) && nibble(&updates[j].path, depth) == nib {
			j++
		}
		*prefix = append(*prefix, nib)
		newChild, err := s.applySubtree(acc, children[nib], prefix, updates[i:j])
		*prefix = (*prefix)[:len(*prefix)-1]
		if err != nil {
			return nil, err
		}
		if !childEq(newChild, children[nib]) {
			children[nib] = newChild
			changed = true
		}
		i = j
	}

	if !changed {
		return old, nil
	}
	acc.stale = append(acc.stale, nodeRecordKey(&nk))

	var only *child
	present := 0
	for _, c := range children {
		if c != nil {
			present++
			if present == 1 {
				only = c
			} else {
				break
			}
		}
	}
	switch {
	case present == 0:
		return nil, nil // subtree emptied
	case present == 1 && only.isLeaf:
		// Collapse: the lone surviving leaf rises here.
		var nib uint8
		for i, c := range children {
			if c != nil {
				nib = uint8(i)
				break
			}
		}
		*prefix = append(*prefix, nib)
		leaf, err := s.takeLeaf(acc, only, *prefix)
		*prefix = (*prefix)[:len(*prefix)-1]
		if err != nil {
			return nil, err
		}
		return s.putLeaf(acc, *prefix, leaf)
	default:
		newNode := &node{internal: &internalNode{children: children}}
		h := newNode.hash()
		nkNew := nodeKey{version: acc.newVersion, prefix: append([]uint8(nil), *prefix...)}
		acc.pending[string(nkNew.encode())] = pendingNode{nk: nkNew, n: newNode}
		return &child{version: acc.newVersion, hash: h, isLeaf: false}, nil
	}
}

// buildFromPairs builds a fresh subtree at prefix from sorted leaves.
func (s *Store) buildFromPairs(acc *commitAcc, prefix *[]uint8, pairs []leafNode) (*child, error) {
	switch len(pairs) {
	case 0:
		return nil, nil
	case 1:
		return s.putLeaf(acc, *prefix, pairs[0])
	default:
		depth := len(*prefix)
		var children [16]*child
		for i := 0; i < len(pairs); {
			nib := nibble(&pairs[i].path, depth)
			j := i + 1
			for j < len(pairs) && nibble(&pairs[j].path, depth) == nib {
				j++
			}
			*prefix = append(*prefix, nib)
			c, err := s.buildFromPairs(acc, prefix, pairs[i:j])
			*prefix = (*prefix)[:len(*prefix)-1]
			if err != nil {
				return nil, err
			}
			children[nib] = c
			i = j
		}
		n := &node{internal: &internalNode{children: children}}
		h := n.hash()
		nk := nodeKey{version: acc.newVersion, prefix: append([]uint8(nil), *prefix...)}
		acc.pending[string(nk.encode())] = pendingNode{nk: nk, n: n}
		return &child{version: acc.newVersion, hash: h, isLeaf: false}, nil
	}
}

// putLeaf stores a leaf node at prefix (new version) and returns its
// child ref. A leaf whose value was (re)written this commit also emits
// its value record.
func (s *Store) putLeaf(acc *commitAcc, prefix []uint8, leaf leafNode) (*child, error) {
	if leaf.valueVersion == acc.newVersion {
		if _, done := acc.values[leaf.vh]; !done {
			ct, ok := acc.insertCTs[leaf.path]
			if !ok {
				return nil, errCorrupted("internal: landed insert without ciphertext")
			}
			acc.values[leaf.vh] = ct
		}
	}
	l := leaf
	n := &node{leaf: &l}
	h := n.hash()
	nk := nodeKey{version: acc.newVersion, prefix: append([]uint8(nil), prefix...)}
	acc.pending[string(nk.encode())] = pendingNode{nk: nk, n: n}
	return &child{version: acc.newVersion, hash: h, isLeaf: true}, nil
}

// takeLeaf fetches the content of a leaf child at prefix so it can be
// re-homed (collapse). If it was written this very commit it is removed
// from the pending set (never persisted); otherwise its stored record
// is marked stale.
func (s *Store) takeLeaf(acc *commitAcc, c *child, prefix []uint8) (leafNode, error) {
	nk := nodeKey{version: c.version, prefix: append([]uint8(nil), prefix...)}
	if c.version == acc.newVersion {
		k := string(nk.encode())
		pn, ok := acc.pending[k]
		if !ok || pn.n.leaf == nil {
			return leafNode{}, errCorruptedf("pending leaf %v missing or wrong kind", &nk)
		}
		delete(acc.pending, k)
		return *pn.n.leaf, nil
	}
	n, err := s.loadNode(&nk, &c.hash)
	if err != nil {
		return leafNode{}, err
	}
	if n.leaf == nil {
		return leafNode{}, errCorruptedf("node %v kind mismatch", &nk)
	}
	acc.stale = append(acc.stale, nodeRecordKey(&nk))
	return *n.leaf, nil
}
