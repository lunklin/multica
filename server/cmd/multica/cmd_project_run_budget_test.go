package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
)

func projectBudgetTestCommand(t *testing.T, name, server string) *cobra.Command {
	t.Helper()
	redirectTestHome(t, t.TempDir())
	for name, value := range map[string]string{
		"MULTICA_SERVER_URL": server, "MULTICA_WORKSPACE_ID": "22222222-2222-2222-2222-222222222222", "MULTICA_TOKEN": "test-token",
		"MULTICA_TASK_ID": "", "MULTICA_AGENT_ID": "", "MULTICA_DAEMON_PORT": "", "MULTICA_TASK_CONFIG_ROOT": "",
	} {
		t.Setenv(name, value)
	}
	cmd, _, err := newProjectRunBudgetCommand().Find([]string{name})
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestProjectRunBudgetCLIResolvesProjectAndPreservesAllowanceContract(t *testing.T) {
	const project = "11111111-1111-1111-1111-111111111111"
	var body map[string]any
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Workspace-ID") != "22222222-2222-2222-2222-222222222222" {
			t.Errorf("workspace scope missing: %q", r.Header.Get("X-Workspace-ID"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			json.NewEncoder(w).Encode(map[string]any{"projects": []map[string]any{{"id": project, "title": "bounded project"}}})
		case r.Method == http.MethodPut && r.URL.Path == "/api/projects/"+project+"/run-budget":
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"project_id": project, "configured": true, "remaining_runs": 6, "revision": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cmd := projectBudgetTestCommand(t, "set", server.URL)
	for flag, value := range map[string]string{"max-runs": "6", "expected-revision": "0", "deadline": "2030-01-01T00:00:00Z", "paused": "true"} {
		if err := cmd.Flags().Set(flag, value); err != nil {
			t.Fatal(err)
		}
	}
	out, err := captureStdout(t, func() error { return runProjectBudgetSet(cmd, []string{project[:8]}) })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || body["max_runs"] != float64(6) || body["expected_revision"] != float64(0) || body["deadline"] != "2030-01-01T00:00:00Z" || body["paused"] != true {
		t.Fatalf("wrong project or allowance request: calls=%d, body=%v", calls, body)
	}
	if _, ok := body["admitted_runs"]; ok {
		t.Fatal("CLI attempted to reset charged admissions")
	}
	if _, ok := body["stop_on_failure"]; ok {
		t.Fatal("CLI overwrote an existing failure policy without its flag")
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["project_id"] != project || result["remaining_runs"] != float64(6) {
		t.Fatalf("invalid response output: %s, err=%v", out, err)
	}
}

func TestProjectRunBudgetCLIFailurePolicyAndExplicitResumeFlags(t *testing.T) {
	const project = "11111111-1111-1111-1111-111111111111"
	for _, tc := range []struct {
		name, policy, pause string
	}{
		{"policy omitted", "", ""},
		{"enable policy", "true", ""},
		{"disable policy", "false", ""},
		{"explicit failure resume", "", "false"},
		{"enable and pause", "true", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/api/projects/"+project+"/run-budget" {
					http.NotFound(w, r)
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode failure-policy request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"project_id": project, "configured": true, "revision": 6})
			}))
			defer server.Close()
			cmd := projectBudgetTestCommand(t, "set", server.URL)
			for key, value := range map[string]string{"max-runs": "12", "expected-revision": "5", "deadline": "2030-01-01T00:00:00Z"} {
				if err := cmd.Flags().Set(key, value); err != nil {
					t.Fatal(err)
				}
			}
			for key, value := range map[string]string{"stop-on-failure": tc.policy, "paused": tc.pause} {
				if value != "" {
					if err := cmd.Flags().Set(key, value); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := captureStdout(t, func() error { return runProjectBudgetSet(cmd, []string{project}) }); err != nil {
				t.Fatal(err)
			}
			policy, policySet := body["stop_on_failure"]
			if policySet != (tc.policy != "") || (policySet && policy != (tc.policy == "true")) {
				t.Fatalf("failure policy flag lost explicitness: %v", body)
			}
			pause, pauseSet := body["paused"]
			if pauseSet != (tc.pause != "") || (pauseSet && pause != (tc.pause == "true")) {
				t.Fatalf("CLI implicitly acknowledged a failure latch: %v", body)
			}
			for _, readonly := range []string{"admitted_runs", "failure_task_id", "clear_failure_latch"} {
				if _, found := body[readonly]; found {
					t.Fatalf("CLI sent internal/readonly field %q", readonly)
				}
			}
		})
	}
}

func TestProjectRunBudgetCLIGetPreservesFailurePolicyAndCause(t *testing.T) {
	const project = "11111111-1111-1111-1111-111111111111"
	const failed = "33333333-3333-3333-3333-333333333333"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/projects/"+project+"/run-budget" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"project_id": project, "configured": true,
			"stop_on_failure": true, "failure_task_id": failed, "stop_reason": "failure_latch", "remaining_runs": 5})
	}))
	defer server.Close()
	cmd := projectBudgetTestCommand(t, "get", server.URL)
	out, err := captureStdout(t, func() error { return runProjectBudgetGet(cmd, []string{project}) })
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["stop_on_failure"] != true || result["failure_task_id"] != failed || result["stop_reason"] != "failure_latch" || result["remaining_runs"] != float64(5) {
		t.Fatalf("CLI discarded new server failure fields: %s, err=%v", out, err)
	}
}

func TestProjectRunBudgetCLIGetPreservesUnconfiguredState(t *testing.T) {
	const project = "11111111-1111-1111-1111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/projects/"+project+"/run-budget" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"project_id": project, "configured": false, "max_runs": nil, "remaining_runs": nil})
	}))
	defer server.Close()
	cmd := projectBudgetTestCommand(t, "get", server.URL)
	out, err := captureStdout(t, func() error { return runProjectBudgetGet(cmd, []string{project}) })
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["configured"] != false || result["max_runs"] != nil || result["remaining_runs"] != nil {
		t.Fatalf("unconfigured allowance was misrepresented: %s, err=%v", out, err)
	}
}
