# Viper Codex Development Plan

This is the project-specific implementation roadmap and model-allocation
policy for Viper. It is intended to be read before any build or code change.
The root AGENTS.md makes that requirement explicit for Codex.

The plan is based on the current repository, the industry-grade design, and
the storage durability contract. It turns the original six-phase proposal into
smaller vertical delivery phases so that Raft correctness, durable recovery,
security, and verification each receive an explicit review gate.

Status: active
Last reviewed: 2026-08-10

## Current execution override

The user has explicitly directed Codex to complete the remaining project work
with the currently active model. Do not pause for a model handoff or involve
Sol high during this execution. The model-allocation labels below remain risk
classification metadata and do not override that explicit task-continuity
instruction. Tests, static analysis, security checks, and written acceptance
gates remain mandatory evidence.

## Current baseline

The repository currently has:

- versioned protobuf contracts and validation rules;
- dependency-light public contracts and bounded error metadata;
- enforced package-boundary checks and local verification commands;
- a deterministic Raft storage port;
- checksummed WAL records, recovery validation, atomic snapshots, and
  checkpoint compaction in internal/storage;
- package boundaries for Raft, transport, state machine, service, security,
  observability, checker, chaos, and test infrastructure.

The substantive implementation now spans the deterministic Raft core, durable
cluster integration, authenticated service wiring, independent verification,
snapshot/backup handling, deterministic faults, and a local runtime. The final
consolidated gate remains the release evidence boundary.

## Latest verified progress

As of 2026-08-10:

- Phase 3 bounded transport work is implemented: static membership, bounded
  in-memory transport, TLS-required gRPC transport, protobuf conversion,
  message validation, checksums, and focused transport fixtures.
- Phase 4 bounded state-machine work is implemented: versioned commands,
  ordered Put/Get/CAS application, request deduplication, conflict handling,
  deterministic snapshots, restore validation, and focused fixtures.
- Phase 5 bounded client work is implemented: typed Put/Get/CAS client methods,
  explicit consistency selection, request validation, and safe gRPC error
  detail mapping.
- The local verification tools are available through `make tools`; `.tools/`
  is ignored because it contains downloaded binaries.
- The consolidated final verification passed on 2026-08-10 with
  `GOCACHE=/tmp/viper-gocache make verify security-check build`: Buf lint and
  contract validation, vet, race tests, build, govulncheck, and Gitleaks all
  passed. Govulncheck reported no vulnerabilities in reachable code; it also
  listed vulnerabilities in imported packages that the code does not call.
- The Raft core, real-storage restart path, static cluster, replicated KV
  service path, authenticated public gRPC constructor, checker, resumable
  snapshots, backup verification/restore, deterministic chaos harness, and
  local CLI runtime are implemented with focused coverage.
- Repository-side production hardening is implemented: single-process store
  ownership, bounded public rate/concurrency limits, private metrics, offline
  backup export/restore, version and authenticated probe commands, pinned CI
  tools, non-root Docker/Compose packaging, a Kubernetes starting template,
  and benchmark smoke coverage.
- The remaining release evidence is environment-dependent: real multi-host
  staging, target hardware performance and soak results, backup/restore and
  upgrade drills, certificate rotation, external image/dependency security
  review, signed artifacts, and production monitoring integration.

## How Codex must use this plan

1. Read AGENTS.md, this document, README.md, and the relevant industry-grade
   design document before starting a task.
2. Select one task from the earliest incomplete phase whose dependencies are
   satisfied. Do not implement a later phase as a shortcut around an earlier
   gate.
3. Before editing, classify the task and note the model that would be preferred.
   This classification sets the recommended model for the task boundary; it
   does not authorize a mid-task model switch.
4. When the user asks Codex to complete a specific in-scope task, complete that
   task end-to-end with the currently active model. Do not pause merely to
   request Sol high or Luna max. Keep the task's written scope, acceptance
   criteria, and checks intact.
5. Under the current execution override, continue with the active model for
   subsequent in-scope tasks. Do not request or imply a Sol high handoff.
6. Stop and surface the issue when the task requires an unresolved product or
   protocol choice, contradicts a documented invariant, or expands beyond its
   phase. Do not hide the decision in code.
7. Run the task checks and the phase gate. A model's confidence is not
   evidence of correctness.
8. Update phase status only after the acceptance criteria pass. Keep
   unsupported features and production claims explicit.

