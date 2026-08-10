package chaos

import "testing"

func FuzzDecodeSchedule(f *testing.F) {
	seed, err := Encode(Schedule{Version: ScheduleVersion, Seed: 1, Actions: []Action{{Kind: ActionClockAdvance, Duration: 10}}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Decode(data)
	})
}
