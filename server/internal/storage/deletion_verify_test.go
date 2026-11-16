package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalDeletionVerification(t *testing.T) {
	dir := t.TempDir()
	s := &LocalStorage{uploadDir: dir}
	ctx := context.Background()
	for _, suffix := range []string{"", metaSuffix, tempSuffix} {
		t.Run("present"+suffix, func(t *testing.T) {
			p := filepath.Join(dir, "SYN-object"+suffix)
			if suffix == tempSuffix {
				p = tempPath(filepath.Join(dir, "SYN-object"))
			}
			if err := os.WriteFile(p, []byte("SYN only"), 0600); err != nil {
				t.Fatal(err)
			}
			if s.VerifyDeleted(ctx, "SYN-object") == nil {
				t.Fatal("existing object or sidecar must not verify")
			}
			if err := s.DeleteObject(ctx, "SYN-object"); err != nil {
				t.Fatal(err)
			}
			if err := s.VerifyDeleted(ctx, "SYN-object"); err != nil {
				t.Fatal(err)
			}
		})
	}
	if s.VerifyDeleted(ctx, "../outside") == nil || s.VerifyDeleted(ctx, "") == nil {
		t.Fatal("invalid key accepted")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if s.VerifyDeleted(cancelCtx, "SYN-object") == nil {
		t.Fatal("cancellation must not verify")
	}
}
