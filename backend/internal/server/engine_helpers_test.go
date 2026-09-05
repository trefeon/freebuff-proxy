package server

// chatErrClass bucket pins: the trace error column is the dashboard's only
// signal when the downstream access line keeps its 200 default (client gone
// before anything was written), so a canceled client must not collapse into
// the generic "error" bucket.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"freebuff-proxy/backend/internal/upstream"
)

func TestChatErrClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"canceled", context.Canceled, "client_canceled"},
		{"wrapped canceled", fmt.Errorf("chat attempt: %w", context.Canceled), "client_canceled"},
		{"turn spend limited", &upstream.TurnSpendLimitError{Status: http.StatusTooManyRequests, Body: "x"}, "turn_spend_limited"},
		{"superseded", &upstream.SessionSupersededError{Status: http.StatusConflict}, "session_superseded"},
		{"generic", errors.New("boom"), "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatErrClass(tc.err); got != tc.want {
				t.Errorf("chatErrClass(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
