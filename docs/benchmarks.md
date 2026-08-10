# Benchmark evidence

The following is a local microbenchmark smoke run, not a production SLO. It
was run on 2026-08-10 with Go 1.26.5 on an AMD Ryzen 7 8845HS with Radeon 780M
Graphics, Linux amd64:

```text
GOCACHE=/tmp/viper-gocache go test -run '^$' -bench . -benchmem ./internal/statemachine ./internal/storage
```

Observed results:

| Workload | Result |
| --- | ---: |
| Machine Apply, unique Put | 1,970 ns/op, 797 B/op, 14 allocs/op |
| Machine Get | 34.31 ns/op, 16 B/op, 1 alloc/op |
| Durable Store Persist, one WAL entry with sync | 2,635 ns/op, 492 B/op, 5 allocs/op |

These measurements use one process, one local filesystem, no TLS, no network,
and no replicated quorum. They must not be used as a production latency or
throughput promise. A release still requires staging measurements with the
target topology, hardware, payload sizes, p50/p95/p99 latency, throughput,
recovery time, and failure behavior.