## Model allocation policy

The allocation is a risk and workflow policy for this repository, not a claim
that one model can replace engineering review. These classifications recommend
the model before a task starts. Once an explicit in-scope task starts, the
task-continuity rule above takes precedence over model preference.

### Sol high

Sol high is the default owner for:

- architecture and dependency decisions;
- Raft elections, term handling, log matching, commitment, leadership, and
  membership behavior;
- concurrency, event-loop or locking design, timing, cancellation, and
  ambiguous outcomes;
- WAL, snapshot, recovery, crash consistency, backup, restore, and upgrade
  semantics;
- public protobuf contracts, wire compatibility, persisted formats, and error
  semantics;
- authentication, authorization, TLS identity, resource limits, redaction,
  and administrative boundaries;
- state-machine atomicity, request deduplication, retry behavior, read
  consistency, and linearizability semantics;
- checker algorithms, reference models, counterexample validity, and fault
  schedules;
- changes spanning packages or changing an existing invariant;
- investigation of failing race, fuzz, chaos, or integration tests;
- final architecture, security, and release reviews.

### Luna max

Luna max may own work only when the contract is already fixed and the task is
bounded, deterministic, and easy to verify in isolation:

- table-driven fixtures and mechanical test cases derived from an approved
  acceptance criterion;
- protobuf/gRPC adapter wiring after the message and error contract is frozen;
- codec plumbing, serialization helpers, and generated-code integration that
  do not change the format;
- metrics, logging, health, CLI, client, Docker Compose, and demo adapters
  after their security and operational behavior is specified;
- documentation, examples, comments, formatting, and narrowly scoped
  refactors;
- benchmark harnesses and result presentation, without inventing performance
  guarantees.

Luna max must escalate to Sol high before changing a public API, persisted
format, Raft invariant, checker oracle, security policy, fault schedule, or
cross-package dependency. A Luna task must include focused tests or an
explicit reason why documentation-only work has no executable check.

### Default routing rule

When choosing between the two:

| Change characteristic | Model |
| --- | --- |
| New invariant or distributed-systems rule | Sol high |
| New API, wire field, storage record, or compatibility rule | Sol high |
| Security, authorization, resource-bound, or redaction decision | Sol high |
| Concurrency, timing, crash, retry, or ambiguous-result behavior | Sol high |
| Checker semantics or fault-injection semantics | Sol high |
| Cross-package or architectural change | Sol high |
| Implementation of an already-approved, single-package adapter | Luna max |
| Mechanical fixtures, docs, formatting, or presentation | Luna max |
| Unclear or mixed-risk task | Sol high |

## Phase status and order

| Phase | Outcome | Starting status | Primary model |
| --- | --- | --- | --- |
| 0. Baseline and contract freeze | Stable contracts, boundaries, and gates | Established; maintain | Sol high |
| 1. Deterministic Raft core | Correct in-memory consensus state machine | Implemented; gate passed 2026-08-10 | Sol high |
| 2. Durable Raft integration | Persist-before-ack and fail-closed recovery | Implemented; gate passed 2026-08-10 | Sol high |
| 3. Peer transport and static cluster | Reproducible 3/5-node cluster runtime | Implemented; gate passed 2026-08-10 | Sol high |
| 4. Replicated KV state machine | Ordered Put/Get/CAS with retry safety | Implemented; gate passed 2026-08-10 | Sol high |
| 5. Client service and read consistency | Safe public API and linearizable reads | Implemented; gate passed 2026-08-10 | Sol high |
| 6. Security and operations | Authenticated, bounded, observable service | Implemented; gate passed 2026-08-10 | Sol high |
| 7. Checker and history tooling | Independent linearizability verification | Implemented; gate passed 2026-08-10 | Sol high |
| 8. Snapshots, backup, and upgrades | Long-running recovery and compatibility | Snapshot/backup implemented; gate passed 2026-08-10; mixed-version upgrades explicitly unsupported | Sol high |
| 9. Chaos and end-to-end verification | Reproducible fault evidence | Implemented; gate passed 2026-08-10 | Sol high |
| 10. Deployment, demo, performance, and release | Documented, tested deliverable | Repository hardening and local profiles implemented; local gate passed; external release evidence pending | Sol high |

The primary-model column records the original risk classification for each
phase. Under the current execution override it does not trigger a model
switch; the active model executes the work and the repository checks provide
the evidence boundary.

