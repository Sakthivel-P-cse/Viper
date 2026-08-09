# Viper

Viper is a modular, single-region distributed key-value service built around
Raft consensus. The production baseline targets linearizable writes and reads,
durable recovery, authenticated transport, bounded resource use, and explicit
operational behavior.

The implementation is being delivered phase by phase. The baseline established
versioned contracts, package boundaries, build checks, and test infrastructure.
The storage baseline adds checksummed WAL persistence, fail-closed recovery,
atomic snapshots, and checkpoint compaction. Transport, state-machine, and
client-adapter foundations are now present, while the Raft core and its
integration remain the next correctness gate. See the [industry-grade design](docs/industry-grade/design.md),
the [storage durability contract](docs/industry-grade/storage.md), and
`documentation.md` for the original project documentation.

## Module boundaries

- `api/proto`: versioned public and peer wire contracts.
- `pkg/contracts`: dependency-light public domain contracts.
- `internal/raft`: deterministic consensus core.
- `internal/storage`: WAL, metadata, snapshots, and recovery adapters.
- `internal/transport`: in-memory and gRPC peer transports.
- `internal/statemachine`: replicated KV application and deduplication.
- `internal/service`: client-facing application orchestration.
- `internal/security`: identity and authorization policies.
- `internal/observability`: logging, metrics, tracing, and health adapters.
- `internal/checker`: standalone history and linearizability verification.
- `internal/chaos`: deterministic fault injection.
- `internal/testkit`: shared test-only infrastructure.

Production packages follow enforced one-way dependency rules. The Raft core and
checker do not depend on infrastructure packages.

## Local verification

Requirements: Go 1.26+, Buf 1.72+, and `rg`.

```bash
make verify
```

The module path is intentionally local (`viper`) until the repository receives
a stable public URL. It must be changed once, before publishing generated API
clients or a v1 release. The architecture test derives this value from `go.mod`
and fails closed when an internal module has no declared dependency policy.

Every gRPC server and client must apply `contracts.MaxRPCMessageBytes` as its
message limit. Protovalidate rules provide field and collection limits before
requests enter the proposal or peer-processing pipeline.
