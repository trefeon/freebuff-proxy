package session

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// adoptionManager wires a manager with CLI adoption enabled. ownerFile is
// the freebuff-instance-owner.json path (does not need to exist yet).
// alive overrides the PID liveness check (nil = platform check).
func adoptionManager(t *testing.T, mock *testutil.MockUpstream, ownerFile string, owner CLIOwner, alive func(int) bool) *Manager {
	t.Helper()
	mgr := newTestManager(t, mock)
	mgr.SetCLIAdoption(CLIAdoption{Enabled: true, OwnerFile: ownerFile, Initial: owner})
	if alive != nil {
		mgr.adopt.testAlive = alive
	}
	return mgr
}

func writeOwnerFile(t *testing.T, path string, owner CLIOwner) {
	t.Helper()
	data := `{"instanceId":"` + owner.InstanceID + `","pid":` + strconv.Itoa(owner.PID) + `}`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAdoptDisabledCreatesNormally verifies the baseline: without adoption,
// EnsureSession creates a session.
func TestAdoptDisabledCreatesNormally(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != mock.InstanceID {
		t.Errorf("instance = %q, want %q", instance, mock.InstanceID)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1", mock.SessionCreates)
	}
}

// TestAdoptMissingOwnerFileRefuses verifies issue #97(c): adoption enabled
// but freebuff-instance-owner.json missing → clear refusal error, NO
// competing session create.
func TestAdoptMissingOwnerFileRefuses(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := adoptionManager(t, mock, filepath.Join(t.TempDir(), "freebuff-instance-owner.json"), CLIOwner{}, nil)

	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("EnsureSession succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "refusing to create a competing session") {
		t.Errorf("err = %v, want refusal message", err)
	}
	if mock.SessionCreates != 0 {
		t.Errorf("creates = %d, want 0 (must never create a competing session)", mock.SessionCreates)
	}
}

// TestAdoptAdoptsLiveCLISession verifies issue #97(b): with the CLI process
// alive, the manager polls (GET) the CLI's instance and adopts it — never
// POSTs a competing session create.
func TestAdoptAdoptsLiveCLISession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// The mock echoes its own instance id — pin it to the CLI's.
	mock.InstanceID = "cli-inst-0001"
	ownerFile := filepath.Join(t.TempDir(), "owner.json")
	writeOwnerFile(t, ownerFile, CLIOwner{InstanceID: "cli-inst-0001", PID: 4242})
	mgr := adoptionManager(t, mock, ownerFile, CLIOwner{}, func(int) bool { return true })

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "cli-inst-0001" {
		t.Errorf("instance = %q, want adopted cli-inst-0001", instance)
	}
	if mock.SessionCreates != 0 {
		t.Errorf("creates = %d, want 0 (adoption must never POST a competing session)", mock.SessionCreates)
	}
	if mock.SessionPolls == 0 {
		t.Error("polls = 0, want at least one GET to verify the CLI session")
	}
}

// TestAdoptDeadCLICreates verifies the fallback: the CLI process is not
// running (pid dead), so the proxy creates its own session.
func TestAdoptDeadCLICreates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ownerFile := filepath.Join(t.TempDir(), "owner.json")
	writeOwnerFile(t, ownerFile, CLIOwner{InstanceID: "cli-inst-0001", PID: 4242})
	mgr := adoptionManager(t, mock, ownerFile, CLIOwner{}, func(int) bool { return false })

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1 (CLI dead → create)", mock.SessionCreates)
	}
	if instance == "" {
		t.Error("instance empty after create")
	}
}

// TestAdoptModelMismatchRefuses verifies a CLI session bound to another
// model refuses with a clear error (reference sessions.js:143), while the
// matching model adopts.
func TestAdoptModelMismatchRefuses(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ownerFile := filepath.Join(t.TempDir(), "owner.json")
	writeOwnerFile(t, ownerFile, CLIOwner{InstanceID: "cli-inst-0001", PID: 4242})
	// Custom handler: the CLI session is active for deepseek-v4-pro.
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"status":     "active",
			"instanceId": "cli-inst-0001",
			"model":      "deepseek/deepseek-v4-pro",
			"expiresAt":  time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		})
	}
	mgr := adoptionManager(t, mock, ownerFile, CLIOwner{}, func(int) bool { return true })

	// Wrong model: refuse, never create.
	_, err := mgr.EnsureSessionForModel(context.Background(), "z-ai/glm-5.2")
	if err == nil {
		t.Fatal("EnsureSessionForModel(glm) succeeded, want model-mismatch refusal")
	}
	if !strings.Contains(err.Error(), "refusing to create a competing session") {
		t.Errorf("err = %v, want refusal message", err)
	}
	if mock.SessionCreates != 0 {
		t.Errorf("creates = %d, want 0", mock.SessionCreates)
	}

	// Matching model: adopt.
	instance, err := mgr.EnsureSessionForModel(context.Background(), "deepseek/deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	if instance != "cli-inst-0001" {
		t.Errorf("instance = %q, want cli-inst-0001", instance)
	}
}

// TestAdoptReReadsOwnerFile verifies issue #97(c): the owner file is
// re-read before every refresh — a CLI restart (new instance + live pid)
// is adopted instead of the stale startup snapshot.
func TestAdoptReReadsOwnerFile(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ownerFile := filepath.Join(t.TempDir(), "owner.json")
	writeOwnerFile(t, ownerFile, CLIOwner{InstanceID: "cli-inst-0001", PID: 9999})
	mgr := adoptionManager(t, mock, ownerFile, CLIOwner{InstanceID: "cli-inst-0001", PID: 9999}, func(int) bool { return false })

	// CLI dead: first Ensure creates the proxy's own session.
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 1 {
		t.Fatalf("creates = %d, want 1", mock.SessionCreates)
	}

	// CLI restarts: owner file rewritten to a NEW instance + live pid.
	// The next refresh must adopt it instead of re-creating.
	mock.InstanceID = "cli-inst-0002"
	writeOwnerFile(t, ownerFile, CLIOwner{InstanceID: "cli-inst-0002", PID: 12345})
	mgr.Invalidate()
	mock.SessionCreates = 0
	mgr.adopt.testAlive = func(int) bool { return true }

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "cli-inst-0002" {
		t.Errorf("instance = %q, want re-read cli-inst-0002", instance)
	}
	if mock.SessionCreates != 0 {
		t.Errorf("creates = %d, want 0 (re-read owner adopted, no create)", mock.SessionCreates)
	}
}
