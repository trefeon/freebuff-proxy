package dashboard

import (
	"bufio"
	"encoding/json"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHandleEventsStreamInitialSnapshotAndPing verifies the SSE endpoint:
// 1. Sets text/event-stream headers with no-cache and flushes immediately.
// 2. Delivers an initial "tokens" event carrying valid tokensData JSON.
// 3. Connects the client to the hub and respects request context cancellation.
func TestHandleEventsStreamInitialSnapshotAndPing(t *testing.T) {
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
		BridgeEnabled:      true,
	}
	reg := registry.New(cfg, nil)
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := New(func() *config.Config { return cfg }, p, reg, nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(d.HandleEvents))
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}

	rdr := bufio.NewReader(resp.Body)
	// Read until first "data:" line (retry, event: tokens, data: ...)
	var dataLine string
	for i := 0; i < 10; i++ {
		line, err := rdr.ReadString('\n')
		if err != nil {
			t.Fatalf("reading stream (line %d): %v", i, err)
		}
		if strings.HasPrefix(line, "data: ") {
			dataLine = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if dataLine == "" {
		t.Fatal("stream did not emit a data line within 10 lines")
	}

	var td tokensData
	if err := json.Unmarshal([]byte(dataLine), &td); err != nil {
		t.Fatalf("first event data is not valid tokensData JSON: %v\npayload: %s", err, dataLine)
	}
	if td.Mode != "bridge" {
		t.Errorf("initial snapshot Mode = %q, want bridge", td.Mode)
	}
}

// TestEventStreamHubLifecycle verifies the lazy-start / lazy-stop diff loop:
// zero subscribers = no loop; 1 subscriber = loop starts; unsubscribe = stops.
func TestEventStreamHubLifecycle(t *testing.T) {
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
		BridgeEnabled:      true,
	}
	reg := registry.New(cfg, nil)
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := New(func() *config.Config { return cfg }, p, reg, nil, nil)
	h := d.events
	h.mu.Lock()
	if len(h.clients) != 0 || h.stop != nil {
		t.Errorf("hub initial state: %d clients, stop chan %v", len(h.clients), h.stop)
	}
	h.mu.Unlock()

	ch1, cancel1, first1 := h.subscribe(d)
	if first1 == "" || ch1 == nil {
		t.Fatal("subscribe returned nil channel or empty payload")
	}

	h.mu.Lock()
	if len(h.clients) != 1 || h.stop == nil {
		t.Errorf("hub with 1 sub: %d clients, stop chan %v", len(h.clients), h.stop)
	}
	h.mu.Unlock()

	_, cancel2, _ := h.subscribe(d)
	h.mu.Lock()
	if len(h.clients) != 2 {
		t.Errorf("hub with 2 subs: %d clients, want 2", len(h.clients))
	}
	h.mu.Unlock()

	cancel1()
	h.mu.Lock()
	if len(h.clients) != 1 {
		t.Errorf("hub after 1 cancel: %d clients, want 1", len(h.clients))
	}
	h.mu.Unlock()

	cancel2()
	h.mu.Lock()
	if len(h.clients) != 0 || !h.stopped {
		t.Errorf("hub after all canceled: %d clients, stopped=%v (want true)", len(h.clients), h.stopped)
	}
	h.mu.Unlock()
}

func TestTokenStateHashDetectsLimitsChange(t *testing.T) {
	d := &Dashboard{}
	td1 := tokensData{
		Mode:       "pooled",
		TokenCount: 1,
		Tokens: []tokenDetail{
			{
				tokenCard: tokenCard{
					Index:                  0,
					RequestsPerMinuteLimit: 30,
					RequestsPerDayLimit:    1500,
				},
			},
		},
	}
	h1 := d.tokenStateHash(td1)

	td2 := tokensData{
		Mode:       "pooled",
		TokenCount: 1,
		Tokens: []tokenDetail{
			{
				tokenCard: tokenCard{
					Index:                  0,
					RequestsPerMinuteLimit: 15,
					RequestsPerDayLimit:    1000,
				},
			},
		},
	}
	h2 := d.tokenStateHash(td2)
	if h1 == h2 {
		t.Errorf("tokenStateHash did not change when limits changed (%s == %s)", h1, h2)
	}
}
