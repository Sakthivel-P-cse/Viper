package chaos

import (
	"testing"

	"viper/internal/raft"
)

func TestHarnessStepAppliesAllActionsAtTheirScheduledStep(t *testing.T) {
	harness, err := NewHarness([]raft.NodeID{"node-1", "node-2", "node-3"}, Schedule{
		Version: ScheduleVersion,
		Seed:    1,
		Actions: []Action{
			{Step: 2, Target: "node-2", Kind: ActionCrash, Count: 1},
			{Step: 2, Target: "node-3", Kind: ActionDiskFailure, Count: 1},
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
