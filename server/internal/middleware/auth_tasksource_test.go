package middleware

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/tasksource"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type taskSourceAuthDB struct{ token db.TaskToken }

func (d taskSourceAuthDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, fmt.Errorf("unexpected Exec")
}
func (d taskSourceAuthDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, fmt.Errorf("unexpected Query")
}
func (d taskSourceAuthDB) QueryRow(_ context.Context, query string, args ...interface{}) pgx.Row {
	if !strings.Contains(query, "FROM task_token") || len(args) != 1 || args[0] != d.token.TokenHash {
		return taskSourceAuthRow{err: fmt.Errorf("unexpected query")}
	}
	return taskSourceAuthRow{token: d.token}
}

type taskSourceAuthRow struct {
	token db.TaskToken
	err   error
}

func (r taskSourceAuthRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	values := []interface{}{r.token.ID, r.token.TokenHash, r.token.TaskID, r.token.AgentID, r.token.WorkspaceID, r.token.UserID, r.token.ExpiresAt, r.token.CreatedAt}
	if len(dest) != len(values) {
		return fmt.Errorf("unexpected scan length %d", len(dest))
	}
	for i, v := range values {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(v))
	}
	return nil
}

func TestAuthTaskSourceUsesCredentialBindingRatherThanHeaders(t *testing.T) {
	t.Parallel()
	bound := util.MustParseUUID("11111111-1111-4111-8111-111111111111")
	forged := util.MustParseUUID("22222222-2222-4222-8222-222222222222")
	credential := "mat_offline_task_source_fixture"
	token := db.TaskToken{TaskID: bound, AgentID: util.MustParseUUID("33333333-3333-4333-8333-333333333333"), WorkspaceID: util.MustParseUUID("44444444-4444-4444-8444-444444444444"), UserID: util.MustParseUUID("55555555-5555-4555-8555-555555555555"), TokenHash: auth.HashToken(credential), ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}}
	var got pgtype.UUID
	handler := Auth(db.New(taskSourceAuthDB{token}), nil, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = tasksource.TaskID(r.Context())
		if r.Header.Get("X-Task-ID") != util.UUIDToString(bound) {
			t.Error("task header did not come from credential")
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("X-Task-ID", util.UUIDToString(forged))
	req.Header.Set("X-Actor-Source", "member")
	req = req.WithContext(tasksource.WithTaskID(req.Context(), forged))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || got != bound {
		t.Fatalf("credential source lost or spoofed: status=%d got=%v want=%v", rec.Code, got, bound)
	}
}

func TestAuthTaskSourceHumanCannotForgeOrInheritTaskIdentity(t *testing.T) {
	t.Parallel()
	forged := util.MustParseUUID("22222222-2222-4222-8222-222222222222")
	called := false
	handler := Auth(nil, nil, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if tasksource.TaskID(r.Context()).Valid || r.Header.Get("X-Task-ID") != "" {
			t.Error("human authentication inherited a task source")
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+generateToken(validClaims(), auth.JWTSecret()))
	req.Header.Set("X-Task-ID", util.UUIDToString(forged))
	req.Header.Set("X-Actor-Source", "task_token")
	req = req.WithContext(tasksource.WithTaskID(req.Context(), forged))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("human request rejected: status=%d called=%v", rec.Code, called)
	}
}
