package attachmentadmin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

type HTTP struct {
	Service *Service
	Actor   func(*http.Request) (Actor, error)
}
type Version struct {
	Build      string `json:"server_version"`
	Capability string `json:"capability"`
	Commit     string `json:"server_commit"`
}

const Prefix = "/api/attachment-admin/"

func (h HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Never persist arbitrary client headers in audit records. This is a reported
	// version, not an attestation of the executable or authenticated identity.
	service := *h.Service
	service.ClientVersion = "UNKNOWN"
	if r.Header.Get("X-Client-Version") == CLIVersion {
		service.ClientVersion = CLIVersion
	}
	h.Service = &service
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	a, err := h.Actor(r)
	if err != nil {
		respond(w, nil, err)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, Prefix), "/")
	if !strings.HasPrefix(r.URL.Path, Prefix) {
		respond(w, nil, fail("not_found", 404))
		return
	}
	if len(parts) == 1 && parts[0] == "version" && r.Method == "GET" {
		err = h.Service.transaction(r.Context(), a, func(Tx) error { return nil })
		respond(w, Version{h.Service.Version, Capability, h.Service.Commit}, err)
		return
	}
	if len(parts) == 2 && parts[0] == "operations" && r.Method == "GET" {
		op, err := h.Service.Get(r.Context(), a, parts[1])
		respond(w, op, err)
		return
	}
	if len(parts) < 2 || parts[0] != "attachments" {
		respond(w, nil, fail("not_found", 404))
		return
	}
	id := parts[1]
	if len(parts) == 2 && r.Method == "GET" {
		m, err := h.Service.Inspect(r.Context(), a, id)
		if err == nil {
			reason := ""
			if m.Captured {
				reason = "captured_context_immutable"
			} else if _, err := h.Service.storageNamespace(r.Context(), m.URL); err != nil {
				reason = "storage_namespace_unavailable"
			}
			respond(w, map[string]any{"metadata": m, "can_delete": reason == "", "reason": reason}, nil)
		} else {
			respond(w, nil, err)
		}
		return
	}
	if len(parts) != 3 || r.Method != "POST" {
		respond(w, nil, fail("method_not_allowed", 405))
		return
	}
	switch parts[2] {
	case "plans":
		var scope Scope
		if err := decode(r, &scope); err != nil {
			respond(w, nil, err)
			return
		}
		p, err := h.Service.Preview(r.Context(), a, id, scope)
		respond(w, p, err)
	case "confirm":
		var input struct {
			PlanID    string `json:"plan_id"`
			RequestID string `json:"request_id"`
		}
		if err := decode(r, &input); err != nil {
			respond(w, nil, err)
			return
		}
		op, err := h.Service.Confirm(r.Context(), a, id, input.PlanID, input.RequestID)
		respond(w, op, err)
	default:
		respond(w, nil, fail("not_found", 404))
	}
}
func decode(r *http.Request, out any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 65537))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return fail("invalid_input", 400)
	}
	if d.Decode(new(any)) != io.EOF {
		return fail("invalid_input", 400)
	}
	return nil
}
func respond(w http.ResponseWriter, value any, err error) {
	if err != nil {
		e := &Error{Code: "operation_result_unknown_read_request_id", Status: 500}
		var known *Error
		if errors.As(err, &known) {
			e = known
		} else if errors.Is(err, ErrMissing) {
			e = &Error{Code: "not_found_not_a_removal_receipt", Status: 404}
		}
		w.WriteHeader(e.Status)
		_ = json.NewEncoder(w).Encode(e)
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}
