# Local TLS cluster profile

This profile is a repeatable local integration environment, not a production
availability proof. It runs three authenticated Viper processes with separate
persistent data directories, peer listeners, client listeners, and an internal
metrics network.

Generate short-lived development certificates and node configurations:

```text
./deploy/compose/generate-dev-tls.sh
export VIPER_UID=$(id -u)
export VIPER_GID=$(id -g)
docker compose -f deploy/compose/docker-compose.yml up --build -d
docker compose -f deploy/compose/docker-compose.yml ps
docker compose -f deploy/compose/docker-compose.yml logs --no-log-prefix node-1
```

Stop the profile with:

```text
docker compose -f deploy/compose/docker-compose.yml down
```

The generated `.runtime/` directory contains private keys and is ignored by
Git. Do not reuse these certificates outside local testing.
