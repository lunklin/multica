package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BindDeletion opens the original root before recording the private recovery
// recipe. All subsequent operations use this root handle, even if its pathname
// is replaced or renamed while a request is in flight.
func (s *LocalStorage) BindDeletion(ctx context.Context, key string) (DeletionTarget, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" || key == "." || filepath.IsAbs(key) || filepath.Clean(key) != key || key == ".." || strings.HasPrefix(key, ".."+string(filepath.Separator)) || strings.ContainsAny(key, "\\\x00") || isInternalLocalPath(key) {
		return nil, fmt.Errorf("invalid primary object key")
	}
	absolute, err := filepath.Abs(s.uploadDir)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	identity, err := localDirectoryIdentity(info)
	if err != nil {
		root.Close()
		return nil, err
	}
	h := sha256.Sum256([]byte("local-root-v1\x00" + canonical + "\x00" + identity))
	return &localDeletionTarget{root: root, key: key, namespace: hex.EncodeToString(h[:])}, nil
}

type localDeletionTarget struct {
	root           *os.Root
	key, namespace string
}

func (t *localDeletionTarget) Namespace() string { return t.namespace }
func (t *localDeletionTarget) Close() error      { return t.root.Close() }
func (t *localDeletionTarget) paths() []string {
	return []string{t.key, t.key + metaSuffix, tempPath(t.key)}
}
func (t *localDeletionTarget) Delete(ctx context.Context) error {
	for _, p := range t.paths() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := t.root.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
func (t *localDeletionTarget) VerifyDeleted(ctx context.Context) error {
	for _, p := range t.paths() {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := t.root.Lstat(p)
		if !os.IsNotExist(err) {
			return fmt.Errorf("primary object absence not verified")
		}
	}
	return nil
}
