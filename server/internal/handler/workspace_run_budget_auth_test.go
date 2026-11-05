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
)

// Exercise the same authentication and workspace gates as the API routes,
// rather than supplying the handler with simulated post-authentication headers.
func TestWorkspaceRunBudgetAuthenticatedAuthority(t *testing.T) {
	ctx := context.Background()
	workspace, f := budgetFixture(t)
	otherWorkspace, other := budgetFixture(t)
	router := chi.NewRouter()
	router.Use(middleware.Auth(testHandler.Queries, nil, nil, nil))
	router.Route("/api/workspaces/{id}", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireWorkspaceMemberFromURL(testHandler.Queries, "id"))
			r.Get("/run-budget", testHandler.GetWorkspaceRunBudget)
		})
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireWorkspaceRoleFromURL(testHandler.Queries, "id", "owner", "admin"))
			r.Put("/run-budget", testHandler.ConfigureWorkspaceRunBudget)
		})
	})
	sessionToken := func(user string) string {
		t.Helper()
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": user, "exp": time.Now().Add(time.Hour).Unix(),
		}).SignedString(auth.JWTSecret())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	request := func(method, target, token string, body any) *http.Request {
		r := newRequest(method, "/api/workspaces/"+target+"/run-budget", body)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Del("X-Workspace-ID")
		return r
	}
	ownerToken := sessionToken(testUserID)
	initial := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
	for _, ws := range []string{workspace, otherWorkspace} {
		testutil.Call(t, router.ServeHTTP, request(http.MethodPut, ws, ownerToken, initial)).Want(http.StatusOK)
	}
	taskToken := func(fixture *testutil.Fixture, ws string) string {
		t.Helper()
		runtime := fixture.Runtime(t, "budget auth runtime")
		agent := fixture.Agent(t, "budget auth agent", runtime)
		task := fixture.Task(t, agent, testutil.Cols{"runtime_id": runtime, "status": "running"})
		token, err := auth.GenerateAgentTaskToken()
		if err != nil {
			t.Fatal(err)
		}
		fixture.Insert(t, "task_token", testutil.Cols{
			"token_hash": auth.HashToken(token), "task_id": task, "agent_id": agent,
			"workspace_id": ws, "user_id": testUserID, "expires_at": time.Now().Add(time.Hour),
		})
		return token
	}
	agentToken := taskToken(f, workspace)
	otherAgentToken := taskToken(other, otherWorkspace)
	member := f.User(t, "Budget ordinary member", fmt.Sprintf("budget-member-%d@example.test", time.Now().UnixNano()))
	f.Member(t, workspace, member, "member")
	memberToken := sessionToken(member)
	enlarge := map[string]any{"max_runs": 7, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 1}
	for _, tc := range []struct {
		name, method, token string
		body                any
		spoof               bool
		want                int
	}{
		{"owner agent may read", http.MethodGet, agentToken, nil, false, http.StatusOK},
		{"owner agent cannot enlarge", http.MethodPut, agentToken, enlarge, false, http.StatusForbidden},
		{"owner agent cannot forge human source", http.MethodPut, agentToken, enlarge, true, http.StatusForbidden},
		{"ordinary member may read", http.MethodGet, memberToken, nil, false, http.StatusOK},
		{"ordinary member cannot enlarge", http.MethodPut, memberToken, enlarge, false, http.StatusForbidden},
		{"other workspace agent cannot read by URL alone", http.MethodGet, otherAgentToken, nil, false, http.StatusForbidden},
		{"other workspace agent cannot enlarge by URL alone", http.MethodPut, otherAgentToken, enlarge, false, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request(tc.method, workspace, tc.token, tc.body)
			if tc.spoof {
				r.Header.Set("X-Actor-Source", "member")
				r.Header.Set("X-Agent-ID", "")
				r.Header.Set("X-Task-ID", "")
			}
			testutil.Call(t, router.ServeHTTP, r).Want(tc.want)
		})
	}
	r := request(http.MethodGet, workspace, otherAgentToken, nil)
	r.Header.Set("X-Workspace-ID", workspace)
	testutil.Call(t, router.ServeHTTP, r).Want(http.StatusForbidden)
	budget, err := testHandler.Queries.GetWorkspaceRunBudget(ctx, parseUUID(workspace))
	if err != nil || budget.MaxRuns != 6 || budget.Revision != 1 || budget.AdmittedRuns != 1 {
		t.Fatalf("rejected credentials changed the allowance: max=%d revision=%d admitted=%d err=%v", budget.MaxRuns, budget.Revision, budget.AdmittedRuns, err)
	}
}
