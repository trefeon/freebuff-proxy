package upstream

import (
	"strconv"
	"testing"
	"time"
)

// TestSharedWireTimeSemantics pins the unified timestamp interpretation that
// both getTime (JSON field extraction) and parseFlexTime (direct value) now
// share: RFC3339, RFC3339Nano, unix seconds, and unix milliseconds parse
// identically through either caller (issue #304). unixFrom treats >= 1e11 as
// milliseconds.
func TestSharedWireTimeSemantics(t *testing.T) {
	base := time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC)
	sec := base.Unix()
	ms := base.UnixMilli()

	cases := []struct {
		name string
		val  any
		want time.Time
	}{
		{"RFC3339", base.Format(time.RFC3339), base},
		{"RFC3339Nano", base.Format(time.RFC3339Nano), base},
		{"unix seconds", float64(sec), base},
		{"unix seconds string", itoa(sec), base},
		{"unix ms", float64(ms), base},
		{"unix ms string", itoa(ms), base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// parseFlexTime direct.
			if got, err := parseFlexTime(tc.val); err != nil {
				t.Fatalf("parseFlexTime(%v): %v", tc.val, err)
			} else if !got.Equal(tc.want) {
				t.Errorf("parseFlexTime(%v) = %v, want %v", tc.val, got, tc.want)
			}
			// getTime through a map (the parsed field path).
			m := map[string]any{"resetAt": tc.val}
			if got, ok := getTime(m, "resetAt"); !ok {
				t.Errorf("getTime(%v) = no match", tc.val)
			} else if !got.Equal(tc.want) {
				t.Errorf("getTime(%v) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
