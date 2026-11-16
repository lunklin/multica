//go:build attachmentadminintegration && (darwin || linux)

package dbintegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const optIn = "CHEE-199-DEDICATED-SYN-ONLY"

// These are initialization receipts, not database connection strings. No TCP,
// password, environment fallback, migration runner or database creation exists
// in this harness. A separately authorized initializer must supply both the
// private receipt and matching database marker before the first connection.
type target struct {
	Format     int       `json:"format"`
	Purpose    string    `json:"purpose"`
	Root       string    `json:"root"`
	Nonce      string    `json:"nonce"`
	Database   string    `json:"database"`
	Role       string    `json:"role"`
	Port       uint16    `json:"port"`
	PID        int       `json:"pid"`
	SystemID   string    `json:"system_identifier"`
	RootDevice uint64    `json:"root_device"`
	RootInode  uint64    `json:"root_inode"`
	DataDevice uint64    `json:"data_device"`
	DataInode  uint64    `json:"data_inode"`
	SchemaSHA  string    `json:"schema_sha256"`
	Expires    time.Time `json:"expires_at"`
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var synName = regexp.MustCompile(`^chee199_syn_[0-9a-f]{32}$`)
var systemID = regexp.MustCompile(`^[0-9]{16,20}$`)

func privateJSON(name string, value any) error {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("receipt_unavailable")
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil || !i.Mode().IsRegular() || i.Mode().Perm() != 0600 || i.Size() > 16384 {
		return errors.New("receipt_not_private_regular_file")
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != uint32(os.Getuid()) {
		return errors.New("receipt_owner_mismatch")
	}
	d := json.NewDecoder(io.LimitReader(f, 16385))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("receipt_invalid_json")
	}
	return nil
}

func directoryIdentity(name string) (uint64, uint64, error) {
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil || resolved != name || !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return 0, 0, errors.New("target_path_not_canonical")
	}
	i, err := os.Lstat(name)
	if err != nil || !i.IsDir() || i.Mode().Perm() != 0700 {
		return 0, 0, errors.New("target_directory_not_private")
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != uint32(os.Getuid()) {
		return 0, 0, errors.New("target_owner_mismatch")
	}
	return uint64(s.Dev), uint64(s.Ino), nil
}

func loadTarget(name, consent string, now time.Time) (target, error) {
	var t target
	if consent != optIn || name == "" {
		return t, errors.New("dedicated_syn_target_required_NOT_RUN")
	}
	if err := privateJSON(name, &t); err != nil {
		return t, err
	}
	if t.Format != 1 || t.Purpose != optIn || !hex64.MatchString(t.Nonce) || !synName.MatchString(t.Database) || t.Role != t.Database || t.Port < 1024 || t.PID <= 1 || !systemID.MatchString(t.SystemID) || t.SchemaSHA != schemaSHA || !now.Before(t.Expires) || t.Expires.Sub(now) > 2*time.Hour {
		return t, errors.New("target_receipt_mismatch")
	}
	if name != filepath.Join(t.Root, "INIT-RECEIPT.json") {
		return t, errors.New("receipt_outside_target")
	}
	if err := t.checkFiles(); err != nil {
		return t, err
	}
	return t, nil
}

func (t target) checkFiles() error {
	d, i, err := directoryIdentity(t.Root)
	if err != nil || d != t.RootDevice || i != t.RootInode {
		return errors.New("target_root_drift")
	}
	d, i, err = directoryIdentity(filepath.Join(t.Root, "pgdata"))
	if err != nil || d != t.DataDevice || i != t.DataInode {
		return errors.New("target_data_drift")
	}
	if _, _, err = directoryIdentity(filepath.Join(t.Root, "socket")); err != nil {
		return err
	}
	// Bind the local socket and its lock to the initializer's still-running
	// postmaster. Files alone are not OS attestation; SQL rechecks follow.
	socket := filepath.Join(t.Root, "socket", fmt.Sprintf(".s.PGSQL.%d", t.Port))
	si, err := os.Lstat(socket)
	if err != nil || si.Mode()&os.ModeSocket == 0 {
		return errors.New("target_socket_required")
	}
	for _, name := range []string{filepath.Join(t.Root, "pgdata", "postmaster.pid"), socket + ".lock"} {
		f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return errors.New("postmaster_receipt_missing")
		}
		b, readErr := io.ReadAll(io.LimitReader(f, 4096))
		f.Close()
		lines := strings.Split(string(b), "\n")
		if readErr != nil || len(lines) < 2 || lines[0] != strconv.Itoa(t.PID) || lines[1] != filepath.Join(t.Root, "pgdata") {
			return errors.New("postmaster_identity_mismatch")
		}
	}
	return nil
}

