package chaos

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"
)

const (
	ScheduleVersion  = 1
	MaxScheduleBytes = 4 * 1024 * 1024
	MaxActions       = 10_000
)

// ActionKind identifies a deterministic test-only fault.
type ActionKind string

const (
	ActionDelay        ActionKind = "delay"
	ActionDrop         ActionKind = "drop"
	ActionDuplicate    ActionKind = "duplicate"
	ActionReorder      ActionKind = "reorder"
	ActionCrash        ActionKind = "crash"
	ActionDiskFailure  ActionKind = "disk_failure"
	ActionClockAdvance ActionKind = "clock_advance"
)

// Action is one bounded fault injection instruction.
type Action struct {
	Step     uint64        `json:"step"`
	Target   string        `json:"target,omitempty"`
	Kind     ActionKind    `json:"kind"`
	Duration time.Duration `json:"duration,omitempty"`
	Count    uint32        `json:"count,omitempty"`
}

// Schedule is a versioned, reproducible fault plan.
type Schedule struct {
	Version uint32   `json:"version"`
	Seed    int64    `json:"seed"`
	Actions []Action `json:"actions"`
}

var (
	ErrInvalidSchedule  = errors.New("invalid chaos schedule")
	ErrScheduleTooLarge = errors.New("chaos schedule exceeds bounds")
)

// Encode validates and serializes a schedule.
func Encode(schedule Schedule) ([]byte, error) {
	if err := Validate(schedule); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(schedule)
	if err != nil {
		return nil, fmt.Errorf("encode chaos schedule: %w", err)
	}
	if len(encoded) > MaxScheduleBytes {
		return nil, ErrScheduleTooLarge
	}
	return encoded, nil
}

// Decode strictly decodes a schedule and rejects unknown fields.
func Decode(data []byte) (Schedule, error) {
	if len(data) > MaxScheduleBytes {
		return Schedule{}, ErrScheduleTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var schedule Schedule
	if err := decoder.Decode(&schedule); err != nil {
		return Schedule{}, fmt.Errorf("%w: invalid JSON", ErrInvalidSchedule)
	}
	if err := Validate(schedule); err != nil {
		return Schedule{}, err
	}
	return schedule, nil
}

// Validate checks action bounds and deterministic ordering.
func Validate(schedule Schedule) error {
	if schedule.Version != ScheduleVersion || len(schedule.Actions) > MaxActions || schedule.Seed == 0 {
		return ErrInvalidSchedule
	}
	previousStep := uint64(0)
	for index, action := range schedule.Actions {
		if action.Step < previousStep || index > 0 && action.Step == previousStep && action.Kind == "" {
			return ErrInvalidSchedule
		}
		if action.Target != "" && !validTarget(action.Target) {
			return ErrInvalidSchedule
		}
		if !validAction(action) {
			return ErrInvalidSchedule
		}
		previousStep = action.Step
	}
	return nil
}

func validAction(action Action) bool {
	switch action.Kind {
	case ActionDelay, ActionClockAdvance:
		return action.Duration > 0 && action.Duration <= time.Minute
	case ActionDrop, ActionReorder:
		return action.Count > 0 && action.Count <= 1024
	case ActionDuplicate:
		return action.Count >= 1 && action.Count <= 1024
	case ActionCrash, ActionDiskFailure:
		return action.Count <= 1
	default:
		return false
	}
}

func validTarget(target string) bool {
	if len(target) == 0 || len(target) > 128 {
		return false
	}
	for _, character := range target {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' || character == ':') {
			return false
		}
	}
	return true
}

// Cursor replays a validated schedule deterministically.
type Cursor struct {
	schedule Schedule
	position int
	rng      *rand.Rand
}

// NewCursor creates a schedule cursor with a reproducible random source.
func NewCursor(schedule Schedule) (*Cursor, error) {
	if err := Validate(schedule); err != nil {
		return nil, err
	}
	return &Cursor{schedule: schedule, rng: rand.New(rand.NewSource(schedule.Seed))}, nil
}

// Next returns the next action and whether one exists.
func (c *Cursor) Next() (Action, bool) {
	if c == nil || c.position >= len(c.schedule.Actions) {
		return Action{}, false
	}
	action := c.schedule.Actions[c.position]
	c.position++
	return action, true
}

// ShuffleTargets returns a deterministic permutation of target names for a
// reorder fault without mutating the caller's slice.
func (c *Cursor) ShuffleTargets(targets []string) []string {
	if c == nil {
		return append([]string(nil), targets...)
	}
	copyOfTargets := append([]string(nil), targets...)
	c.rng.Shuffle(len(copyOfTargets), func(i, j int) { copyOfTargets[i], copyOfTargets[j] = copyOfTargets[j], copyOfTargets[i] })
	return copyOfTargets
}
