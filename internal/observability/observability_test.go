package observability

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoggerAllowlistsOperationalFields(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output)
	logger.Info("event", map[string]string{
		"event": "request",
		"role":  "leader",
		"value": "must-not-log",
	})
	if !strings.Contains(output.String(), "leader") || strings.Contains(output.String(), "must-not-log") {
		t.Fatalf("logger output = %s", output.String())
	}
}

func TestMetricsHandlerUsesStablePrivateNames(t *testing.T) {
	counters := NewCounterSet()
	counters.Add("requests_total", 3)
	health := NewHealthRegistry("node-1")
	health.Update(HealthSnapshot{Health: HealthServing, Role: "leader", Term: 4, CommitIndex: 8, AppliedIndex: 7})
	request := httptest.NewRequest(http.MethodGet, "http://metrics.invalid/metrics", nil)
	response := httptest.NewRecorder()
	MetricsHandler(counters, health).ServeHTTP(response, request)
	var output bytes.Buffer
	if _, err := output.ReadFrom(response.Result().Body); err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if response.Code != http.StatusOK || !strings.Contains(output.String(), "viper_requests_total 3") || !strings.Contains(output.String(), "viper_node_role{role=\"leader\"} 1") {
		t.Fatalf("metrics response = %d %s", response.Code, output.String())
	}
	if strings.Contains(output.String(), "node-1") {
		t.Fatal("metrics response exposed node identity")
	}
}

func TestHealthAndMetricsAreBoundedAndIsolated(t *testing.T) {
	health := NewHealthRegistry("node-1")
	health.Update(HealthSnapshot{NodeID: "node-1", Health: HealthServing, Role: "leader", Reason: strings.Repeat("x", 300)})
	snapshot := health.Snapshot()
	if snapshot.Health != HealthServing || len(snapshot.Reason) != 256 {
		t.Fatalf("health snapshot = %#v", snapshot)
	}
	counters := NewCounterSet()
	counters.Add("requests_total", 2)
	counters.Add("unbounded_user_key", 100)
	values := counters.Snapshot()
	if values["requests_total"] != 2 || len(values) != 1 {
		t.Fatalf("counter snapshot = %#v", values)
	}
	values["requests_total"] = 0
	if counters.Snapshot()["requests_total"] != 2 {
		t.Fatal("counter snapshot was not isolated")
	}
}