func (t target) connect(ctx context.Context) (*pgxpool.Pool, error) {
	if err := t.checkFiles(); err != nil {
		return nil, err
	}
	// Explicit values override libpq defaults. TestMain rejects PG* and other
	// ambient service credentials before calling this function.
	cfg, err := pgxpool.ParseConfig("host=/invalid user=invalid dbname=invalid sslmode=disable connect_timeout=3 passfile=/dev/null")
	if err != nil {
		return nil, errors.New("target_config_invalid")
	}
	cfg.ConnConfig.Host = filepath.Join(t.Root, "socket")
	cfg.ConnConfig.Port = t.Port
	cfg.ConnConfig.Database = t.Database
	cfg.ConnConfig.User = t.Role
	cfg.ConnConfig.Password = ""
	cfg.ConnConfig.Fallbacks = nil
	cfg.ConnConfig.RuntimeParams = map[string]string{"search_path": "public", "timezone": "UTC", "application_name": "chee199-syn-dbintegration", "statement_timeout": "10000", "lock_timeout": "5000", "idle_in_transaction_session_timeout": "10000"}
	cfg.MaxConns = 4
	cfg.MinConns = 0
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error { return t.verifyConnection(ctx, c) }
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("target_connection_NOT_RUN")
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, errors.New("target_verification_NOT_RUN")
	}
	return p, nil
}

func (t target) verifyConnection(ctx context.Context, c *pgx.Conn) error {
	if !time.Now().Before(t.Expires) {
		return errors.New("target_receipt_expired")
	}
	if err := t.checkFiles(); err != nil {
		return err
	}
	var database, role, sid, data, nonce, schema string
	var major int
	// The first server statements are read-only. No fixture is constructed until
	// all identity checks pass; no attempt is made to discover another target.
	err := c.QueryRow(ctx, `SELECT current_database(), current_user, system_identifier::text, current_setting('data_directory'), current_setting('server_version_num')::int/10000 FROM pg_control_system()`).Scan(&database, &role, &sid, &data, &major)
	if err != nil || database != t.Database || role != t.Role || sid != t.SystemID || data != filepath.Join(t.Root, "pgdata") || major != 17 {
		return errors.New("database_identity_mismatch")
	}
	err = c.QueryRow(ctx, `SELECT min(nonce),min(schema_sha256) FROM chee199_syn_target WHERE singleton=true AND purpose=$1 HAVING count(*)=1`, optIn).Scan(&nonce, &schema)
	if err != nil || nonce != t.Nonce || schema != schemaSHA {
		return errors.New("database_marker_mismatch")
	}
	var versions []string
	if err = c.QueryRow(ctx, `SELECT array_agg(version ORDER BY version) FROM schema_migrations`).Scan(&versions); err != nil || !slices.Equal(versions, expectedVersions()) {
		return errors.New("schema_ledger_mismatch")
	}
	var indexReady bool
	if err = c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_index WHERE indexrelid=to_regclass('attachment_admin_record_unique') AND indisvalid AND indisunique)`).Scan(&indexReady); err != nil || !indexReady {
		return errors.New("candidate_index_unavailable")
	}
	return nil
}
