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
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["project_id"] != project || result["remaining_runs"] != float64(6) {
		t.Fatalf("invalid response output: %s, err=%v", out, err)
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
