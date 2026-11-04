package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type projectRunBudgetResponse struct {
	ProjectID string `json:"project_id"`
	workspaceRunBudgetResponse
}

func projectBudgetResponse(b db.ProjectRunBudget) projectRunBudgetResponse {
	return projectRunBudgetResponse{ProjectID: uuidToString(b.ProjectID), workspaceRunBudgetResponse: runBudgetResponse(db.WorkspaceRunBudget{
		WorkspaceID: b.WorkspaceID, MaxRuns: b.MaxRuns, AdmittedRuns: b.AdmittedRuns,
		Deadline: b.Deadline, Paused: b.Paused, Revision: b.Revision,
	})}
}

func (h *Handler) GetProjectRunBudget(w http.ResponseWriter, r *http.Request) {
	project, ok := h.loadProjectForResource(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceMember(w, r, uuidToString(project.WorkspaceID), "project not found"); !ok {
		return
	}
	b, err := h.Queries.GetProjectRunBudget(r.Context(), db.GetProjectRunBudgetParams{WorkspaceID: project.WorkspaceID, ProjectID: project.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, projectRunBudgetResponse{ProjectID: uuidToString(project.ID), workspaceRunBudgetResponse: workspaceRunBudgetResponse{WorkspaceID: uuidToString(project.WorkspaceID)}})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read project run budget")
		return
	}
	writeJSON(w, http.StatusOK, projectBudgetResponse(b))
}

// ConfigureProjectRunBudget preserves charged admissions and excludes task
// actors even when their backing human account owns the project/workspace.
func (h *Handler) ConfigureProjectRunBudget(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Actor-Source") == "task_token" || r.Header.Get("X-Agent-ID") != "" {
		writeError(w, http.StatusForbidden, "agents may read but may not configure run budgets")
		return
	}
	project, ok := h.loadProjectForResource(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceRole(w, r, uuidToString(project.WorkspaceID), "project not found", "owner", "admin"); !ok {
		return
	}
	var in struct {
		MaxRuns          int64     `json:"max_runs"`
		Deadline         time.Time `json:"deadline"`
		Paused           bool      `json:"paused"`
		ExpectedRevision *int64    `json:"expected_revision"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.MaxRuns < 1 || in.MaxRuns > 1000000 || in.ExpectedRevision == nil || *in.ExpectedRevision < 0 || in.Deadline.IsZero() {
		writeError(w, http.StatusBadRequest, "max_runs, deadline and expected_revision are required; usage cannot be reset")
		return
	}
	if !in.Paused && !in.Deadline.After(time.Now()) {
		writeError(w, http.StatusBadRequest, "an unpaused budget requires a future deadline")
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to configure project run budget")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	// Native queue inserts take workspace KEY SHARE. This brief exclusive row
	// fence serializes initial arming with every project-derived enqueue.
	if _, err := q.LockWorkspaceForRunBudget(r.Context(), project.WorkspaceID); err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if _, err := q.LockProjectForRunBudget(r.Context(), db.LockProjectForRunBudgetParams{WorkspaceID: project.WorkspaceID, ProjectID: project.ID}); err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	deadline := pgtype.Timestamptz{Time: in.Deadline, Valid: true}
	var b db.ProjectRunBudget
	if *in.ExpectedRevision == 0 {
		active, activeErr := q.ProjectHasUnfinishedBudgetRuns(r.Context(), db.ProjectHasUnfinishedBudgetRunsParams{WorkspaceID: project.WorkspaceID, ProjectID: project.ID})
		if activeErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to inspect pending project runs")
			return
		}
		if active {
			writeErrorCode(w, http.StatusConflict, "run_budget_active_runs", "finish existing project runs before arming a budget")
			return
		}
		b, err = q.ConfigureProjectRunBudget(r.Context(), db.ConfigureProjectRunBudgetParams{WorkspaceID: project.WorkspaceID, ProjectID: project.ID, MaxRuns: in.MaxRuns, Deadline: deadline, Paused: in.Paused})
	} else {
		b, err = q.UpdateProjectRunBudget(r.Context(), db.UpdateProjectRunBudgetParams{WorkspaceID: project.WorkspaceID, ProjectID: project.ID, MaxRuns: in.MaxRuns, Deadline: deadline, Paused: in.Paused, ExpectedRevision: *in.ExpectedRevision})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeErrorCode(w, http.StatusConflict, "run_budget_conflict", "revision changed or limit is below admitted usage")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to configure project run budget")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to configure project run budget")
		return
	}
	writeJSON(w, http.StatusOK, projectBudgetResponse(b))
}

// A bounded agent may request ordinary immediate work, but cannot create or
// alter durable execution rules that could outlive its source admission.
func (h *Handler) refuseBudgetedAgentAutopilotWrite(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		return false
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		writeError(w, http.StatusForbidden, "task identity is required for recurring execution changes")
		return true
	}
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return true
	}
	bounded, err := h.Queries.IsTaskProjectRunBudgeted(r.Context(), db.IsTaskProjectRunBudgetedParams{WorkspaceID: workspaceID, TaskID: taskID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect source project run budget")
		return true
	}
	if bounded {
		writeError(w, http.StatusForbidden, "project-budgeted agents may not create or modify recurring execution rules")
		return true
	}
	return false
}
