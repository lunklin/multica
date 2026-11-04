package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

const workflowCLIProject = "11111111-1111-1111-1111-111111111111"
const workflowCLIWorkspace = "22222222-2222-2222-2222-222222222222"
const workflowCLIID = "33333333-3333-3333-3333-333333333333"

func projectWorkflowTestCommand(t *testing.T, server string) *cobra.Command {
	t.Helper()
	redirectTestHome(t, t.TempDir())
	for name, value := range map[string]string{
		"MULTICA_SERVER_URL": server, "MULTICA_WORKSPACE_ID": workflowCLIWorkspace,
		"MULTICA_TOKEN": "mat_workflow_read_fixture", "MULTICA_TASK_ID": "44444444-4444-4444-4444-444444444444",
		"MULTICA_AGENT_ID": "55555555-5555-5555-5555-555555555555", "MULTICA_DAEMON_PORT": "", "MULTICA_TASK_CONFIG_ROOT": "",
	} {
		t.Setenv(name, value)
	}
	command := newProjectWorkflowCommand()
	command.SilenceUsage, command.SilenceErrors = true, true
	return command
}

func workflowCLIResponse(isDefault bool) map[string]any {
	return map[string]any{
		"id": workflowCLIID, "workspace_id": workflowCLIWorkspace, "name": "Daily workflow", "is_default": isDefault,
		"future_workflow_policy": map[string]any{"schema": "future", "revision": 9},
		"steps": []map[string]any{
			{"status_key": "build", "handler": map[string]any{"type": "agent", "id": "engineer", "future_handler_policy": "keep"}, "instructions": "Implement only the accepted scope.\nKeep the author receipt.", "next_status_key": "review", "future_step_rule": true},
			{"status_key": "review", "handler": map[string]any{"type": "agent", "id": "reviewer"}, "instructions": "Review exact commit independently.", "next_status_key": "human", "back_status_key": "build"},
			{"status_key": "human", "handler": map[string]any{"type": "member", "id": "requester"}, "instructions": "Wait for human check."},
		},
	}
}

func workflowCLICatalog() map[string]any {
	return map[string]any{"statuses": []map[string]any{
		{"key": "build", "name": "日常開發：實作"}, {"key": "review", "name": "日常開發：獨立審查"},
		{"key": "human", "name": "日常開發：人工檢查"}, {"key": "unrelated", "name": "Other lane"},
	}}
}

func TestProjectWorkflowCommandIsRegistered(t *testing.T) {
	get, remaining, err := rootCmd.Find([]string{"project", "workflow", "get", workflowCLIProject})
	if err != nil || get.CommandPath() != "multica project workflow get" || !reflect.DeepEqual(remaining, []string{workflowCLIProject}) {
		t.Fatalf("command missing from real tree: %v %v %v", get, remaining, err)
	}
	if flag := get.Flags().Lookup("output"); flag == nil || flag.DefValue != "json" {
		t.Fatal("workflow get must default to JSON")
	}
}

