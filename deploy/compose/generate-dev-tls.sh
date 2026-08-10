#!/bin/sh
set -eu

PROJECT_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
RUNTIME="$PROJECT_ROOT/.runtime"
TLS="$RUNTIME/tls"
CONFIG="$RUNTIME/config"
DATA="$RUNTIME/data"

umask 077
mkdir -p "$TLS" "$CONFIG" "$DATA/node-1" "$DATA/node-2" "$DATA/node-3"

if [ ! -f "$TLS/cluster-ca.crt" ]; then
	openssl req -x509 -newkey rsa:3072 -nodes -days 7 \
		-keyout "$TLS/cluster-ca.key" -out "$TLS/cluster-ca.crt" \
		-subj "/CN=Viper local cluster CA" \
		-addext "basicConstraints=critical,CA:TRUE" \
		-addext "keyUsage=critical,keyCertSign,cRLSign"
fi

if [ ! -f "$TLS/client-ca.crt" ]; then
	openssl req -x509 -newkey rsa:3072 -nodes -days 7 \
		-keyout "$TLS/client-ca.key" -out "$TLS/client-ca.crt" \
		-subj "/CN=Viper local client CA" \
		-addext "basicConstraints=critical,CA:TRUE" \
		-addext "keyUsage=critical,keyCertSign,cRLSign"
fi

issue_cert() {
	name=$1
	ca_prefix=$2
	uri=$3
	shift 3
	key="$TLS/$name.key"
	csr="$RUNTIME/$name.csr"
	ext="$RUNTIME/$name.ext"
	{
		printf '%s\n' 'basicConstraints=critical,CA:FALSE'
		printf '%s\n' 'keyUsage=critical,digitalSignature,keyEncipherment'
		printf '%s\n' 'extendedKeyUsage=serverAuth,clientAuth'
		printf '%s' 'subjectAltName=URI:'
		printf '%s' "$uri"
		for dns in "$@"; do
			printf ',DNS:%s' "$dns"
		done
		printf '\n'
	} > "$ext"
	openssl req -new -newkey rsa:2048 -nodes -keyout "$key" -out "$csr" -subj "/CN=$name"
	openssl x509 -req -days 7 -in "$csr" \
		-CA "$TLS/$ca_prefix-ca.crt" -CAkey "$TLS/$ca_prefix-ca.key" \
		-CAcreateserial -out "$TLS/$name.crt" -extfile "$ext"
	rm -f "$csr" "$ext" "$TLS/$ca_prefix-ca.srl"
}

for node in node-1 node-2 node-3; do
	issue_cert "$node" cluster "urn:viper:node:$node" "$node"
done
issue_cert client-server client "urn:viper:server:client" node-1 node-2 node-3
issue_cert client-1 client "urn:viper:client:client-1" client-1
issue_cert admin-1 client "urn:viper:admin:admin-1" admin-1

for node in node-1 node-2 node-3; do
	cat > "$CONFIG/$node.json" <<EOF
{
  "node_id": "$node",
  "data_dir": "/var/lib/viper/$node",
  "peer_listen": "0.0.0.0:730${node#node-}",
  "client_listen": "0.0.0.0:740${node#node-}",
  "peers": [
    {"id": "node-1", "address": "node-1:7301", "server_name": "node-1"},
    {"id": "node-2", "address": "node-2:7302", "server_name": "node-2"},
    {"id": "node-3", "address": "node-3:7303", "server_name": "node-3"}
  ],
  "peer_tls": {
    "certificate": "/etc/viper/tls/$node.crt",
    "key": "/etc/viper/tls/$node.key",
    "ca": "/etc/viper/tls/cluster-ca.crt"
  },
  "client_tls": {
    "certificate": "/etc/viper/tls/client-server.crt",
    "key": "/etc/viper/tls/client-server.key",
    "ca": "/etc/viper/tls/client-ca.crt"
  },
  "allowed_clients": ["client-1"],
  "allowed_administrators": ["admin-1"],
  "metrics_listen": "0.0.0.0:750${node#node-}",
  "max_concurrent_requests": 256,
  "max_in_flight_peer_requests": 64,
  "shutdown_timeout_millis": 5000
}
EOF
done

printf '%s\n' "Generated local TLS and configs under $RUNTIME. Export VIPER_UID=$(id -u) and VIPER_GID=$(id -g) before docker compose up."
