//go:build attachmentadminintegration && (darwin || linux)

package dbintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicitly selectable offline controls. They do not connect, open sockets,
// initialize a database or silently skip the DB suite when a target is absent.
func TestTargetContract(t *testing.T) {
	if _, err := loadTarget("", "", time.Now()); err == nil {
		t.Fatal("missing opt-in/target accepted")
	}
	if _, err := loadTarget(filepath.Join(t.TempDir(), "missing.json"), optIn, time.Now()); err == nil {
		t.Fatal("unknown target accepted")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "INIT-RECEIPT.json")
	bad := target{Format: 1, Purpose: optIn, Root: dir, Database: "multica", Role: "multica", Nonce: strings.Repeat("a", 64), Port: 55432, PID: 123, SystemID: "1234567890123456789", SchemaSHA: schemaSHA, Expires: time.Now().Add(time.Hour)}
	b, _ := json.Marshal(bad)
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTarget(file, optIn, time.Now()); err == nil {
		t.Fatal("non-SYN namespace accepted")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTarget(link, optIn, time.Now()); err == nil {
		t.Fatal("symlink receipt accepted")
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTarget(file, optIn, time.Now()); err == nil {
		t.Fatal("public receipt accepted")
	}
	t.Run("selection-entry-refusals", func(t *testing.T) {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal("self executable unavailable")
		}
		modes := [][]string{{"-syn-offline", "-test.run=^TestTargetContract$"}, {}, {"-syn-resume-child", "-test.run=^TestResumeChild$"}}
		for _, mode := range modes {
			for _, selection := range []string{"-test.skip=.", "-test.list=.", "-test.count=0"} {
				args := append(append([]string(nil), mode...), selection)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				cmd := exec.CommandContext(ctx, binary, args...)
				cmd.Dir = dir
				cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + dir, "GOMAXPROCS=2", "GOMEMLIMIT=1GiB"}
				child := recordChild(t, ctx, cmd)
				cancel()
				var result struct {
					State string `json:"state"`
				}
				if child.ExitCode == nil || *child.ExitCode != 78 || json.Unmarshal(child.Stdout, &result) != nil || result.State != "test_selection_would_change_required_coverage_NOT_RUN" {
					t.Fatal("actual entry did not reject a selection before target access; see child receipt")
				}
			}
		}
	})
	t.Run("child-start-failure-evidence", func(t *testing.T) {
		cmd := exec.Command(filepath.Join(dir, "missing-SYN-executable"))
		cmd.Dir = dir
		child := recordChild(t, context.Background(), cmd)
		if child.PID != nil || child.ExitCode != nil || child.Terminal != "NOT_STARTED" || child.RunError == "" {
			t.Fatal("startup failure did not retain explicit unknown PID/exit")
		}
	})
	t.Run("child-output-and-timeout-evidence", func(t *testing.T) {
		for _, kind := range []string{"success", "nonzero", "timeout"} {
			t.Run(kind, func(t *testing.T) {
				script := `printf 'SYN-stdout\377'; printf 'SYN-stderr\376' >&2; exit 0`
				if kind == "nonzero" {
					script = `printf 'SYN-stdout\377'; printf 'SYN-stderr\376' >&2; exit 23`
				}
				if kind == "timeout" {
					script = `printf 'SYN-stdout\377'; printf 'SYN-stderr\376' >&2; exec /bin/sleep 5`
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
				cmd.Dir = dir
				cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + dir, "GOMAXPROCS=2", "GOMEMLIMIT=1GiB"}
				child := recordChild(t, ctx, cmd)
				var decoded childReceipt
				encoded, err := json.Marshal(child)
				if err != nil || json.Unmarshal(encoded, &decoded) != nil || !bytes.Equal(decoded.Stdout, append([]byte("SYN-stdout"), 0xff)) || !bytes.Equal(decoded.Stderr, append([]byte("SYN-stderr"), 0xfe)) {
					t.Fatal("child streams were not preserved losslessly")
				}
				if child.PID == nil || child.Finished.Before(child.Started) {
					t.Fatal("child command identity or times missing")
				}
				if kind == "timeout" {
					if child.ContextError == "" || child.RunError == "" || child.ExitCode != nil || child.Terminal != "TERMINAL_WITHOUT_EXIT_CODE" {
						t.Fatal("timeout terminal receipt incomplete")
					}
				} else {
					want := 0
					if kind == "nonzero" {
						want = 23
					}
					if child.ExitCode == nil || *child.ExitCode != want || child.Terminal != "EXITED" || (want == 0) != (child.RunError == "") {
						t.Fatal("child exit receipt incorrect")
					}
				}
			})
		}
	})
}
