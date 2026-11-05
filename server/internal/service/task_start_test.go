package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type startReplayDB struct {
	mockDBTX
	lifecycleErr    error
	startErr        error
	lifecycleCalls  int
	startCalls      int
	unexpectedCalls int
}

func (m *startReplayDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.HasPrefix(sql, "-- name: LockQoderTaskIssueForLifecycle "):
		m.lifecycleCalls++
		return &mockRow{err: m.lifecycleErr}
	case strings.HasPrefix(sql, "-- name: StartAgentTaskWithSupplement "):
		m.startCalls++
		return &mockRow{err: m.startErr}
	default:
		m.unexpectedCalls++
		return &mockRow{err: errors.New("unexpected legacy start query")}
	}
}

func TestStartTaskLegacyDoesNotAcknowledgeReplayOrDatabaseFailure(t *testing.T) {
	databaseErr := errors.New("database unavailable")
	for _, tc := range []struct {
		name         string
		lifecycleErr error
		startErr     error
		wantErr      error
		wantStarts   int
	}{
		{"replay", pgx.ErrNoRows, pgx.ErrNoRows, pgx.ErrNoRows, 1},
		{"start database failure", pgx.ErrNoRows, databaseErr, databaseErr, 1},
		{"lifecycle database failure", databaseErr, databaseErr, databaseErr, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &startReplayDB{lifecycleErr: tc.lifecycleErr, startErr: tc.startErr}
			svc := &TaskService{Queries: db.New(store)}
			got, err := svc.StartTask(context.Background(), testUUID(1))
			if got != nil || !errors.Is(err, tc.wantErr) || store.lifecycleCalls != 1 || store.startCalls != tc.wantStarts || store.unexpectedCalls != 0 {
				t.Fatalf("legacy start: task=%v err=%v lifecycle_reads=%d start_mutations=%d unexpected=%d", got, err, store.lifecycleCalls, store.startCalls, store.unexpectedCalls)
			}
		})
	}
}
