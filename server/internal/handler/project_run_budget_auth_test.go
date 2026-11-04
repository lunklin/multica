package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestProjectRunBudgetAuthenticatedAuthority(t *testing.T) {
	ws, project, fixture := projectBudgetFixture(t)
	otherWS, _, other := projectBudgetFixture(t)
	router := chi.NewRouter()
	router.Use(middleware.Auth(testHandler.Queries, nil, nil, nil))
	router.Route("/api/projects/{id}", func(r chi.Router) {
		r.Get("/run-budget", testHandler.GetProjectRunBudget)
		r.Put("/run-budget", testHandler.ConfigureProjectRunBudget)
	})
	userToken := func(user string) string {
		t.Helper()
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": user, "exp": time.Now().Add(time.Hour).Unix()}).SignedString(auth.JWTSecret())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	request := func(method, token string, body any) *http.Request {
		r := newRequest(method, "/api/projects/"+project+"/run-budget", body)
		r.Header.Set("X-Workspace-ID", ws)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}
	ownerToken := userToken(testUserID)
	initial := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
	testutil.Call(t, router.ServeHTTP, request(http.MethodPut, ownerToken, initial)).Want(http.StatusOK)
	taskToken := func(f *testutil.Fixture, workspace string) string {
		t.Helper()
		runtime := f.Runtime(t, "project budget auth runtime")
		agent := f.Agent(t, "project budget auth agent", runtime)
		task := f.Task(t, agent, testutil.Cols{"runtime_id": runtime, "status": "running"})
		token, err := auth.GenerateAgentTaskToken()
		if err != nil {
			t.Fatal(err)
		}
		f.Insert(t, "task_token", testutil.Cols{"token_hash": auth.HashToken(token), "task_id": task, "agent_id": agent, "workspace_id": workspace, "user_id": testUserID, "expires_at": time.Now().Add(time.Hour)})
		return token
	}
	agentToken, otherToken := taskToken(fixture, ws), taskToken(other, otherWS)
	member := fixture.User(t, "project budget member", fmt.Sprintf("project-budget-member-%d@example.test", time.Now().UnixNano()))
	fixture.Member(t, ws, member, "member")
	memberToken := userToken(member)
	enlarge := map[string]any{"max_runs": 7, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 1}
	for _, tc := range []struct {
		name, method, token string
		body                any
		spoof               bool
		want                int
	}{
		{"owner task may read", http.MethodGet, agentToken, nil, false, http.StatusOK},
		{"owner task cannot configure", http.MethodPut, agentToken, enlarge, false, http.StatusForbidden},
		{"owner task cannot forge human headers", http.MethodPut, agentToken, enlarge, true, http.StatusForbidden},
		{"member may read", http.MethodGet, memberToken, nil, false, http.StatusOK},
		{"member cannot configure", http.MethodPut, memberToken, enlarge, false, http.StatusForbidden},
		{"other workspace task cannot read", http.MethodGet, otherToken, nil, false, http.StatusForbidden},
		{"other workspace task cannot configure", http.MethodPut, otherToken, enlarge, true, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request(tc.method, tc.token, tc.body)
			if tc.spoof {
				r.Header.Set("X-Actor-Source", "member")
				r.Header.Set("X-Agent-ID", "")
				r.Header.Set("X-Task-ID", "")
			}
			testutil.Call(t, router.ServeHTTP, r).Want(tc.want)
		})
	}
	b, err := testHandler.Queries.GetProjectRunBudget(context.Background(), db.GetProjectRunBudgetParams{WorkspaceID: parseUUID(ws), ProjectID: parseUUID(project)})
	if err != nil || b.MaxRuns != 6 || b.Revision != 1 || b.AdmittedRuns != 0 {
		t.Fatalf("rejected credentials changed budget: %+v, err=%v", b, err)
	}
	// Human authentication strips untrusted actor headers rather than treating
	// an owner-supplied X-Agent-ID as a genuine task identity.
	r := request(http.MethodPut, ownerToken, enlarge)
	r.Header.Set("X-Agent-ID", "spoofed-agent")
	r.Header.Set("X-Task-ID", "spoofed-task")
	r.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(t, router.ServeHTTP, r).Want(http.StatusOK)
}

func TestProjectRunBudgetBlocksAgentRecurringRuleEscape(t *testing.T) {
	ws, project, fixture := projectBudgetFixture(t)
	body := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, body)).Want(http.StatusOK)
	runtime := fixture.Runtime(t, "recurring rule source runtime")
	agent := fixture.Agent(t, "recurring rule source agent", runtime)
	issue := fixture.Issue(t, "bounded rule source", testutil.Cols{"project_id": project})
	boundedTask := fixture.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": issue, "status": "running"})
	unboundedTask := fixture.Task(t, agent, testutil.Cols{"runtime_id": runtime, "status": "running"})
	taskToken := func(task string) string {
		t.Helper()
		token, err := auth.GenerateAgentTaskToken()
		if err != nil {
			t.Fatal(err)
		}
		fixture.Insert(t, "task_token", testutil.Cols{"token_hash": auth.HashToken(token), "task_id": task, "agent_id": agent, "workspace_id": ws, "user_id": testUserID, "expires_at": time.Now().Add(time.Hour)})
		return token
	}
	boundedToken, unboundedToken := taskToken(boundedTask), taskToken(unboundedTask)
	owner, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": testUserID, "exp": time.Now().Add(time.Hour).Unix()}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Use(middleware.Auth(testHandler.Queries, nil, nil, nil))
	router.Post("/controlled-autopilot-write", func(w http.ResponseWriter, r *http.Request) {
		if testHandler.refuseBudgetedAgentAutopilotWrite(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for _, tc := range []struct {
		name, token, spoofTask string
		want                   int
	}{
		{"bounded source refused", boundedToken, "", http.StatusForbidden},
		{"bounded source cannot select unbounded task", boundedToken, unboundedTask, http.StatusForbidden},
		{"unbudgeted source unchanged", unboundedToken, "", http.StatusNoContent},
		{"human may manage recurring rules", owner, boundedTask, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRequest(http.MethodPost, "/controlled-autopilot-write", nil)
			r.Header.Set("X-Workspace-ID", ws)
			r.Header.Set("Authorization", "Bearer "+tc.token)
			if tc.spoofTask != "" {
				r.Header.Set("X-Task-ID", tc.spoofTask)
				r.Header.Set("X-Agent-ID", agent)
				r.Header.Set("X-Actor-Source", "task_token")
			}
			testutil.Call(t, router.ServeHTTP, r).Want(tc.want)
		})
	}
}
