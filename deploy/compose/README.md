# Local TLS cluster profile

This profile is a repeatable local integration environment, not a production
availability proof. It runs three authenticated Viper processes with separate
persistent data directories, peer listeners, client listeners, and an internal
metrics network.

With Docker Compose, OpenSSL, Python 3, and Go installed, generate short-lived
development certificates and configurations, then start the cluster:

```text
./deploy/compose/generate-dev-tls.sh
export VIPER_UID=$(id -u)
export VIPER_GID=$(id -g)
docker compose -f deploy/compose/docker-compose.yml up --build -d
docker compose -f deploy/compose/docker-compose.yml ps
docker compose -f deploy/compose/docker-compose.yml logs --no-log-prefix node-1
```

Start the read-only local dashboard in a separate terminal:

```text
go run ./cmd/viper dashboard -config .runtime/config/dashboard.json
```

Open <http://127.0.0.1:8080> on the same machine. The dashboard uses the
generated administrator certificate to query each node over TLS. Node client
ports are published only on loopback. Individual status observations do not
prove quorum or production availability. Stop the dashboard with Ctrl+C.

Stop the profile with:

```text
docker compose -f deploy/compose/docker-compose.yml down
```

The generated `.runtime/` directory contains private keys and is ignored by
Git. Do not reuse these certificates outside local testing.
