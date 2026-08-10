package main

import (
	"os"
	"path/filepath"
	"testing"

	"viper/internal/raft"
	"viper/internal/transport"
)

func validServeConfigForTest() serveConfig {
	return serveConfig{
		NodeID:       "node-1",
		DataDir:      "/var/lib/viper/node-1",
		PeerListen:   "127.0.0.1:7301",
		ClientListen: "127.0.0.1:7401",
		Peers: []transport.Peer{
			{ID: "node-1", Address: "127.0.0.1:7301"},
			{ID: "node-2", Address: "127.0.0.1:7302"},
			{ID: "node-3", Address: "127.0.0.1:7303"},
		},
		PeerTLS:   tlsFiles{Certificate: "/etc/viper/node.crt", Key: "/etc/viper/node.key", CA: "/etc/viper/ca.crt"},
		ClientTLS: tlsFiles{Certificate: "/etc/viper/client.crt", Key: "/etc/viper/client.key", CA: "/etc/viper/client-ca.crt"},
	}
}

func TestValidateServeConfigRequiresProductionSafeShape(t *testing.T) {
	base := validServeConfigForTest()
	if err := validateServeConfig(base); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*serveConfig)
	}{
		{name: "relative data directory", mutate: func(config *serveConfig) { config.DataDir = "data/node-1" }},
		{name: "same listeners", mutate: func(config *serveConfig) { config.ClientListen = config.PeerListen }},
		{name: "unbounded requests", mutate: func(config *serveConfig) { config.MaxConcurrentRequests = 4097 }},
		{name: "missing TLS path", mutate: func(config *serveConfig) { config.ClientTLS.Key = "client.key" }},
		{name: "wrong membership", mutate: func(config *serveConfig) {
			config.Peers = []transport.Peer{{ID: raft.NodeID("node-1"), Address: "127.0.0.1:7301"}}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := base
			testCase.mutate(&config)
			if err := validateServeConfig(config); err == nil {
				t.Fatal("unsafe configuration was accepted")
			}
		})
	}
}

func TestWritePrivateFileRefusesAccidentalOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	if err := writePrivateFile(path, []byte("one"), false); err != nil {
		t.Fatalf("write private file: %v", err)
	}
	if err := writePrivateFile(path, []byte("two"), false); err == nil {
		t.Fatal("second write unexpectedly overwrote the backup")
	}
	if err := writePrivateFile(path, []byte("two"), true); err != nil {
		t.Fatalf("forced write private file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "two" {
		t.Fatalf("backup data = %q, error = %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %o, want 600", info.Mode().Perm())
	}
}
