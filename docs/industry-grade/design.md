# Viper Industry-Grade Baseline

## What and why

Viper currently describes an educational Raft demonstration. An industry-grade
system must make stronger guarantees explicit and prove them operationally: safe
membership, durable recovery, authenticated traffic, bounded resource usage,
observable behavior, upgrade compatibility, and repeatable failure testing.

The recommended target is a production-quality single-region distributed KV
service for trusted internal workloads, with a documented API and operational
contract. It should be a focused system rather than an attempt to replace etcd,
Consul, or CockroachDB. The linearizability checker remains a verification and
test tool, not a runtime dependency or a claim that arbitrary production
histories can be exhaustively checked.

## Requirements

- Implement Raft with randomized elections, current-term commit restriction,
  election restriction, durable state-before-ack, ordered application, and
  single-writer leadership semantics.
- Support static clusters first; add joint-consensus membership changes only
  after the static implementation is proven. A cluster must never change
  membership by editing configuration on one node.
- Provide durable WAL/log storage with checksummed records, corruption
  detection, atomic metadata updates, recovery replay, and crash-consistency
  tests.
- Provide snapshots and log compaction before claiming long-running operation.
  Snapshot installation must be resumable and must not lose committed data.
- Define linearizable reads explicitly. `Get(consistent=true)` must use a
  ReadIndex or a no-op entry; follower reads must be explicitly labeled stale.
- Make Put and CAS retry-safe with client request IDs and a deduplication
  record that survives restart. The API must document retry and timeout
  semantics.
- Validate all RPC fields, enforce maximum key/value/request sizes, bound
  queues and concurrency, and return stable typed errors.
- Authenticate and authorize clients and peer traffic. Use TLS with peer
  identity verification and least-privilege service credentials.
- Expose health, readiness, leader, term, commit index, applied index, storage,
  replication, and request metrics without exposing values or credentials.
- Emit structured, redacted logs with correlation IDs. Never log request
  bodies, values, tokens, cookies, or database credentials.
- Support graceful shutdown, restart, rolling upgrade compatibility, backup,
  restore, timeouts, cancellation, and clear operator runbooks. The current
  release uses a maintenance upgrade boundary until mixed-version tests exist.
- Define an availability and durability SLO, capacity limits, supported
  topology, backup RPO/RTO, and upgrade policy before production release.
- Keep the checker independent from Raft and KV implementation packages. Use it
  on bounded histories and complement it with model-based, property-based,
  fault-injection, and Jepsen-style tests.

## Acceptance criteria

- A three- or five-node cluster survives any single-node crash without loss of
  acknowledged writes and elects a leader within the published failure
  detection target.
- A minority partition cannot acknowledge a write. A majority partition can
  make progress, and healing converges without divergent committed state.
- Restart recovery passes injected crash points around every durable write,
  replay, snapshot, and acknowledgment boundary.
- No committed entry is overwritten; election and commit safety properties are
  checked by invariant tests and randomized fault schedules.
- Consistent reads never return a value older than a completed acknowledged
  write. Stale reads identify their consistency mode in the API contract.
- Replaying the same request ID after timeout returns the original result and
  does not apply the command twice, including after leader failover.
- Malformed, oversized, unauthenticated, unauthorized, and rate-exceeding
  requests are rejected with bounded resource consumption.
- A node with a corrupted WAL or snapshot fails closed, reports a diagnostic
  health state, and does not silently serve data.
- Snapshot restore, backup verification, rolling upgrade, and rollback are
  tested in CI or a repeatable staging environment.
- CI passes formatting, static analysis, unit/integration tests, race tests,
  fuzz tests for parsers and RPC boundaries, dependency scanning, secret
  scanning, and container/image vulnerability checks.
- Published performance results include workload, topology, hardware, payload
  sizes, percentile latency, throughput, and failure behavior; no undocumented
  performance target is treated as a guarantee.

## Design

### Service boundaries

Each node contains these explicit layers:

1. API/authentication layer: gRPC and optional HTTP gateway, request limits,
   authentication, authorization, deadlines, request IDs, and typed errors.
2. Proposal layer: leader-only mutation admission, idempotency lookup, and
   proposal lifecycle tracking.
3. Raft core: deterministic state transitions driven by an event loop or
   carefully isolated locks; transport and storage are injected interfaces.
4. Apply layer: ordered, durable application of committed commands with
   deduplication state updated atomically with the state-machine mutation.
5. Storage layer: WAL/log, metadata, snapshots, checksums, fsync policy, and
   recovery.
6. Operations layer: metrics, structured logs, tracing hooks, health endpoints,
   admin authorization, and diagnostics.

The checker is a separate module and executable. It consumes a versioned history
format and an abstract KV model; it must not import node, storage, or service
packages.

### Consistency model

Mutations and `consistent=true` reads are linearizable. A follower may serve a
stale read only through an explicitly stale API or option. The API must not use
an ambiguous boolean as the only indication of consistency; expose a named
consistency mode and return the observed revision/index where useful.

