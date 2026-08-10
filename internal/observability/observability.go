package observability

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Logger emits structured records using only an allowlisted set of safe
// operational fields. Request bodies, values, credentials, and tokens are not
// accepted by the API.
type Logger struct {
	logger *slog.Logger
}

// NewLogger creates a JSON logger. A nil writer disables output safely.
func NewLogger(writer io.Writer) *Logger {
	if writer == nil {
		writer = io.Discard
	}
	return &Logger{logger: slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo}))}
}

// Info emits one bounded operational event.
func (l *Logger) Info(message string, fields map[string]string) {
	if l == nil || l.logger == nil {
		return
	}
	attributes := make([]any, 0, len(fields)*2)
	for key, value := range fields {
		if !safeField(key) || len(value) > 256 {
			continue
		}
		attributes = append(attributes, key, value)
	}
	l.logger.Info(message, attributes...)
}

func safeField(key string) bool {
	switch key {
	case "event", "node_id", "role", "health", "reason", "operation", "error_code", "outcome", "peer", "term", "commit_index", "applied_index", "status":
		return true
	default:
		return false
	}
}

// HealthState is a redacted process readiness state.
type HealthState uint8

const (
	HealthUnknown HealthState = iota
	HealthServing
	HealthDegraded
	HealthNotServing
)

// HealthSnapshot is the complete safe status surface exposed to operators.
type HealthSnapshot struct {
	NodeID       string
	Health       HealthState
	Role         string
	Term         uint64
	CommitIndex  uint64
	AppliedIndex uint64
	Reason       string
	UpdatedAt    time.Time
}

// HealthRegistry stores one isolated status snapshot.
type HealthRegistry struct {
	mu       sync.RWMutex
	snapshot HealthSnapshot
}

// NewHealthRegistry creates a registry with a non-serving initial state.
func NewHealthRegistry(nodeID string) *HealthRegistry {
	return &HealthRegistry{snapshot: HealthSnapshot{NodeID: nodeID, Health: HealthNotServing, UpdatedAt: time.Now()}}
}

// Update replaces health state after bounding operator-visible fields.
func (r *HealthRegistry) Update(snapshot HealthSnapshot) {
	if r == nil {
		return
	}
	if len(snapshot.NodeID) > 128 {
		snapshot.NodeID = snapshot.NodeID[:128]
	}
	if len(snapshot.Role) > 32 {
		snapshot.Role = snapshot.Role[:32]
	}
	if len(snapshot.Reason) > 256 {
		snapshot.Reason = snapshot.Reason[:256]
	}
	snapshot.UpdatedAt = time.Now()
	r.mu.Lock()
	r.snapshot = snapshot
	r.mu.Unlock()
}

// Snapshot returns an isolated status value.
func (r *HealthRegistry) Snapshot() HealthSnapshot {
	if r == nil {
		return HealthSnapshot{Health: HealthUnknown}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshot
}

// CounterSet exposes bounded counters keyed only by predefined safe labels.
type CounterSet struct {
	mu       sync.Mutex
	counters map[string]uint64
}

// NewCounterSet creates an empty metric set.
func NewCounterSet() *CounterSet { return &CounterSet{counters: make(map[string]uint64)} }

// Add increments a named counter. Unknown or oversized labels are ignored.
func (c *CounterSet) Add(name string, delta uint64) {
	if c == nil || len(name) == 0 || len(name) > 64 || !safeMetric(name) {
		return
	}
	c.mu.Lock()
	c.counters[name] += delta
	c.mu.Unlock()
}

// Snapshot returns a copy of all bounded counters.
func (c *CounterSet) Snapshot() map[string]uint64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	copyOfCounters := make(map[string]uint64, len(c.counters))
	for name, value := range c.counters {
		copyOfCounters[name] = value
	}
	return copyOfCounters
}

// WritePrometheus writes a stable, label-free exposition suitable for a
// private scrape endpoint. Metric names are allowlisted by Add, so client
// identifiers and request data can never become label cardinality.
func (c *CounterSet) WritePrometheus(writer io.Writer) error {
	if writer == nil {
		return fmt.Errorf("metrics writer is nil")
	}
	values := c.Snapshot()
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := fmt.Fprintf(writer, "# TYPE viper_%s counter\nviper_%s %d\n", name, name, values[name]); err != nil {
			return err
		}
	}
	return nil
}

// MetricsHandler exposes bounded process metrics. It is intentionally a
// separate opt-in HTTP endpoint and should be bound to a private interface or
// protected by the deployment network policy.
func MetricsHandler(counters *CounterSet, health *HealthRegistry) http.Handler {
	if counters == nil {
		counters = NewCounterSet()
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/metrics" {
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := counters.WritePrometheus(writer); err != nil {
			http.Error(writer, "metrics unavailable", http.StatusInternalServerError)
			return
		}
		snapshot := health.Snapshot()
		_, _ = fmt.Fprintln(writer, "# TYPE viper_node_health gauge")
		if snapshot.Health == HealthServing || snapshot.Health == HealthDegraded {
			_, _ = fmt.Fprintln(writer, "viper_node_health 1")
		} else {
			_, _ = fmt.Fprintln(writer, "viper_node_health 0")
		}
		_, _ = fmt.Fprintf(writer, "# TYPE viper_node_term gauge\nviper_node_term %d\n", snapshot.Term)
		_, _ = fmt.Fprintf(writer, "# TYPE viper_node_commit_index gauge\nviper_node_commit_index %d\n", snapshot.CommitIndex)
		_, _ = fmt.Fprintf(writer, "# TYPE viper_node_applied_index gauge\nviper_node_applied_index %d\n", snapshot.AppliedIndex)
		role := snapshot.Role
		if role != "follower" && role != "candidate" && role != "leader" {
			role = "unknown"
		}
		_, _ = fmt.Fprintf(writer, "# TYPE viper_node_role gauge\nviper_node_role{role=%q} 1\n", role)
	})
}

func safeMetric(name string) bool {
	switch name {
	case "requests_total", "request_errors_total", "request_rate_limited_total", "proposals_total", "proposal_failures_total", "elections_total", "replication_failures_total", "wal_sync_failures_total", "snapshot_failures_total":
		return true
	default:
		return false
	}
}
