package attachmentadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFormalCLI(t *testing.T) {
	binary := os.Getenv("ADMIN_BINARY")
	if binary == "" {
		t.Skip("ADMIN_BINARY required: build formal standalone distribution first")
	}
	s, f, o, _, scope := setup(t)
	var lost atomic.Bool
	api := HTTP{Service: s, Actor: func(r *http.Request) (Actor, error) {
		a := Actor{ID: id(3), Workspace: r.Header.Get("X-Workspace-ID"), Human: true}
		switch r.Header.Get("Authorization") {
		case "Bearer SYN-owner":
		case "Bearer SYN-member":
			a.ID = id(99)
		case "Bearer SYN-task-owner":
			a.Human = false
		default:
			return a, fail("unauthenticated", 401)
		}
		return a, nil
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/confirm") && lost.CompareAndSwap(true, false) {
			rec := httptest.NewRecorder()
			api.ServeHTTP(rec, r)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		api.ServeHTTP(w, r)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "scope.json")
	b, _ := json.Marshal(scope)
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	run := func(label, token, workspace string, want int, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Env = []string{"MULTICA_SERVER_URL=" + server.URL, "MULTICA_WORKSPACE_ID=" + workspace, "MULTICA_TOKEN=" + token}
		var stdoutBuffer, stderrBuffer bytes.Buffer
		cmd.Stdout = &stdoutBuffer
		cmd.Stderr = &stderrBuffer
		cwd, _ := os.Getwd()
		start := time.Now().UTC()
		err := cmd.Run()
		end := time.Now().UTC()
		out := stdoutBuffer.Bytes()
		stderr := stderrBuffer.Bytes()
		exit := 0
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatal(err)
			}
			exit = ee.ExitCode()
		}
		t.Logf("CLI %s utc_start=%s utc_end=%s cwd=%q pid=%d argv=%q exit=%d stdout=%s stderr=%s", label, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), cwd, cmd.Process.Pid, args, exit, out, stderr)
		if exit != want {
			t.Fatalf("%s exit %d want %d", label, exit, want)
		}
		if !json.Valid(out) && !(len(args) > 0 && args[0] == "--help") {
			t.Fatalf("invalid JSON: %s", out)
		}
		for _, secret := range []string{"SYN-owner", "SYN-task-owner", "syn-object-"} {
			if strings.Contains(string(out), secret) || strings.Contains(string(stderr), secret) {
				t.Fatal("private locator or credential in CLI sink")
			}
		}
		return out
	}
	run("help-no-profile", "", "", 0, "--help")
	run("local-version-no-auth", "", "", 0, "version")
	run("server-capability", "SYN-owner", id(1), 0, "version", "--server", "--output", "json")
	run("inspect", "SYN-owner", id(1), 0, "attachment", "inspect", id(10))
	run("invalid-id", "SYN-owner", id(1), 5, "attachment", "inspect", "bad")
	run("non-owner", "SYN-member", id(1), 3, "attachment", "inspect", id(10))
	run("task-owner-server-denial", "SYN-task-owner", id(1), 3, "attachment", "inspect", id(10))
	run("task-token-local-denial", "mat_SYN", id(1), 3, "attachment", "inspect", id(10))
	run("cross-workspace", "SYN-owner", id(99), 3, "attachment", "inspect", id(10))
	run("unknown-receipt-is-not-success", "SYN-owner", id(1), 4, "attachment", "operation", "get", id(100))
	f.Lock()
	m := f.image.Attachments[id(10)]
	m.Captured = true
	f.image.Attachments[id(10)] = m
	f.Unlock()
	run("captured-inspect", "SYN-owner", id(1), 0, "attachment", "inspect", id(10))
	run("captured-refusal", "SYN-owner", id(1), 5, "attachment", "delete", id(10), "--dry-run", "--scope-file", file)
	f.Lock()
	m.Captured = false
	f.image.Attachments[id(10)] = m
	f.Unlock()
	var p Plan
	if err := json.Unmarshal(run("dry-run", "SYN-owner", id(1), 0, "attachment", "delete", id(10), "--dry-run", "--scope-file", file), &p); err != nil {
		t.Fatal(err)
	}
	f.Lock()
	o.Lock()
	if len(f.image.Attachments) != 16 || o.calls != 0 {
		t.Fatal("dry-run removed data")
	}
	o.Unlock()
	f.Unlock()
	f.Lock()
	m.Filename = "SYN-drift"
	f.image.Attachments[id(10)] = m
	f.Unlock()
	run("drift-no-change", "SYN-owner", id(1), 5, "attachment", "delete", id(10), "--confirm-plan", p.ID, "--request-id", id(99))
	run("rejected-readback", "SYN-owner", id(1), 0, "attachment", "operation", "get", id(99))
	f.Lock()
	m.Filename = "SYN-10.txt"
	f.image.Attachments[id(10)] = m
	f.Unlock()
	o.Lock()
	o.fail = true
	o.Unlock()
	run("storage-partial", "SYN-owner", id(1), 1, "attachment", "delete", id(10), "--confirm-plan", p.ID, "--request-id", id(100))
	run("pending-readback", "SYN-owner", id(1), 0, "attachment", "operation", "get", id(100))
	o.Lock()
	o.fail = false
	o.Unlock()
	run("same-id-resume", "SYN-owner", id(1), 0, "attachment", "delete", id(10), "--confirm-plan", p.ID, "--request-id", id(100))
	run("same-id-reentry", "SYN-owner", id(1), 0, "attachment", "delete", id(10), "--confirm-plan", p.ID, "--request-id", id(100))
	f.Lock()
	if len(f.image.Attachments) != 15 || len(f.image.Comments) != 1 {
		t.Fatal("unrelated data removed")
	}
	f.Unlock()
	var p2 Plan
	if err := json.Unmarshal(run("unbound-preview", "SYN-owner", id(1), 0, "attachment", "delete", id(11), "--dry-run", "--scope-file", file), &p2); err != nil {
		t.Fatal(err)
	}
	lost.Store(true)
	run("committed-response-lost", "SYN-owner", id(1), 2, "attachment", "delete", id(11), "--confirm-plan", p2.ID, "--request-id", id(101))
	got := run("lost-response-read-same-id", "SYN-owner", id(1), 0, "attachment", "operation", "get", id(101))
	var op Operation
	if json.Unmarshal(got, &op) != nil || op.State != Removed {
		t.Fatal("ambiguous response not reconciled")
	}
	run("lost-response-safe-reentry", "SYN-owner", id(1), 0, "attachment", "delete", id(11), "--confirm-plan", p2.ID, "--request-id", id(101))
	f.Lock()
	o.Lock()
	defer f.Unlock()
	defer o.Unlock()
	if len(f.image.Attachments) != 14 || len(f.image.Comments) != 1 || len(o.objects) != 14 {
		t.Fatal("wrong final delta")
	}
}

func TestUnsupportedServerAndHelpNoNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"server_version":"legacy","capability":"none"}`))
	}))
	defer server.Close()
	var out strings.Builder
	c := CLIConfig{Server: server.URL, Workspace: id(1), Token: "SYN-owner"}
	exit := RunCLI(context.Background(), []string{"attachment", "inspect", id(10)}, c, &out, &out)
	if exit != 5 || calls != 1 || !strings.Contains(out.String(), "unsupported_server_capability") {
		t.Fatal(exit, calls, out.String())
	}
	before := calls
	if RunCLI(context.Background(), []string{"--help"}, CLIConfig{Machine: true}, &out, &out) != 0 || calls != before {
		t.Fatal("help made an authenticated call")
	}
}
