package chaos

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"viper/internal/raft"
)

func TestHarnessMajorityCommitsThroughDeterministicFaults(t *testing.T) {
	schedule := Schedule{Version: ScheduleVersion, Seed: 7, Actions: []Action{
		{Step: 1, Kind: ActionClockAdvance, Duration: 100 * time.Millisecond},
		{Step: 2, Target: "node-2", Kind: ActionCrash, Count: 1},
	}}
	harness, err := NewHarness([]raft.NodeID{"node-1", "node-2", "node-3"}, schedule)
	if err != nil {
		t.Fatalf("create harness: %v", err)
	}
	defer harness.Close()
	if err := harness.Step(); err != nil {
		t.Fatalf("elect harness leader: %v", err)
	}
	if _, ok := harness.Leader(); !ok {
		t.Fatal("harness did not elect a leader")
	}
	if err := harness.Apply(schedule.Actions[1]); err != nil {
		t.Fatalf("apply crash fault: %v", err)
	}
	index, err := harness.Propose([]byte("majority-command"))
	if err != nil || index != 1 {
		t.Fatalf("majority proposal index = %d, error = %v", index, err)
	}
	if state, ok := harness.State("node-1"); !ok || state.CommitIndex != 1 {
		t.Fatalf("leader state after fault = %#v", state)
	}
}

func TestHarnessFaultPreventsMinorityAcknowledgement(t *testing.T) {
	schedule := Schedule{Version: ScheduleVersion, Seed: 9, Actions: nil}
	harness, err := NewHarness([]raft.NodeID{"node-1", "node-2", "node-3"}, schedule)
	if err != nil {
		t.Fatalf("create harness: %v", err)
	}
	defer harness.Close()
	if err := harness.Apply(Action{Kind: ActionClockAdvance, Duration: 100 * time.Millisecond}); err != nil {
		t.Fatalf("advance clock: %v", err)
	}
	if err := harness.Tick(); err != nil {
		t.Fatalf("elect leader: %v", err)
	}
	leader, ok := harness.Leader()
	if !ok {
		t.Fatal("missing leader")
	}
	if err := harness.Apply(Action{Target: string(leader), Kind: ActionCrash, Count: 1}); err != nil {
		t.Fatalf("crash leader: %v", err)
	}
	if _, err := harness.Propose([]byte("rejected")); !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("faulted leader proposal = %v, want ErrNotLeader", err)
	}
}

func TestHarnessLeaderCrashElectsReplacement(t *testing.T) {
	harness, err := NewHarness([]raft.NodeID{"node-1", "node-2", "node-3"}, Schedule{Version: ScheduleVersion, Seed: 11})
	if err != nil {
		t.Fatalf("create harness: %v", err)
	}
	defer harness.Close()
	if err := harness.Apply(Action{Kind: ActionClockAdvance, Duration: 100 * time.Millisecond}); err != nil {
		t.Fatalf("advance election clock: %v", err)
	}
	if err := harness.Tick(); err != nil {
		t.Fatalf("elect initial leader: %v", err)
	}
	oldLeader, ok := harness.Leader()
	if !ok {
		t.Fatal("missing initial leader")
	}
	if err := harness.Apply(Action{Target: string(oldLeader), Kind: ActionCrash, Count: 1}); err != nil {
		t.Fatalf("crash leader: %v", err)
	}
	if err := harness.Apply(Action{Kind: ActionClockAdvance, Duration: 100 * time.Millisecond}); err != nil {
		t.Fatalf("advance replacement clock: %v", err)
	}
	if err := harness.Tick(); err != nil {
		t.Fatalf("elect replacement leader: %v", err)
	}
	newLeader, ok := harness.Leader()
	if !ok || newLeader == oldLeader {
		t.Fatalf("replacement leader = %q, old leader = %q", newLeader, oldLeader)
	}
	if _, err := harness.Propose([]byte("replacement-command")); err != nil {
		t.Fatalf("replacement proposal: %v", err)
	}
}

func TestScheduleRoundTripAndDeterministicCursor(t *testing.T) {
	schedule := Schedule{Version: ScheduleVersion, Seed: 42, Actions: []Action{
		{Step: 1, Target: "node-1", Kind: ActionDelay, Duration: 10 * time.Millisecond},
		{Step: 2, Target: "node-2", Kind: ActionDrop, Count: 2},
	}}
	encoded, err := Encode(schedule)
	if err != nil {
		t.Fatalf("encode schedule: %v", err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("decode schedule: %v", err)
	}
	encodedAgain, err := Encode(decoded)
	if err != nil || !bytes.Equal(encoded, encodedAgain) {
		t.Fatalf("schedule encoding is not deterministic: %s vs %s", encoded, encodedAgain)
	}
	left, err := NewCursor(schedule)
	if err != nil {
		t.Fatalf("create left cursor: %v", err)
	}
	right, err := NewCursor(schedule)
	if err != nil {
		t.Fatalf("create right cursor: %v", err)
	}
	leftTargets := left.ShuffleTargets([]string{"node-1", "node-2", "node-3"})
	rightTargets := right.ShuffleTargets([]string{"node-1", "node-2", "node-3"})
	if len(leftTargets) != len(rightTargets) || leftTargets[0] != rightTargets[0] || leftTargets[1] != rightTargets[1] || leftTargets[2] != rightTargets[2] {
		t.Fatalf("seeded permutations differ: %#v vs %#v", leftTargets, rightTargets)
	}
}

func TestScheduleRejectsUnsafeFaults(t *testing.T) {
	cases := []Schedule{
		{Version: ScheduleVersion, Seed: 1, Actions: []Action{{Kind: ActionDelay, Duration: 0}}},
		{Version: ScheduleVersion, Seed: 1, Actions: []Action{{Kind: ActionDrop, Count: 0}}},
		{Version: ScheduleVersion, Seed: 0, Actions: nil},
		{Version: ScheduleVersion, Seed: 1, Actions: []Action{{Kind: ActionDelay, Duration: 2 * time.Hour}}},
	}
	for _, schedule := range cases {
		if err := Validate(schedule); err != ErrInvalidSchedule {
			t.Fatalf("unsafe schedule error = %v, want ErrInvalidSchedule", err)
		}
	}
}