## Phase 0 — Baseline and contract freeze

### Outcome

The repository has one authoritative set of contracts, package boundaries,
limits, failure categories, and verification commands.

### Sol high work

- Reconcile the older educational proposal with the industry-grade baseline.
- Freeze the invariants for term safety, current-term commitment, durable
  acknowledgment, ordered application, idempotent mutation, and explicit read
  consistency.
- Review protobuf compatibility rules, size limits, typed errors, and the
  one-way dependency policy.
- Decide whether a proposed feature belongs in the baseline or is explicitly
  out of scope.

### Luna max work

- Maintain documentation links, package comments, contract fixtures, and
  mechanical validation cases after Sol has approved the contract.
- Keep formatting and generated-contract checks reproducible.

### Acceptance gate

- make verify passes.
- Architecture tests reject forbidden imports and undeclared module policy.
- Public and persisted contracts have versioning and compatibility notes.
- No implementation task begins with an unresolved safety or API decision.

### Checks

~~~text
make verify
~~~

### Out of scope

- Implementing Raft behavior.
- Adding a new storage or wire format without a Sol high design decision.

## Phase 1 — Deterministic Raft core

### Outcome

internal/raft provides a deterministic, infrastructure-independent consensus
state machine with testable ports for time, transport, and durable storage.

### Sol high work

- Define the state, events, outputs, and ownership rules for follower,
  candidate, and leader transitions.
- Implement randomized-election behavior behind an injectable deterministic
  clock.
- Implement RequestVote and AppendEntries transitions, log matching,
  conflict handling, election restriction, term advancement, and leader
  replication state.
- Implement the current-term commit restriction and ensure no committed entry
  can be overwritten.
- Define how persistence requests are emitted before an externally visible
  acknowledgment.
- Review every transition against Raft safety invariants.

### Luna max work

- Add table-driven transition fixtures from Sol-approved cases.
- Add deterministic clock and transport test helpers once their interfaces are
  frozen.
- Add focused regression tests for already-specified edge cases without
  changing the state machine.

### Acceptance gate

- Core tests cover elections, stale and newer terms, one-vote-per-term,
  up-to-date-log voting, log mismatch, conflict backtracking, replication,
  current-term commitment, and leadership loss.
- The core has no imports from storage, transport, service, or observability.
- A deterministic 3-node model reaches a leader and commits an entry when a
  majority is available.
- No minority can commit an entry in the model.

### Checks

~~~text
go test -race ./internal/raft ./internal/architecture
go vet ./...
make fmt-check
~~~

### Out of scope

- gRPC, TLS, process orchestration, Docker, and real filesystem I/O.
- Dynamic membership changes.
- The linearizability checker.

## Phase 2 — Durable Raft integration and recovery

### Outcome

Raft uses the existing StableStore safely: hard state and log changes are
durable before acknowledgment, recovery is validated, and storage failures
make the node unsafe to serve.

### Sol high work

- Define the persistence boundary for votes, terms, log suffixes, truncation,
  commit index, and recovery.
- Integrate Raft transitions with storage.Persist without acknowledging
  messages or client proposals before the required sync completes.
- Define unhealthy behavior after write, sync, checksum, disk-full, or recovery
  errors.
- Integrate snapshot metadata with Raft indices and terms.
- Review crash windows around every durable write and acknowledgment boundary.

### Luna max work

- Add approved codec regression fixtures and bounded corruption cases.
- Add test helpers for reopening stores and comparing recovered state.
- Add focused tests for already-specified tail truncation, interior corruption,
  reserved bytes, size limits, and immutable returned state.

### Acceptance gate

- Acknowledged writes survive process restart.
- A failed write or sync prevents subsequent unsafe operations until reopen.
- Incomplete provable WAL tails are handled according to the storage contract;
  interior corruption fails closed.
- Committed entries cannot be truncated.
- Recovery returns isolated state and preserves snapshot/log consistency.

### Checks

~~~text
go test -race ./internal/storage ./internal/raft
go vet ./...
make verify
~~~

### Out of scope

- Cross-process file locking.
- Multi-region durability.
- Production backup orchestration.

## Phase 3 — Peer transport and static cluster runtime

### Outcome

A reproducible 3- or 5-node static cluster can exchange validated peer
messages, elect a leader, replicate entries, and stop progress safely without
quorum.

