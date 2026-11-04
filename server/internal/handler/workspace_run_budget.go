package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type workspaceRunBudgetResponse struct {
	WorkspaceID   string  `json:"workspace_id"`
	Configured    bool    `json:"configured"`
	MaxRuns       *int64  `json:"max_runs"`
	AdmittedRuns  int64   `json:"admitted_runs"`
	RemainingRuns *int64  `json:"remaining_runs"`
	Deadline      *string `json:"deadline"`
	Paused        bool    `json:"paused"`
	Revision      int64   `json:"revision"`
	StopReason    string  `json:"stop_reason"`
}

func runBudgetResponse(b db.WorkspaceRunBudget) workspaceRunBudgetResponse {
	remaining := max(int64(0), b.MaxRuns-b.AdmittedRuns)
	deadline := timestampToString(b.Deadline)
	stop := ""
	switch {
	case b.Paused:
		stop = "paused"
	case !b.Deadline.Time.After(time.Now()):
		stop = "deadline"
	case remaining == 0:
		stop = "run_limit"
	}
	return workspaceRunBudgetResponse{WorkspaceID: uuidToString(b.WorkspaceID), Configured: true,
		MaxRuns: &b.MaxRuns, AdmittedRuns: b.AdmittedRuns, RemainingRuns: &remaining,
		Deadline: &deadline, Paused: b.Paused, Revision: b.Revision, StopReason: stop}
}

// GetWorkspaceRunBudget exposes the durable ceiling even when the last
// refused enqueue was part of a transaction that rolled back.
func (h *Handler) GetWorkspaceRunBudget(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceMember(w, r, uuidToString(id), "workspace not found"); !ok {
		return
	}
	b, err := h.Queries.GetWorkspaceRunBudget(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, workspaceRunBudgetResponse{WorkspaceID: uuidToString(id)})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read run budget")
		return
	}
	writeJSON(w, http.StatusOK, runBudgetResponse(b))
}

// ConfigureWorkspaceRunBudget never resets admitted_runs. Only a human
// administrator may pause or enlarge an existing allowance.
func (h *Handler) ConfigureWorkspaceRunBudget(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Actor-Source") == "task_token" || r.Header.Get("X-Agent-ID") != "" {
		writeError(w, http.StatusForbidden, "agents may read but may not configure run budgets")
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceRole(w, r, uuidToString(id), "workspace not found", "owner", "admin"); !ok {
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
		writeError(w, http.StatusInternalServerError, "failed to configure run budget")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	// Queue writers take workspace KEY SHARE before inserting. This exclusive
	// row lock closes the initial-configuration race with those writers.
	if _, err := q.LockWorkspaceForRunBudget(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	deadline := pgtype.Timestamptz{Time: in.Deadline, Valid: true}
	var b db.WorkspaceRunBudget
	if *in.ExpectedRevision == 0 {
		active, activeErr := q.WorkspaceHasUnfinishedRuns(r.Context(), id)
		if activeErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to inspect pending runs")
			return
		}
		if active {
			writeErrorCode(w, http.StatusConflict, "run_budget_active_runs", "finish existing runs before arming a budget")
			return
		}
		b, err = q.ConfigureWorkspaceRunBudget(r.Context(), db.ConfigureWorkspaceRunBudgetParams{WorkspaceID: id, MaxRuns: in.MaxRuns, Deadline: deadline, Paused: in.Paused})
	} else {
		b, err = q.UpdateWorkspaceRunBudget(r.Context(), db.UpdateWorkspaceRunBudgetParams{WorkspaceID: id, MaxRuns: in.MaxRuns, Deadline: deadline, Paused: in.Paused, ExpectedRevision: *in.ExpectedRevision})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeErrorCode(w, http.StatusConflict, "run_budget_conflict", "revision changed or limit is below admitted usage")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to configure run budget")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to configure run budget")
		return
	}
	writeJSON(w, http.StatusOK, runBudgetResponse(b))
}
