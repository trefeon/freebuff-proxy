package server

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// newTestServerStack is the ONE internal stack constructor (issue #256):
// real clients/session managers per mock, fallback registry, pool, server —
// configured by apiKeys + the mutation hook, optionally with a custom
// logger/log ring. Thin wrappers adapt it to httptest or extra Options.
// NewTestServerStack is the exported bridge for the external test package's
// thin httptest wrappers (issue #256); internal tests call
// newTestServerStack directly.
func NewTestServerStack(t *testing.T, apiKeys []string, mocks []*testutil.MockUpstream, mut func(*config.Config), logger *slog.Logger, ring *logring.Handler, opts ...func(*Server)) (*Server, *pool.Pool) {
	return newTestServerStack(t, apiKeys, mocks, mut, logger, ring, opts...)
}

func newTestServerStack(t *testing.T, apiKeys []string, mocks []*testutil.MockUpstream, mut func(*config.Config), logger *slog.Logger, ring *logring.Handler, opts ...func(*Server)) (*Server, *pool.Pool) {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, len(mocks)),
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
		APIKeys:            apiKeys,
		LogAccess:          true,
		DashboardEnabled:   true,
		// Dev tools default off in production (DEVTOOLS_ENABLED); the test
		// stack exercises the smoke/playground surfaces, so keep them on
		// unless a test explicitly flips it (S-05 gate tests set false).
		DevToolsEnabled: true,
		QuotaFallbackModels: map[string]string{
			"deepseek/deepseek-v4-flash": "mimo/mimo-v2.5",
			"z-ai/glm-5.2":               "deepseek/deepseek-v4-flash",
		},
	}
	if mut != nil {
		mut(cfg)
	}
	clients := make([]*upstream.Client, 0, len(mocks))
	sessions := make([]*session.Manager, 0, len(mocks))
	for i, mock := range mocks {
		cfg.AuthTokens[i] = fmt.Sprintf("tok-%d", i)
		clientCfg := *cfg
		clientCfg.UpstreamBaseURL = mock.URL()
		client, err := upstream.New(cfg.AuthTokens[i], &clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManager(client))
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, clients, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	// Options are applied inside New, BEFORE the dashboard construction, so
	// version/auth-client wiring reaches the embedded SPA (the old
	// newServerOpts did the same).
	serverOpts := make([]Option, 0, len(opts))
	for _, o := range opts {
		serverOpts = append(serverOpts, o)
	}
	srv := New(cfg, p, reg, logger, ring, "", serverOpts...)
	return srv, p
}