### Sol high work

- Define peer lifecycle, deadlines, cancellation, backpressure, retry, and
  shutdown behavior.
- Integrate the Raft core with in-memory transport first, then the gRPC
  transport.
- Enforce contracts.MaxRPCMessageBytes, protobuf validation, bounded queues,
  and bounded replication batches at every peer boundary.
- Define static cluster bootstrap and reject unsafe runtime membership edits.
- Review leader hints, stale-term handling, duplicate messages, and lost
  responses.

### Luna max work

- Wire approved protobuf messages to transport adapters.
- Implement deterministic in-memory transport fixtures and test-only network
  delay/drop helpers.
- Add configuration parsing and cluster startup plumbing after the schema and
  safety behavior are fixed.

### Acceptance gate

- A 3- or 5-node local cluster elects one leader within the published target.
- The majority partition can commit; the minority partition cannot acknowledge
  a mutation.
- A leader change does not overwrite a committed entry.
- Oversized, malformed, stale, duplicate, and cancelled peer messages are
  rejected or handled within bounded resources.
- Shutdown stops admission and closes peer work within the documented deadline.

### Checks

~~~text
buf lint
go test -race ./internal/raft ./internal/transport ./internal/architecture
go vet ./...
make proto-check
~~~

### Out of scope

- Dynamic membership and joint consensus.
- Public client authorization; that is Phase 6.
- Multi-region topology.

## Phase 4 — Replicated KV state machine and idempotent mutations

### Outcome

Put, Get, and CompareAndSwap are applied in committed-log order, with durable
request deduplication and retry-safe mutation results.

### Sol high work

- Define the replicated command envelope and its versioning rules.
- Define atomic state-machine apply semantics for the KV image, revision, and
  deduplication result.
- Implement Put and CAS semantics, including absent values, previous values,
  revisions, and conflict behavior.
- Define the retention and snapshot behavior for request IDs and prior
  results.
- Review retry, timeout, leader-failover, and ambiguous-outcome paths.

### Luna max work

- Implement approved command codecs and state-machine helpers.
- Add table-driven Put/CAS model fixtures and deterministic test data.
- Add focused serialization and bounded-input tests after the command contract
  is frozen.

### Acceptance gate

- Committed commands apply exactly once in index order.
- Retrying the same client/request ID returns the original result without
  applying the mutation twice, including after restart and leader failover.
- CAS is atomic and returns the documented previous state.
- State-machine and deduplication state recover together.

### Checks

~~~text
go test -race ./internal/statemachine ./internal/raft ./internal/storage
go vet ./...
make verify
~~~

### Out of scope

- SQL, secondary indexes, transactions across keys, or arbitrary queries.
- Stale reads exposed without an explicit consistency mode.

## Phase 5 — Client service and read consistency

### Outcome

The client-facing service exposes safe typed errors, linearizable mutations,
explicitly stale reads, request deadlines, and leader routing.

### Sol high work

- Define admission, proposal lifecycle, cancellation, and leadership-loss
  behavior.
- Implement linearizable reads with ReadIndex or a no-op entry; never imply
  linearizability for a follower read.
- Define stale-read labeling and returned revision/index metadata.
- Define typed error mapping for NotLeader, Unavailable, deadline exceeded,
  permission denied, corruption, conflicts, and resource exhaustion.
- Review request ownership checks, retry semantics, and bounded concurrency.

### Luna max work

- Implement gRPC handler adapters and client helpers from the approved
  contract.
- Add error-detail serialization, leader-hint formatting, and CLI request
  plumbing.
- Add table-driven request validation cases already specified in protobuf and
  public contracts.

### Acceptance gate

- A completed acknowledged write is visible to every later linearizable read.
- A stale read is explicitly requested and labeled.
- Leadership loss returns the documented ambiguous outcome unless the request
  ID resolves it.
- Invalid and oversized requests are rejected before expensive processing.
- Public errors contain safe metadata and no values, tokens, request bodies, or
  internal stack traces.

### Checks

~~~text
buf lint
go test -race ./internal/service ./internal/statemachine ./internal/raft
go vet ./...
make proto-check
~~~

### Out of scope

- Administrative authorization implementation.
- Observability dashboards and deployment manifests.

## Phase 6 — Security and operations

### Outcome

