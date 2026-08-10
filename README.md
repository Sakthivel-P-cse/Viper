# Viper

Viper is a modular, single-region distributed key-value service built around
Raft consensus. The production baseline targets linearizable writes and reads,
durable recovery, authenticated transport, bounded resource use, and explicit
operational behavior.

The implementation now includes the deterministic Raft core, checksummed WAL
recovery, static in-memory and TLS peer transport, replicated KV service
handlers, authenticated gRPC construction, snapshots, backups, a bounded
linearizability checker, deterministic chaos fixtures, and a local CLI runtime.
The consolidated verification pass is green for the current bounded scope. See
the [industry-grade design](docs/industry-grade/design.md),
the [storage durability contract](docs/industry-grade/storage.md), and
the [local runbook](docs/runbook.md) for supported usage. `documentation.md`
remains the original project proposal and is subordinate to the industry-grade
baseline.

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

Run the safe local demo with:

```bash
go run ./cmd/viper demo
```

Use `go run ./cmd/viper serve -config ...` only with pre-provisioned storage,
static membership, and TLS files. The local demo is not a production
availability claim. A repeatable authenticated local process cluster is
available under [deploy/compose](deploy/compose/README.md), and release
evidence requirements are documented in [docs/release.md](docs/release.md).

The `probe` command performs one authenticated Put and linearizable Get
against a running endpoint. It requires a client certificate and CA and never
prints credentials.

The module path is intentionally local (`viper`) until the repository receives
a stable public URL. It must be changed once, before publishing generated API
clients or a v1 release. The architecture test derives this value from `go.mod`
and fails closed when an internal module has no declared dependency policy.

Every gRPC server and client must apply `contracts.MaxRPCMessageBytes` as its
message limit. Protovalidate rules provide field and collection limits before
requests enter the proposal or peer-processing pipeline.
