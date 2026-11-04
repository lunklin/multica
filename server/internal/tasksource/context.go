// Package tasksource carries the authenticated task responsible for new work.
package tasksource

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

type taskIDKey struct{}

// WithTaskID is used at the authentication boundary after resolving a task
// credential. An invalid ID clears inherited identity for non-task auth paths.
func WithTaskID(ctx context.Context, id pgtype.UUID) context.Context {
	return context.WithValue(ctx, taskIDKey{}, id)
}

func TaskID(ctx context.Context) pgtype.UUID {
	id, _ := ctx.Value(taskIDKey{}).(pgtype.UUID)
	return id
}

// DelegatedFrom prefers the authenticated immediate caller without changing
// human attribution. Background work retains its existing recorded lineage.
func DelegatedFrom(ctx context.Context, fallback pgtype.UUID) pgtype.UUID {
	if id := TaskID(ctx); id.Valid {
		return id
	}
	return fallback
}