The service has authenticated client and peer traffic, least-privilege
authorization, safe operational endpoints, bounded resources, redacted
observability, and graceful shutdown.

### Sol high work

- Define TLS certificate identity, peer authorization, client authentication,
  rotation, and failure behavior.
- Define authorization for data operations, peer operations, health, and admin
  endpoints.
- Review CORS or gateway exposure if an HTTP gateway is added.
- Define request, queue, connection, memory, and concurrency limits.
- Define redacted structured logs, safe metric labels, correlation IDs, health
  versus readiness, and disk-pressure behavior.
- Define graceful shutdown, admin-route protection, and diagnostic health
  states.

### Luna max work

- Implement approved middleware, metric collectors, health adapters, and
  redacted log adapters.
- Add configuration validation and documentation for already-decided identity
  and limits.
- Add focused tests for safe field redaction and status formatting.

### Acceptance gate

- Unauthenticated and unauthorized requests are rejected.
- Peer identity is checked against the configured cluster identity.
- Logs and metrics never expose values, credentials, tokens, cookies, request
  bodies, or unbounded user data.
- Rate, size, queue, and concurrency limits bound resource use.
- A corrupted or unhealthy node is not ready to serve data.
- Shutdown drains bounded work and closes listeners within the documented
  deadline.

### Checks

~~~text
go test -race ./internal/security ./internal/observability ./internal/service
go vet ./...
make verify
~~~

### Out of scope

- Multi-tenant billing, external identity-provider product integration, and
  internet-facing deployment.

## Phase 7 — Independent checker and history tooling

### Outcome

The checker independently consumes a versioned, bounded operation history and
reports linearizability with a useful counterexample when the history is
invalid.

### Sol high work

- Define the abstract KV reference model and history semantics.
- Implement and review the Wing-Gong-style search, memoization key, ordering
  rules, and counterexample reconstruction.
- Prove the checker has no imports from Raft, storage, service, or node
  implementation packages.
- Define checker bounds and fail-safe behavior for oversized histories.
- Design valid and deliberately invalid histories, including retry and
  consistency-mode cases.

### Luna max work

- Implement the versioned history parser/serializer after the schema is
  approved.
- Add fixtures, CLI output formatting, stable summaries, and documentation.
- Add bounded generated histories from Sol-approved model cases.

### Acceptance gate

- Valid sequential, concurrent, retry, and failure histories are accepted.
- Invalid histories produce deterministic failure results and a traceable
  counterexample.
- The checker is independent of the service implementation.
- Oversized or malformed histories are rejected with bounded work.

### Checks

~~~text
go test -race ./internal/checker ./pkg/contracts
go vet ./...
make verify
~~~

### Out of scope

- Exhaustively checking unbounded production histories.
- Treating the checker as a runtime dependency of the node.

## Phase 8 — Snapshots, compaction, backup, and upgrades

### Outcome

Long-running nodes can compact logs, install resumable snapshots, restore
verified backups, and participate in a documented rolling compatibility
boundary.

### Sol high work

- Define the snapshot cut, state-machine image, deduplication state, index/term
  metadata, and activation protocol.
- Integrate resumable InstallSnapshot behavior with checksums, offsets,
  cancellation, and stale-term handling.
- Define compaction ordering so no committed or needed entry is lost.
- Define backup verification, restore activation, last-known-good preservation,
  and failure reporting.
- Define wire and persisted-format compatibility for rolling upgrades and the
  downgrade boundary.
- Keep membership static until a separately reviewed joint-consensus design is
  approved.

### Luna max work

- Implement approved chunk transfer plumbing and test fixtures.
- Add snapshot/restore CLI presentation, runbook examples, and compatibility
  matrix formatting.
- Add deterministic test helpers for reopen, compaction, and resumable transfer.

### Acceptance gate

- Snapshot creation and installation preserve committed data and deduplication
  state.
- Interrupted snapshot installation resumes or fails safely without activation
  of an invalid image.
- Compaction recovery handles the active-snapshot/old-WAL crash window.
- Backup restore verifies data before activation and preserves the last known
  good state on failure.
- Mixed-version compatibility and rollback limits are tested and documented.

### Checks

~~~text
go test -race ./internal/storage ./internal/raft ./internal/statemachine ./internal/transport
go vet ./...
make verify
~~~

### Out of scope

- Multi-region synchronous consensus.
- Live membership edits until joint consensus and operator safeguards exist.

