package tasksource

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestTaskSourcePreservesIdentityAcrossDerivedContexts(t *testing.T) {
	t.Parallel()
	trusted := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	fallback := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	base := WithTaskID(context.Background(), trusted)
	cancelled, cancel := context.WithCancel(base)
	cancel()
	for _, ctx := range []context.Context{base, cancelled, context.WithoutCancel(cancelled)} {
		if got := DelegatedFrom(ctx, fallback); got != trusted {
			t.Fatalf("derived request lost authenticated source: got %v want %v", got, trusted)
		}
	}
}

func TestTaskSourceClearAndBackgroundFallback(t *testing.T) {
	t.Parallel()
	trusted := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	fallback := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	for _, ctx := range []context.Context{context.Background(), WithTaskID(WithTaskID(context.Background(), trusted), pgtype.UUID{})} {
		if TaskID(ctx).Valid {
			t.Fatal("non-task context retained authenticated source")
		}
		if got := DelegatedFrom(ctx, fallback); got != fallback {
			t.Fatalf("background lineage changed: got %v want %v", got, fallback)
		}
	}
}
