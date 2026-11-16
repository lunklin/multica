package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/attachmentadmin"
	"github.com/multica-ai/multica/server/internal/storage"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// AttachmentAdmin is mounted inside the existing authenticated workspace group.
// The middleware's authoritative machine marker is checked again here; a task
// originator's user ID never grants this path human authority.
func (h *Handler) AttachmentAdmin(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) || r.Header.Get("X-Agent-ID") != "" || r.Header.Get("X-Task-ID") != "" || r.Header.Get("X-Actor-Source") != "" {
		writeJSON(w, 403, map[string]string{"code": "human_owner_required"})
		return
	}
	uid, ok := requireUserID(w, r)
	if !ok {
		return
	}
	ws := h.resolveWorkspaceID(r)
	if !attachmentadmin.ValidID(uid) || !attachmentadmin.ValidID(ws) {
		writeJSON(w, 400, map[string]string{"code": "invalid_workspace"})
		return
	}
	if h.cfg.ServerVersion == "" || !attachmentadmin.ValidCommit(h.cfg.ServerCommit) {
		writeJSON(w, 503, map[string]string{"code": "server_build_unknown"})
		return
	}
	r = h.withWakeupActor(r)
	var objects attachmentadmin.ObjectStore
	if h.Storage != nil {
		objects = adminObjects{h.Storage}
	}
	svc := &attachmentadmin.Service{Store: adminStore{h}, Objects: objects, Version: h.cfg.ServerVersion + "@" + h.cfg.ServerCommit + "/" + attachmentadmin.Capability, Commit: h.cfg.ServerCommit}
	attachmentadmin.HTTP{Service: svc, Actor: func(*http.Request) (attachmentadmin.Actor, error) {
		return attachmentadmin.Actor{ID: uid, Workspace: ws, Human: true}, nil
	}}.ServeHTTP(w, r)
}

type adminObjects struct{ storage.Storage }

func (o adminObjects) Bind(ctx context.Context, rawURL, expectedNamespace string) (attachmentadmin.ObjectTarget, error) {
	key := o.KeyFromURL(rawURL)
	if key == "" || key == "." || path.Clean(key) != key || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\\\x00?#") || key == ".." || strings.HasPrefix(key, "../") || o.ObjectURL(key) != rawURL {
		return nil, errors.New("unrecognized object location")
	}
	binder, ok := o.Storage.(storage.DeletionBinder)
	if !ok {
		return nil, errors.New("storage namespace binding unavailable")
	}
	target, err := binder.BindDeletion(ctx, key)
	if err != nil {
		return nil, err
	}
	if target.Namespace() == "" || expectedNamespace != "" && target.Namespace() != expectedNamespace {
		_ = target.Close()
		return nil, errors.New("storage namespace changed")
	}
	return target, nil
}

type adminStore struct{ h *Handler }