## Phase 9 — Chaos and end-to-end verification

### Outcome

Deterministic fault schedules demonstrate safety and recovery under partitions,
delays, duplication, reordering, crashes, disk errors, and clock changes.

### Sol high work

- Define the fault model, reproducibility seed, schedule format, and expected
  invariants.
- Design partition and crash scenarios around majority/minority behavior,
  acknowledged-write durability, leader failover, and healing.
- Define test-only hooks for delay, drop, duplicate, reorder, disk failure, and
  clock movement without weakening production safety checks.
- Define the deliberate unsafe-commit scenario and prove the independent
  checker catches it.
- Review all false-positive and false-negative interpretations.

### Luna max work

- Implement the approved scenario runner, fault adapters, seed handling, and
  result formatting.
- Add scenario fixtures for normal operation, partition, crash/restart,
  snapshot recovery, and checker counterexamples.
- Add reproducible CLI/demo commands and concise failure reports.

### Acceptance gate

- A majority partition progresses and a minority cannot acknowledge a write.
- A leader crash elects a replacement without losing acknowledged writes.
- Healing converges logs and state without overwriting committed entries.
- Corrupt WAL/snapshot and disk failures fail closed.
- The checker accepts safe histories and rejects the deliberately unsafe
  history with a useful counterexample.
- Every failure is reproducible from a recorded seed and schedule.

### Checks

~~~text
go test -race ./internal/chaos ./internal/checker ./internal/raft ./internal/storage ./internal/service
go vet ./...
make verify
~~~

### Out of scope

- Randomized chaos without reproducible seeds.
- Disabling production safety checks to make a scenario pass.

## Phase 10 — Deployment, demo, performance, and release

### Outcome

Viper has a documented local demo, operator runbooks, safe deployment
defaults, repository deployment profiles, benchmark smoke evidence, and a
release gate that supports only claims backed by evidence.

### Sol high work

- Review Docker Compose and any orchestrator profile for persistent volumes,
  non-root execution, resource limits, secret injection, restricted network
  access, and safe shutdown.
- Define supported topology, capacity limits, SLOs, backup RPO/RTO, and
  unsupported production scenarios.
- Review the complete normal, partition, crash, unsafe-commit, recovery, and
  checker demos for correctness and truthful output.
- Review performance methodology, percentile reporting, failure behavior, and
  the final security and architecture checklist.
- Approve the final release or list explicit blockers.

### Luna max work

- Implement approved Compose files, local startup scripts, CLI presentation,
  demo output, examples, and runbook formatting.
- Implement benchmark harness plumbing and report generation without changing
  the workload or inventing targets.
- Polish README and documentation links after the technical review.

### Acceptance gate

- A new user can run the documented local profile and complete the authenticated
  probe after generating local TLS material.
- The local five-container profile is not described as production high
  availability.
- Local microbenchmark results are explicitly labeled as non-SLO evidence.
- A staging release report includes workload, topology, hardware, payload
  sizes, percentile latency, throughput, recovery time, and failure behavior.
- Formatting, static analysis, tests, race tests, protocol checks, dependency
  scanning, and secret scanning pass.
- Security, backup/restore, upgrade, and failure behavior are documented.
- Release claims are made only after the environment-dependent evidence is
  attached and reviewed.

### Checks

The final release check is one consolidated pass after all fixes:

~~~text
make verify
go test -race ./...
go vet ./...
make build-release
docker compose -f deploy/compose/docker-compose.yml config --quiet
~~~

Add the repository's pinned dependency, container, and secret scanners to this
same final pass when those tools are introduced. Do not report a green release
while any required scanner is unavailable or skipped.

### Out of scope

- Claiming compatibility with etcd or another product without conformance
  testing.
- Multi-region production deployment.
- Unbounded checker workloads.

## Standard Codex handoff pattern

Handoffs occur only between separate tasks or runs, never as a reason to
interrupt an explicitly assigned in-scope task.

1. At task start, classify the work and record the recommended model.
2. Complete the assigned task end-to-end with the currently active model.
3. Run the task checks and report the completed scope, evidence, and any
   remaining work.
4. Continue with the active model for the next in-scope task. No model switch
is requested under the current user override.

For current OpenAI model-role guidance, see the
official model guidance at https://developers.openai.com/api/docs/guides/latest-model.
