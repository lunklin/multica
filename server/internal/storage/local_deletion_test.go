package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBoundLocalDeletionSurvivesPathReplacement(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	dir := filepath.Join(parent, "active")
	old := filepath.Join(parent, "retired")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := &LocalStorage{uploadDir: dir}
	key := "SYN-object"
	if _, err := s.Upload(ctx, key, []byte("original"), "text/plain", ""); err != nil {
		t.Fatal(err)
	}
	bound, err := s.BindDeletion(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	originalNamespace := bound.Namespace()
	if err = os.Rename(dir, old); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, key), []byte("replacement must survive"), 0600); err != nil {
		t.Fatal(err)
	}
	other, err := s.BindDeletion(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if other.Namespace() == originalNamespace {
		t.Fatal("replaced directory reused namespace")
	}
	other.Close()
	if err = bound.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err = bound.VerifyDeleted(ctx); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, key)); err != nil || string(b) != "replacement must survive" {
		t.Fatal("wrong namespace was changed", err)
	}
	if _, err = os.Stat(filepath.Join(old, key)); !os.IsNotExist(err) {
		t.Fatal("original rooted object not removed", err)
	}
	if err = bound.Close(); err != nil {
		t.Fatal(err)
	}
	if bound.VerifyDeleted(ctx) == nil {
		t.Fatal("closed handle still usable")
	}
}

func TestBoundLocalDeletionStableNamespaceAndSidecars(t *testing.T) {
	s := &LocalStorage{uploadDir: t.TempDir()}
	ctx := context.Background()
	key := "SYN-object"
	first, err := s.BindDeletion(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	ns := first.Namespace()
	first.Close()
	for _, name := range []string{key, key + metaSuffix, tempPath(key)} {
		if err = os.WriteFile(filepath.Join(s.uploadDir, name), []byte("SYN"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	second, err := s.BindDeletion(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Namespace() != ns {
		t.Fatal("ordinary file changes changed root namespace")
	}
	if second.VerifyDeleted(ctx) == nil {
		t.Fatal("existing object verified")
	}
	if err = second.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err = second.VerifyDeleted(ctx); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", ".", "../outside", "/absolute", ".internal.tmp", "x.meta.json"} {
		if target, err := s.BindDeletion(ctx, key); err == nil {
			target.Close()
			t.Fatalf("accepted key %q", key)
		}
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if target, err := s.BindDeletion(cancelCtx, "SYN"); err == nil {
		target.Close()
		t.Fatal("cancelled bind succeeded")
	}
}