func (s adminStore) Transaction(ctx context.Context, ws string, fn func(attachmentadmin.Tx) error) error {
	tx, err := s.h.beginWakeupWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	// Scope serialization protects durable request IDs and plan creation. It does
	// not replace the existing issue -> comment -> attachment locking order.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "attachment-admin:"+ws); err != nil {
		return err
	}
	if err = fn(&adminTx{tx: tx, q: s.h.Queries.WithTx(tx), ws: ws}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type adminTx struct {
	tx pgx.Tx
	q  *db.Queries
	ws string
}

func (t *adminTx) Owner(ctx context.Context, a attachmentadmin.Actor) error {
	var role string
	err := t.tx.QueryRow(ctx, `SELECT role FROM member WHERE user_id=$1 AND workspace_id=$2 FOR SHARE`, a.ID, t.ws).Scan(&role)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err != nil || role != "owner" {
		return &attachmentadmin.Error{Code: "human_owner_required", Status: 403}
	}
	// Missing/incomplete candidate migrations fail closed before any plan or
	// mutation. A table without the unique request-key index is insufficient.
	var ready bool
	err = t.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_index WHERE indexrelid=to_regclass('attachment_admin_record_unique') AND indisvalid AND indisunique)`).Scan(&ready)
	if err != nil {
		return err
	}
	if !ready {
		return &attachmentadmin.Error{Code: "attachment_admin_schema_unavailable", Status: 503}
	}
	return nil
}
func (t *adminTx) Attachment(ctx context.Context, id string) (attachmentadmin.Metadata, error) {
	empty := attachmentadmin.Metadata{}
	att, err := t.q.GetAttachment(ctx, db.GetAttachmentParams{ID: parseUUID(id), WorkspaceID: parseUUID(t.ws)})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, attachmentadmin.ErrMissing
	}
	if err != nil {
		return empty, err
	}
	if att.IssueID.Valid {
		if _, err = t.q.LockIssueForAttachmentWrite(ctx, db.LockIssueForAttachmentWriteParams{ID: att.IssueID, WorkspaceID: att.WorkspaceID}); err != nil {
			return empty, err
		}
	}
	if att.CommentID.Valid {
		if _, err = t.q.LockLiveComment(ctx, db.LockLiveCommentParams{ID: att.CommentID, WorkspaceID: att.WorkspaceID}); err != nil {
			return empty, err
		}
	}
	fresh, err := t.q.LockAttachmentRow(ctx, db.LockAttachmentRowParams{ID: att.ID, WorkspaceID: att.WorkspaceID})
	if err != nil {
		return empty, err
	}
	if fresh.IssueID != att.IssueID || fresh.CommentID != att.CommentID {
		return empty, &attachmentadmin.Error{Code: "metadata_drift", Status: 409}
	}
	// This endpoint intentionally supports issue-owned attachments only; it does
	// not grant access to private chat or abandoned task uploads.
	if !fresh.SourceContextID.Valid && (!fresh.IssueID.Valid || fresh.ChatSessionID.Valid || fresh.ChatMessageID.Valid) {
		return empty, &attachmentadmin.Error{Code: "issue_attachment_required", Status: 422}
	}
	return attachmentadmin.Metadata{ID: id, Workspace: t.ws, Issue: uuidToString(fresh.IssueID), Comment: uuidToString(fresh.CommentID), Filename: fresh.Filename, Size: fresh.SizeBytes, UploaderID: uuidToString(fresh.UploaderID), UploaderType: fresh.UploaderType, Captured: fresh.SourceContextID.Valid, URL: fresh.Url}, nil
}
func (t *adminTx) read(ctx context.Context, kind, id string, out any) error {
	var data []byte
	err := t.tx.QueryRow(ctx, `SELECT data FROM attachment_admin_record WHERE kind=$1 AND id=$2 AND workspace_id=$3 FOR UPDATE`, kind, id, t.ws).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return attachmentadmin.ErrMissing
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
func (t *adminTx) put(ctx context.Context, kind, id, actor string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	// A global unique key prevents cross-workspace request-ID reuse. The guarded
	// upsert cannot overwrite another actor/workspace's receipt.
	tag, err := t.tx.Exec(ctx, `INSERT INTO attachment_admin_record(kind,id,workspace_id,actor_id,data) VALUES($1,$2,$3,$4,$5) ON CONFLICT(kind,id) DO UPDATE SET data=EXCLUDED.data WHERE attachment_admin_record.workspace_id=EXCLUDED.workspace_id AND attachment_admin_record.actor_id=EXCLUDED.actor_id`, kind, id, t.ws, actor, data)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &attachmentadmin.Error{Code: "request_id_conflict", Status: 409}
	}
	return nil
}
func (t *adminTx) Plan(ctx context.Context, id string) (attachmentadmin.Plan, error) {
	var stored struct {
		Plan      attachmentadmin.Plan `json:"plan"`
		Namespace string               `json:"storage_namespace"`
	}
	err := t.read(ctx, "plan", id, &stored)
	stored.Plan.StorageNamespace = stored.Namespace
	return stored.Plan, err
}
func (t *adminTx) PutPlan(ctx context.Context, p attachmentadmin.Plan) error {
	stored := struct {
		Plan      attachmentadmin.Plan `json:"plan"`
		Namespace string               `json:"storage_namespace"`
	}{p, p.StorageNamespace}
	return t.put(ctx, "plan", p.ID, p.Actor, stored)
}
func (t *adminTx) Operation(ctx context.Context, id string) (attachmentadmin.StoredOperation, error) {
	var op attachmentadmin.StoredOperation
	err := t.read(ctx, "operation", id, &op)
	return op, err
}
func (t *adminTx) PutOperation(ctx context.Context, op attachmentadmin.StoredOperation) error {
	return t.put(ctx, "operation", op.Receipt.ID, op.Receipt.Actor, op)
}
func (t *adminTx) DeleteAttachment(ctx context.Context, m attachmentadmin.Metadata) error {
	row, err := t.q.DeleteAttachment(ctx, db.DeleteAttachmentParams{ID: parseUUID(m.ID), WorkspaceID: parseUUID(t.ws)})
	if err != nil {
		return err
	}
	if !row.Changed {
		return &attachmentadmin.Error{Code: "metadata_drift", Status: 409}
	}
	return nil
}
