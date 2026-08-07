# Storage durability contract

Phase 2 implements the `raft.StableStore` port as a local, single-writer durable
store. The Raft package owns persistence types; `internal/storage` owns the disk
format and recovery adapter so the consensus core remains independent of the
filesystem.

## Commit boundary

- A persistence batch contains hard state, an optional suffix truncation, and
  appended entries in one checksummed WAL record.
- `Persist` returns success only after the complete record has been written and
  the WAL has been synchronized.
- A write or synchronization error makes the open store unhealthy. It rejects
  subsequent operations until the process closes and reopens it for recovery.
- Inputs are validated before any bytes are appended. Terms and commit indexes
  cannot decrease, entries must be contiguous, committed entries cannot be
  truncated, and commands use the public contract size limit.

## Recovery policy

The WAL and snapshot formats are explicitly versioned and use CRC-32C
checksums. Recovery replays complete records in order and validates the same
state invariants used for live writes.

- A final record with a complete, valid checksummed header but an incomplete
  declared payload is truncated to the last complete record and synchronized.
  Header fragments are not provable interrupted writes and fail closed.
- A checksum mismatch, invalid complete record, unsupported version, missing
  snapshot for a checkpoint, or inconsistent snapshot/log state fails closed
  with `storage.ErrCorrupt`.
- Corruption is never silently skipped and an interior record is never
  truncated as though it were a partial tail.

## Snapshots and compaction

Snapshots contain an index, term, opaque state-machine image, format version,
and checksum. Activation writes a same-directory temporary file, synchronizes
it, renames it atomically, and synchronizes the directory. The WAL is then
replaced with a synchronized checkpoint containing retained entries after the
snapshot index.

Recovery supports the crash window in which the new snapshot is active but the
old WAL has not yet been compacted. The snapshot is verified against committed
log history before covered entries are discarded.

## Operational assumptions

- The store directory is provisioned before `storage.Open` is called. This
  makes durable creation and parent-directory synchronization an explicit
  deployment responsibility; the storage module never acknowledges data under
  a newly created, unanchored directory entry.
- One process owns a store directory; cross-process file locking is outside the
  Phase 2 scope and must be enforced by deployment orchestration.
- The filesystem must provide conventional `fsync` and same-directory atomic
  rename semantics.
- Snapshot payload generation and installation belong to the state-machine and
  Raft integration phases; this module persists and verifies opaque payloads.
