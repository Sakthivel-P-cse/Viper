# Viper local runbook

## Fast deterministic demo

The demo uses three in-memory nodes and never claims production high
availability:

```text
go run ./cmd/viper demo
```

It elects a leader, applies a mutation through the KV service, and performs a
linearizable read. The output contains only the leader, found flag, and
revision.

## Durable service

`serve` requires a pre-provisioned data directory, static three- or five-node
membership, separate peer/client listeners, and TLS files. Use the checked-in
[server-config.json](examples/server-config.json) as a shape-only example and
replace every path with deployment-owned files. Certificates must be issued by
the configured CA; peer certificates use `urn:viper:node:<id>` URI SANs, while
client and administrator certificates use `urn:viper:client:<id>` and
`urn:viper:admin:<id>`.

```text
go run ./cmd/viper serve -config /path/to/server.json
```

The process refuses missing storage directories, plaintext gRPC credentials,
unknown cluster members, malformed requests, and unauthorized certificate
identities. Public request concurrency and rate limits are configurable and
bounded. An optional `/metrics` listener exposes only allowlisted metrics; bind
it to a private interface or restrict it with a deployment network policy.
Shutdown is initiated with SIGINT or SIGTERM and drains for the configured
shutdown timeout.

For a staging smoke check, use the authenticated probe with a client
certificate and the server name present in the server certificate:

```text
go run ./cmd/viper probe -target 127.0.0.1:7401 -server-name node-1 \
  -ca /secure/client-ca.crt -certificate /secure/client-1.crt \
  -key /secure/client-1.key
```

## Backup and recovery

Export and verify backups through the `internal/storage` API. The CLI verifies
an operator-provided backup without activating it:

```text
go run ./cmd/viper verify-backup /path/to/backup.json
```

Offline export and restore are available through the CLI. Stop the node before
exporting because the data-directory lock deliberately prevents a concurrent
second owner:

```text
go run ./cmd/viper export-backup -data-dir /var/lib/viper/node-1 -output /secure/backup.json
go run ./cmd/viper verify-backup /secure/backup.json
go run ./cmd/viper restore-backup -data-dir /var/lib/viper/restore-node -backup /secure/backup.json
```

The backup is checksummed, not encrypted. Store it in an encrypted,
access-controlled backup system and verify it before activation.

`storage.RestoreBackup` only targets a directory without an existing Viper WAL
or snapshot. It never overwrites an existing store. Verify a backup before
activation and retain the last-known-good directory until the restored node
has passed readiness checks.

## Fault evidence

Schedules are versioned, bounded, and seeded. Validate a schedule before using
it in the deterministic chaos harness:

```text
go run ./cmd/viper validate-schedule /path/to/schedule.json
```

The harness is test-only. It does not disable production validation or convert
a minority partition into an acknowledged write.

## Operational boundaries

- Membership is static; editing one node's peer list is not a membership
  change protocol.
- A data directory has one active owner. A second process must not be started
  against the same directory; `storage.Open` enforces this with `.viper.lock`.
- The checker is bounded and independent of the serving implementation.
- The local in-memory demo is not a multi-host availability proof.
- Cross-process store locking, multi-region consensus, arbitrary SQL, and
  unbounded history checking are unsupported.
