package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
)

// TestProbeAccountBanMatchesClassify pins issue #306: ProbeAccount's ban
// conversion (via banFromBody on the session WireBody) produces the SAME
// typed error as the classification matrix (classifyError -> parseBan ->
// banFromBody) for the same upstream payload.
func TestProbeAccountBanMatchesClassify(t *testing.T) {
	body := `{"status":"banned","resumes_at":"2026-08-22T07:00:00Z","message":"account temporarily banned"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client, err := New("tok-0", &config.Config{UpstreamBaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ProbeAccount(context.Background())
	if err == nil {
		t.Fatal("ProbeAccount succeeded, want ErrBanned")
	}

	var probeBan *BanError
	if !errors.As(err, &probeBan) {
		t.Fatalf("ProbeAccount err = %T, want *BanError", err)
	}

	classErr := classifyError(http.StatusForbidden, body, nil)
	var classBan *BanError
	if !errors.As(classErr, &classBan) {
		t.Fatalf("classifyError = %T, want *BanError", classErr)
	}

	if !probeBan.ResumesAt.Equal(classBan.ResumesAt) {
		t.Errorf("ProbeAccount.ResumesAt = %v, classify = %v", probeBan.ResumesAt, classBan.ResumesAt)
	}
	if probeBan.Body != classBan.Body {
		t.Errorf("ProbeAccount.Body = %q, classify = %q", probeBan.Body, classBan.Body)
	}
	wantResumes := time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC)
	if !probeBan.ResumesAt.Equal(wantResumes) {
		t.Errorf("ResumesAt = %v, want %v", probeBan.ResumesAt, wantResumes)
	}
}

// TestProbeAccountCountryBlockedMatchesClassify pins the country_blocked
// conversion parity (issue #306).
func TestProbeAccountCountryBlockedMatchesClassify(t *testing.T) {
	body := `{"status":"country_blocked","countryCode":"CN","countryBlockReason":"country_not_allowed","ipPrivacySignals":["vpn"]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client, err := New("tok-0", &config.Config{UpstreamBaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ProbeAccount(context.Background())
	if err == nil {
		t.Fatal("ProbeAccount succeeded, want ErrCountryBlocked")
	}

	var probeCB *CountryBlockedError
	if !errors.As(err, &probeCB) {
		t.Fatalf("ProbeAccount err = %T, want *CountryBlockedError", err)
	}

	classErr := classifyError(http.StatusForbidden, body, nil)
	var classCB *CountryBlockedError
	if !errors.As(classErr, &classCB) {
		t.Fatalf("classifyError = %T, want *CountryBlockedError", classErr)
	}

	if probeCB.CountryCode != classCB.CountryCode || probeCB.CountryBlockReason != classCB.CountryBlockReason {
		t.Errorf("ProbeAccount = %+v, classify = %+v", probeCB, classCB)
	}
	if len(probeCB.IpPrivacySignals) != len(classCB.IpPrivacySignals) {
		t.Errorf("IpPrivacySignals length mismatch: %v vs %v", probeCB.IpPrivacySignals, classCB.IpPrivacySignals)
	}
}
