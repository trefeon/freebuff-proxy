// Mock upstream seam for the production wire client. The simulated upstream
// for dummy/mock tokens (mockSessionState and its quota ledger) lives in the
// testmock package; production methods branch on the narrow MockUpstream
// interface (c.mock), never on the token prefix. NewMockWire lets the testmock
// package register the dummy-token simulator via init without the wire client
// importing it (keeping the production package free of test-support code).
package upstream

import "strings"

// MockUpstream is the narrow simulated-upstream contract the wire client
// consults before hitting the network. It is implemented by the testmock
// package for dummy/mock tokens and by test fixtures; production real-token
// clients leave it nil.
type MockUpstream interface {
	// CreateSession simulates a session create for the model.
	CreateSession(token, model string) (*SessionState, error)
	// GetSession simulates a session poll for the instance ("" = probe-shaped
	// poll with no instance header).
	GetSession(token, instanceID string) (*SessionState, error)
	// Probe simulates the zero-cost token probe.
	Probe(token string) (*SessionState, error)
	// EndSession simulates the session DELETE.
	EndSession(token string) error
	// StartRun simulates an agent-run START.
	StartRun(token, agentID string) (string, error)
	// FinishRun simulates an agent-run FINISH.
	FinishRun(token string) error
}

// NewMockWire is populated by the testmock package (via init) to build a
// MockUpstream for a token. The wire client calls it once in the constructor;
// a nil hook leaves the client on the real network path. It is the only place
// the token prefix is ever inspected, and it lives outside the production
// request paths.
var NewMockWire func(token string) MockUpstream

// IsDummyToken reports whether token is a dummy/mock fixture token
// ("cb_dummy...", "dummy-...", "mock-..."). Used by the token-health path
// (CheckTokenHealth/FetchAccountInfo) to short-circuit a read-only probe; the
// request methods use the MockUpstream interface instead.
func IsDummyToken(token string) bool {
	t := strings.ToLower(strings.TrimSpace(token))
	return strings.HasPrefix(t, "cb_dummy") || strings.HasPrefix(t, "dummy-") || strings.HasPrefix(t, "mock-")
}

// IsMock reports whether the client is backed by a simulated upstream.
func (c *Client) IsMock() bool { return c.mock != nil }

// SetMock installs a simulated upstream on the client. Test fixtures and the
// testmock package use it to wire the dummy-token simulation without the wire
// client importing the simulation.
func (c *Client) SetMock(m MockUpstream) { c.mock = m }