### Storage and recovery

Use a versioned record format with length, type, term/index metadata, payload,
and checksum. Write and sync the record before acknowledging replication. On
startup, validate records, truncate only a provably incomplete tail, and fail
closed on interior corruption. Snapshots include a snapshot index/term,
application state, deduplication state, and format version. Restore is atomic
from the service's perspective and is verified before activation.

### Membership and upgrades

The first release supports a statically configured cluster and rejects peer-set
changes at runtime. The production roadmap then adds Raft joint consensus,
learner nodes, safe removal, quorum checks, and operator confirmation. Wire
protocols and persisted formats are versioned; rolling upgrades require mixed
version compatibility tests and an explicit downgrade boundary.

### Security and privacy

Peer and client connections use TLS. Peer certificates identify node IDs and
are authorized against the configured cluster identity. Clients authenticate
with the selected deployment identity provider, and authorization distinguishes
data operations from administrative actions. Secrets come from environment
injection or a secret manager; local `.env` files are ignored and an
`.env.example` contains placeholders only. Data and histories are treated as
sensitive by default, with redaction and retention guidance.

### Deployment

Docker Compose remains a local demo profile. Production deployment requires a
supported orchestrator profile with persistent volumes, anti-affinity, resource
requests/limits, PodDisruptionBudgets, restricted network policy, encrypted
storage, secret injection, upgrade strategy, and backup integration. A single
host five-container setup is not described as highly available.

## Interfaces and data

- Version all public protobuf packages and reserve removed field numbers.
- Use explicit typed errors such as `NotLeader`, `Unavailable`, `DeadlineExceeded`,
  `PermissionDenied`, `InvalidArgument`, and `DataCorruption`.
- Mutation requests include a client-supplied idempotency/request ID; responses
  include the applied revision and leader hint where safe.
- Health separates liveness, readiness, serving status, and quorum ability.
- Metrics include request totals/latencies by safe labels, proposal failures,
  election changes, replication lag, WAL fsync latency, snapshot age, and disk
  pressure. Never use unbounded keys, values, or client IDs as metric labels.
- History files have a schema version, operation IDs, invocation/completion
  timestamps, consistency mode, result class, and redacted metadata.

## Failure behavior

- On leadership loss, in-flight proposals return an ambiguous outcome unless a
  request ID allows the client to safely resolve the result.
- On quorum loss, mutations fail with `Unavailable`; the node does not fake
  success or continue committing independently.
- On disk-full, fsync failure, checksum mismatch, or unsafe recovery condition,
  the node becomes unready and stops acknowledging writes.
- On invalid or oversized input, reject before allocation-heavy processing.
- On shutdown, stop admission, drain bounded work, persist required state, and
  close listeners within a documented deadline.
- On snapshot or restore failure, preserve the last known-good state and emit a
  diagnostic suitable for an operator runbook.

## Test approach

- Unit-test every Raft transition and safety invariant with deterministic clocks
  and transports.
- Use model-based tests to compare the replicated KV service with a reference
  sequential model.
- Add property/fuzz tests for protobuf decoding, history parsing, WAL records,
  snapshot metadata, and client retry behavior.
- Run deterministic fault schedules for partitions, delay, duplication,
  reordering, crashes, disk errors, and clock-jump simulation.
- Run multi-node integration tests with real persistence and TLS enabled.
- Run bounded linearizability checks on generated histories and retain
  counterexamples as regression fixtures.
- Test backup/restore, rolling upgrades, key rotation, authorization, resource
  limits, and observability redaction.
- Use `go test -race`, `go vet`, a pinned linter, coverage thresholds, dependency
  and container scanning, and secret scanning in CI.

## Risks

- **Scope risk:** Production distributed storage is substantially larger than
  the current educational plan. Mitigation: release a narrow, explicit baseline
  and publish unsupported features.
- **Checker scalability:** Exhaustive linearizability search does not scale to
  arbitrary production histories. Mitigation: bounded histories plus model
  checking and randomized testing.
- **Operational risk:** A local Docker cluster cannot prove production HA.
  Mitigation: separate demo, staging, and production deployment profiles.
- **Data-loss risk:** Incorrect retry handling can duplicate or lose writes.
  Mitigation: durable request IDs, result retention, and ambiguous-outcome
  documentation.
- **Security risk:** Plaintext gRPC and unauthenticated admin actions are not
  acceptable outside a trusted lab. Mitigation: TLS, identity, authorization,
  redaction, and secure defaults.
- **Compatibility risk:** Changing log or protobuf formats can make upgrades
  unsafe. Mitigation: versioned formats and mixed-version CI tests.

## Out of scope

- Multi-region synchronous consensus and global transactions.
- Byzantine fault tolerance.
- Arbitrary SQL/query processing, secondary indexes, or cross-key transactions.
- Claiming compatibility with etcd or another product without conformance work.
- Treating the current 13-section proposal as an implementation plan or proof
  of production readiness.
