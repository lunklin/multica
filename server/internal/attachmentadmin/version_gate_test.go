package attachmentadmin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCLICommitGateStopsEveryFollowup(t *testing.T) {
	binary, _ := testCLIBinary(t)
	childDir := t.TempDir()
	for _, commit := range []string{"", "unknown", "UNKNOWN", "abc123", " ", strings.Repeat("a", 39), strings.Repeat("g", 40)} {
		t.Run("commit="+commit, func(t *testing.T) {
			var followups atomic.Int32
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != Prefix+"version" {
					followups.Add(1)
					w.WriteHeader(500)
					return
				}
				respond(w, Version{Build: "SYN-known-build", Capability: Capability, Commit: commit}, nil)
			})
			server := httptest.NewServer(h)
			defer server.Close()
			for _, args := range [][]string{{"attachment", "inspect", id(10)}, {"attachment", "delete", id(10), "--dry-run", "--scope-file", "must-not-open.json"}, {"attachment", "delete", id(10), "--confirm-plan", id(98), "--request-id", id(99)}} {
				cmd := exec.Command(binary, args...)
				cmd.Dir = childDir
				cmd.Env = []string{"MULTICA_SERVER_URL=" + server.URL, "MULTICA_WORKSPACE_ID=" + id(1), "MULTICA_TOKEN=SYN-owner"}
				var stdout, stderr bytes.Buffer
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr
				start := time.Now().UTC()
				cwd := childDir
				err := cmd.Run()
				end := time.Now().UTC()
				out := stdout.Bytes()
				code := 0
				if e, ok := err.(*exec.ExitError); ok {
					code = e.ExitCode()
				}
				t.Logf("CLI commit-gate commit=%q utc_start=%s utc_end=%s cwd=%q pid=%d argv=%q exit=%d stdout=%s stderr=%s", commit, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), cwd, cmd.Process.Pid, args, code, out, stderr.Bytes())
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 5 {
					t.Fatalf("invalid commit accepted: %q %s %v", commit, out, err)
				}
				if !strings.Contains(string(out), "unsupported_server_capability") {
					t.Fatalf("wrong error %s", out)
				}
			}
			if followups.Load() != 0 {
				t.Fatal("unknown commit caused followup calls")
			}
		})
	}
}

func TestCLIValidCommitControl(t *testing.T) {
	var following atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == Prefix+"version" {
			respond(w, Version{Build: "SYN-build", Capability: Capability, Commit: "1f515002a9940b6fb3cb9658822970ef9426c91c"}, nil)
		} else {
			following.Add(1)
			respond(w, map[string]bool{"observed": true}, nil)
		}
	}))
	defer server.Close()
	var out strings.Builder
	exit := RunCLI(context.Background(), []string{"attachment", "inspect", id(10)}, CLIConfig{Server: server.URL, Workspace: id(1), Token: "SYN-owner"}, &out, &out)
	if exit != 0 || following.Load() != 1 {
		t.Fatal(exit, following.Load(), out.String())
	}
}
