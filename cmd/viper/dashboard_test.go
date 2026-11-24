package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apiv1 "viper/api/gen/v1"
)

type fakeDashboardClient struct{ fail bool }

func (f fakeDashboardClient) GetNodeStatus(_ context.Context, req *apiv1.GetNodeStatusRequest, _ ...grpc.CallOption) (*apiv1.GetNodeStatusResponse, error) {
	if f.fail {
		return nil, status.Error(codes.PermissionDenied, "private secret")
	}
	if req.GetScope() == apiv1.HealthScope_HEALTH_SCOPE_READINESS {
		return nil, status.Error(codes.Unavailable, "private details")
	}
	return &apiv1.GetNodeStatusResponse{NodeId: "node-1", Term: 9007199254740993}, nil
}
func dashboardTestConfig() dashboardConfig {
	return dashboardConfig{Listen: "127.0.0.1:8080", TLS: tlsFiles{Certificate: "/cert", Key: "/key", CA: "/ca"}, Nodes: []dashboardNode{{ID: "node-1", Target: "127.0.0.1:7401", ServerName: "node-1"}}}
}
func TestDashboardConfig(t *testing.T) {
	config := dashboardTestConfig()
	if err := validateDashboardConfig(config); err != nil {
		t.Fatal(err)
	}
	for _, listen := range []string{"0.0.0.0:8080", "localhost:8080", "127.0.0.1:0"} {
		bad := config
		bad.Listen = listen
		if validateDashboardConfig(bad) == nil {
			t.Fatalf("accepted %s", listen)
		}
	}
	config.Nodes = append(config.Nodes, config.Nodes[0])
	if validateDashboardConfig(config) == nil {
		t.Fatal("accepted duplicate")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:8080","unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDashboardConfig(path); err == nil {
		t.Fatal("accepted unknown field")
	}
}
func TestDashboardHandler(t *testing.T) {
	nodes := []dashboardNode{{ID: "node-1"}, {ID: "node-2"}}
	handler := dashboardHandler("127.0.0.1:8080", nodes, []dashboardStatusClient{fakeDashboardClient{}, fakeDashboardClient{fail: true}})
	for _, tc := range []struct {
		method, host, path string
		want               int
	}{
		{"GET", "attacker.example:8080", "/api/status", http.StatusForbidden},
		{"POST", "127.0.0.1:8080", "/api/status", http.StatusMethodNotAllowed},
		{"GET", "127.0.0.1:8080", "/missing", http.StatusNotFound},
	} {
		r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: got %d", tc.path, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("unsafe response: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Nodes []dashboardObservation `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 2 || result.Nodes[0].Liveness.Status.Term != "9007199254740993" || result.Nodes[0].Readiness.Error != "unavailable" || result.Nodes[1].Liveness.Error != "unauthorized" || result.Nodes[0].ObservedAt.IsZero() {
		t.Fatalf("unexpected result: %+v", result.Nodes)
	}
}

func TestDashboardRejectsCrossSiteAndWrongNode(t *testing.T) {
	handler := dashboardHandler("127.0.0.1:8080", []dashboardNode{{ID: "node-1"}}, []dashboardStatusClient{fakeDashboardClient{}})
	for _, header := range []struct{ name, value string }{{"Origin", "http://evil.example"}, {"Sec-Fetch-Site", "cross-site"}} {
		req := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/status", nil)
		req.Header.Set(header.name, header.value)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s: got %d", header.name, response.Code)
		}
	}
	result := dashboardQuery(context.Background(), fakeDashboardClient{}, apiv1.HealthScope_HEALTH_SCOPE_LIVENESS, "node-2")
	if result.Error != "error" || result.Status != nil {
		t.Fatalf("accepted mismatched node: %+v", result)
	}
}
