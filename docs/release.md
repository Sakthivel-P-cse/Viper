# Release and production evidence

Viper's local verification gate is necessary but does not prove availability
on a real deployment. A release candidate must attach evidence for the
following gates:

1. `make verify security-check build` on a clean runner.
2. A successful `docker build --pull` with the release version and commit
   metadata injected through `make build-release`.
3. A three- or five-node staging run with TLS, persistent volumes, and separate
   process ownership for every data directory.
4. Leader crash, minority partition, disk-full, WAL-corruption, snapshot
   restore, and restart drills with no acknowledged-write loss.
5. A verified encrypted off-host backup and a measured restore RPO/RTO.
6. A tested maintenance upgrade and rollback. Mixed-version rolling upgrades
   are unsupported until a compatibility matrix is added.
7. Benchmark and soak results that include workload, topology, hardware,
   payload sizes, p50/p95/p99 latency, throughput, recovery duration, and
   failure behavior.
8. Certificate rotation, authorization, dependency, image, SBOM, secret, and
   external security-review evidence.

The Docker Compose profile under `deploy/compose` is a local integration
profile. It is intentionally not a production high-availability deployment.
Pin the base-image digests and publish signed artifacts before using an image
outside staging.
