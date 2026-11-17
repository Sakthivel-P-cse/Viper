package chaos

import (
	"testing"
	"time"

	"viper/internal/raft"
)

func TestHarnessStepAppliesAllActionsAtTheirScheduledStep(t *testing.T) {
	harness, err := NewHarness([]raft.NodeID{"node-1", "node-2", "node-3"}, Schedule{
		Version: ScheduleVersion,
		Seed:    1,
		Actions: []Action{
			{Step: 2, Target: "node-2", Kind: ActionCrash, Count: 1},
			{Step: 2, Target: "node-3", Kind: ActionCrash, Count: 1},
		},
	})
	if err != nil {
		t.Fatalf("create harness: %v", err)
	}
	defer harness.Close()

	if err := harness.Step(); err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if harness.handlers["node-2"].faulted() || harness.handlers["node-3"].faulted() {
		t.Fatal("step 2 actions were applied during step 1")
	}

	if err := harness.Step(); err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if !harness.handlers["node-2"].faulted() || !harness.handlers["node-3"].faulted() {
		t.Fatal("step 2 did not apply every scheduled action")
	}
}

func TestDiskFailureMakesNodeUnhealthyOnPersist(t *testing.T) {
	harness, err := NewHarness([]raft.NodeID{"node-1", "node-2", "node-3"}, Schedule{Version: ScheduleVersion, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	if err := harness.Apply(Action{Kind: ActionClockAdvance, Duration: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if err := harness.Tick(); err != nil {
		t.Fatal(err)
	}
	leader, ok := harness.Leader()
	if !ok {
		t.Fatal("missing leader")
	}
	if err := harness.Apply(Action{Kind: ActionDiskFailure, Target: string(leader)}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.Propose([]byte("write")); err == nil {
		t.Fatal("proposal succeeded after disk failure")
	}
	if state, _ := harness.State(leader); state.Healthy {
		t.Fatal("node remained healthy after disk failure")
	}
}