func TestProjectWorkflowCLIReadbackPreservesRoutesAndTaskScope(t *testing.T) {
	for _, tc := range []struct {
		name, ref string
		unbound   bool
	}{
		{"named", workflowCLIProject, false}, {"existing prefix resolver", workflowCLIProject[:8], false}, {"workspace Default", workflowCLIProject, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workflow := workflowCLIResponse(tc.unbound)
			var mu sync.Mutex
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				if r.Method != http.MethodGet {
					t.Errorf("read command attempted %s", r.Method)
					w.WriteHeader(405)
					return
				}
				if r.Header.Get("Authorization") != "Bearer mat_workflow_read_fixture" || r.Header.Get("X-Workspace-ID") != workflowCLIWorkspace || r.Header.Get("X-Task-ID") == "" || r.Header.Get("X-Agent-ID") == "" {
					t.Error("existing CLI task authentication/tenant headers were not preserved")
				}
				var response any
				switch r.URL.Path {
				case "/api/projects":
					response = map[string]any{"projects": []map[string]any{{"id": workflowCLIProject, "title": "Product"}}}
				case "/api/projects/" + workflowCLIProject:
					var binding any = workflowCLIID
					if tc.unbound {
						binding = nil
					}
					response = map[string]any{"id": workflowCLIProject, "workflow_id": binding}
				case "/api/issue-workflows/" + workflowCLIID:
					response = workflow
				case "/api/issue-workflows":
					response = map[string]any{"workflows": []any{workflowCLIResponse(false), workflow}}
				case "/api/issue-statuses":
					if r.URL.Query().Get("include_archived") != "true" {
						t.Error("status lookup omitted archived-name support")
					}
					response = workflowCLICatalog()
				default:
					t.Errorf("unexpected GET %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			command := projectWorkflowTestCommand(t, server.URL)
			command.SetArgs([]string{"get", tc.ref, "--output", "json"})
			out, err := captureStdout(t, command.Execute)
			if err != nil {
				t.Fatal(err)
			}
			var result projectWorkflowReadback
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
			source := "project"
			workflowPath := "/api/issue-workflows/" + workflowCLIID
			if tc.unbound {
				source = "default"
				workflowPath = "/api/issue-workflows"
			}
			if result.ProjectID != workflowCLIProject || result.ResolvedSource != source {
				t.Fatalf("wrong resolution: %s", out)
			}
			var got map[string]any
			if err := json.Unmarshal(result.Workflow, &got); err != nil {
				t.Fatal(err)
			}
			wantJSON, _ := json.Marshal(workflow)
			var want map[string]any
			json.Unmarshal(wantJSON, &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("server workflow/handler/unknown fields changed: got=%v want=%v", got, want)
			}
			if !reflect.DeepEqual(result.StatusNames, map[string]string{"build": "日常開發：實作", "review": "日常開發：獨立審查", "human": "日常開發：人工檢查"}) {
				t.Fatalf("wrong names: %v", result.StatusNames)
			}
			wantPaths := []string{"/api/projects/" + workflowCLIProject, workflowPath, "/api/issue-statuses"}
			if tc.ref != workflowCLIProject {
				wantPaths = append([]string{"/api/projects"}, wantPaths...)
			}
			mu.Lock()
			gotPaths := append([]string(nil), paths...)
			mu.Unlock()
			if !reflect.DeepEqual(gotPaths, wantPaths) {
				t.Fatalf("wrong GET sequence: %v want %v", gotPaths, wantPaths)
			}
		})
	}
}

func TestProjectWorkflowCLIFailsClosedWithoutInventingWorkflow(t *testing.T) {
	for _, tc := range []struct {
		name, failAt, body, contains string
		status                       int
		unbound                      bool
	}{
		{"project missing", "project", `{"error":"missing"}`, "get project", 404, false},
		{"binding field missing", "project", `{"id":"` + workflowCLIProject + `"}`, "workflow_id", 200, false},
		{"binding malformed", "project", `{"id":"` + workflowCLIProject + `","workflow_id":[]}`, "workflow_id", 200, false},
		{"named workflow missing does not fall back", "workflow", `{"error":"missing"}`, "get project workflow", 404, false},
		{"malformed workflow JSON", "workflow", `{`, "get project workflow", 200, false},
		{"steps missing", "workflow", `{"id":"` + workflowCLIID + `","is_default":false}`, "nonempty steps", 200, false},
		{"steps null", "workflow", `{"id":"` + workflowCLIID + `","is_default":false,"steps":null}`, "nonempty steps", 200, false},
		{"steps empty", "workflow", `{"id":"` + workflowCLIID + `","is_default":false,"steps":[]}`, "nonempty steps", 200, false},
		{"handler missing", "workflow", `{"id":"` + workflowCLIID + `","is_default":false,"steps":[{"status_key":"build","instructions":"x"}]}`, "handler", 200, false},
		{"agent handler id missing", "workflow", `{"id":"` + workflowCLIID + `","is_default":false,"steps":[{"status_key":"build","handler":{"type":"agent"},"instructions":"x"}]}`, "handler id", 200, false},
		{"route malformed", "workflow", `{"id":"` + workflowCLIID + `","is_default":false,"steps":[{"status_key":"build","handler":{"type":"none"},"instructions":"","next_status_key":42}]}`, "invalid workflow step", 200, false},
		{"Default absent", "list", `{"workflows":[]}`, "found 0", 200, true},
		{"Default ambiguous", "list", `{"workflows":[{"is_default":true},{"is_default":true}]}`, "found 2", 200, true},
		{"Default list malformed", "list", `{"workflows":[{}]}`, "is_default", 200, true},
		{"status catalog unavailable", "catalog", `{"error":"missing"}`, "get workflow status names", 404, false},
		{"status names missing", "catalog", `{"statuses":[]}`, "missing workflow step", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected write %s", r.Method)
					w.WriteHeader(405)
					return
				}
				stage := ""
				var response any
				switch r.URL.Path {
				case "/api/projects/" + workflowCLIProject:
					stage = "project"
					var binding any = workflowCLIID
					if tc.unbound {
						binding = nil
					}
					response = map[string]any{"id": workflowCLIProject, "workflow_id": binding}
				case "/api/issue-workflows/" + workflowCLIID:
					stage = "workflow"
					response = workflowCLIResponse(false)
				case "/api/issue-workflows":
					stage = "list"
					if !tc.unbound {
						t.Error("named workflow failure fell back to Default")
					}
					response = map[string]any{"workflows": []any{workflowCLIResponse(true)}}
				case "/api/issue-statuses":
					stage = "catalog"
					response = workflowCLICatalog()
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if stage == tc.failAt {
					w.WriteHeader(tc.status)
					w.Write([]byte(tc.body))
					return
				}
				json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			command := projectWorkflowTestCommand(t, server.URL)
			command.SetArgs([]string{"get", workflowCLIProject})
			out, err := captureStdout(t, command.Execute)
			if err == nil || !strings.Contains(err.Error(), tc.contains) || out != "" {
				t.Fatalf("expected fail-closed %q with no JSON; err=%v stdout=%s", tc.contains, err, out)
			}
		})
	}
}

func TestProjectWorkflowCLIRejectsUnsupportedOutputBeforeHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid output made HTTP request") }))
	defer server.Close()
	command := projectWorkflowTestCommand(t, server.URL)
	command.SetArgs([]string{"get", workflowCLIProject, "--output", "table"})
	out, err := captureStdout(t, command.Execute)
	if err == nil || !strings.Contains(err.Error(), "only --output json") || out != "" {
		t.Fatalf("invalid output accepted: %v %s", err, out)
	}
}
