// Package attachmentadmin implements bounded, human-owner attachment removal.
// It never handles attachment payloads, credentials, or download capabilities.
package attachmentadmin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"
)

const Capability = "attachment-admin-v1"
const Pending = "METADATA_REMOVED_OBJECT_PENDING"
const Removed = "REMOVED_VERIFIED"
const Rejected = "REJECTED_NO_CHANGE"

type Error struct {
	Code   string `json:"code"`
	Status int    `json:"-"`
}

func (e *Error) Error() string           { return e.Code }
func fail(code string, status int) error { return &Error{code, status} }

type Actor struct {
	ID, Workspace string
	Human         bool
}
type Metadata struct {
	ID           string `json:"id"`
	Workspace    string `json:"workspace_id"`
	Issue        string `json:"issue_id"`
	Comment      string `json:"comment_id"`
	Filename     string `json:"filename"`
	Size         int64  `json:"size_bytes"`
	UploaderType string `json:"uploader_type"`
	UploaderID   string `json:"uploader_id"`
	Captured     bool   `json:"captured_context"`
	URL          string `json:"-"`
}
type Scope struct {
	Workspace string   `json:"workspace_id"`
	Issue     string   `json:"issue_id"`
	IDs       []string `json:"attachment_ids"`
}
type Plan struct {
	ID               string    `json:"id"`
	Actor            string    `json:"actor_id"`
	Version          string    `json:"server_version"`
	Scope            Scope     `json:"scope"`
	Target           Metadata  `json:"target"`
	Fingerprint      string    `json:"metadata_fingerprint"`
	Expires          time.Time `json:"expires_at"`
	StorageNamespace string    `json:"-"`
}
type Operation struct {
	ID                string     `json:"request_id"`
	PlanID            string     `json:"plan_id"`
	Actor             string     `json:"actor_id"`
	Version           string     `json:"server_version"`
	ClientVersion     string     `json:"cli_version_reported"`
	Target            Metadata   `json:"target"`
	Started           time.Time  `json:"started_at"`
	Finished          *time.Time `json:"finished_at,omitempty"`
	State             string     `json:"state"`
	MetadataRemoved   bool       `json:"metadata_removed"`
	PlatformRetrieval string     `json:"platform_retrieval"`
	PrimaryStorage    string     `json:"primary_storage"`
	RemainingCopies   string     `json:"remaining_copies"`
	Reason            string     `json:"reason,omitempty"`
}

// StoredOperation keeps the object location private and durable before the row
// is removed. Only Operation is ever returned to callers.
type StoredOperation struct {
	Receipt          Operation `json:"receipt"`
	ObjectURL        string    `json:"object_url"`
	StorageNamespace string    `json:"storage_namespace"`
}

// Transaction must atomically commit the callback or roll it back. It must
// serialize operations in the workspace and lock attachment owners before rows.
type Store interface {
	Transaction(context.Context, string, func(Tx) error) error
}
type Tx interface {
	Owner(context.Context, Actor) error
	Attachment(context.Context, string) (Metadata, error)
	Plan(context.Context, string) (Plan, error)
	PutPlan(context.Context, Plan) error
	Operation(context.Context, string) (StoredOperation, error)
	PutOperation(context.Context, StoredOperation) error
	DeleteAttachment(context.Context, Metadata) error
}
type ObjectStore interface {
	Bind(context.Context, string, string) (ObjectTarget, error)
}

// A target pins the storage namespace for the entire deletion and verification.
// Close releases only this operation's resources, never a shared storage client.
type ObjectTarget interface {
	Namespace() string
	Delete(context.Context) error
	VerifyDeleted(context.Context) error
	Close() error
}
type Service struct {
	Store         Store
	Objects       ObjectStore
	Version       string
	Commit        string
	ClientVersion string
	Now           func() time.Time
}

var ErrMissing = errors.New("missing")
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func ValidCommit(value string) bool { return commitPattern.MatchString(value) }

func ValidID(id string) bool { return uuidPattern.MatchString(id) }
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}
func fingerprint(m Metadata) string {
	b, _ := json.Marshal(struct {
		Metadata
		URL string
	}{m, m.URL})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *Service) transaction(ctx context.Context, a Actor, fn func(Tx) error) error {
	if !a.Human || !ValidID(a.ID) || !ValidID(a.Workspace) {
		return fail("human_owner_required", 403)
	}
	return s.Store.Transaction(ctx, a.Workspace, func(tx Tx) error {
		if err := tx.Owner(ctx, a); err != nil {
			return err
		}
		return fn(tx)
	})
}
func (s *Service) Inspect(ctx context.Context, a Actor, id string) (Metadata, error) {
	var m Metadata
	if !ValidID(id) {
		return m, fail("invalid_id", 400)
	}
	err := s.transaction(ctx, a, func(tx Tx) error { var err error; m, err = tx.Attachment(ctx, id); return err })
	return m, err
}
func (s *Service) storageNamespace(ctx context.Context, url string) (string, error) {
	if s.Objects == nil || url == "" {
		return "", fail("storage_namespace_unavailable", 503)
	}
	target, err := s.Objects.Bind(ctx, url, "")
	if target != nil {
		defer target.Close()
	}
	if err != nil || target == nil || target.Namespace() == "" {
		return "", fail("storage_namespace_unavailable", 503)
	}
	return target.Namespace(), nil
}
func (s *Service) Preview(ctx context.Context, a Actor, id string, scope Scope) (Plan, error) {
	var p Plan
	if !ValidID(id) || !ValidID(scope.Issue) || scope.Workspace != a.Workspace || len(scope.IDs) != 16 {
		return p, fail("invalid_scope", 400)
	}
	seen := map[string]bool{}
	for _, v := range scope.IDs {
		if !ValidID(v) || seen[v] {
			return p, fail("invalid_allowlist", 400)
		}
		seen[v] = true
	}
	if !seen[id] {
		return p, fail("outside_allowlist", 400)
	}
	scope.IDs = slices.Clone(scope.IDs)
	slices.Sort(scope.IDs)
	err := s.transaction(ctx, a, func(tx Tx) error {
		m, err := tx.Attachment(ctx, id)
		if err != nil {
			return err
		}
		if m.Captured {
			return fail("captured_context_immutable", 422)
		}
		if m.Workspace != scope.Workspace || m.Issue != scope.Issue {
			return fail("scope_mismatch", 409)
		}
		namespace, err := s.storageNamespace(ctx, m.URL)
		if err != nil {
			return err
		}
		p = Plan{ID: newID(), Actor: a.ID, Version: s.Version, Scope: scope, Target: m, Fingerprint: fingerprint(m), Expires: s.now().Add(10 * time.Minute), StorageNamespace: namespace}
		return tx.PutPlan(ctx, p)
	})
	return p, err
}
func (s *Service) Get(ctx context.Context, a Actor, id string) (Operation, error) {
	var op StoredOperation
	if !ValidID(id) {
		return op.Receipt, fail("invalid_request_id", 400)
	}
	err := s.transaction(ctx, a, func(tx Tx) error {
		var err error
		op, err = tx.Operation(ctx, id)
		if err != nil {
			return err
		}
		if op.Receipt.Actor != a.ID || op.Receipt.Target.Workspace != a.Workspace {
			return fail("operation_scope_denied", 403)
		}
		return nil
	})
	return op.Receipt, err
}
func (s *Service) Confirm(ctx context.Context, a Actor, target, planID, requestID string) (Operation, error) {
	var op StoredOperation
	var object ObjectTarget
	defer func() {
		if object != nil {
			_ = object.Close()
		}
	}()
	if !ValidID(target) || !ValidID(planID) || !ValidID(requestID) {
		return op.Receipt, fail("invalid_confirmation", 400)
	}
	err := s.transaction(ctx, a, func(tx Tx) error {
		previous, err := tx.Operation(ctx, requestID)
		if err == nil {
			if previous.Receipt.Actor != a.ID || previous.Receipt.PlanID != planID || previous.Receipt.Target.ID != target || previous.Receipt.Target.Workspace != a.Workspace {
				return fail("request_id_conflict", 409)
			}
			op = previous
			return nil
		}
		if !errors.Is(err, ErrMissing) {
			return err
		}
		p, err := tx.Plan(ctx, planID)
		if err != nil {
			return err
		}
		if p.Actor != a.ID || p.Scope.Workspace != a.Workspace || p.Target.ID != target || !slices.Contains(p.Scope.IDs, target) {
			return fail("plan_scope_denied", 403)
		}
		reject := func(reason string) error {
			now := s.now()
			op = StoredOperation{Receipt: Operation{ID: requestID, PlanID: planID, Actor: a.ID, Version: s.Version, ClientVersion: s.ClientVersion, Target: p.Target, Started: now, Finished: &now, State: Rejected, PlatformRetrieval: "UNCHANGED_BY_OPERATION", PrimaryStorage: "NOT_REQUESTED", RemainingCopies: "NOT_VERIFIED", Reason: reason}}
			return tx.PutOperation(ctx, op)
		}
		if p.Version != s.Version || !s.now().Before(p.Expires) {
			return reject("plan_expired_or_version_changed")
		}
		m, err := tx.Attachment(ctx, target)
		if err != nil {
			return err
		}
		if m.Captured {
			return reject("captured_context_immutable")
		}
		if fingerprint(m) != p.Fingerprint || m.Issue != p.Scope.Issue {
			return reject("metadata_drift")
		}
		if s.Objects == nil || m.URL == "" {
			return fail("storage_unavailable", 503)
		}
		if p.StorageNamespace == "" {
			return reject("plan_storage_namespace_unbound")
		}
		object, err = s.Objects.Bind(ctx, m.URL, p.StorageNamespace)
		if err != nil || object == nil || object.Namespace() == "" {
			return reject("storage_namespace_unavailable_or_changed")
		}
		if object.Namespace() != p.StorageNamespace {
			return reject("storage_namespace_unavailable_or_changed")
		}
		op = StoredOperation{Receipt: Operation{ID: requestID, PlanID: planID, Actor: a.ID, Version: s.Version, ClientVersion: s.ClientVersion, Target: m, Started: s.now(), State: Pending, MetadataRemoved: true, PlatformRetrieval: "ROW_REMOVED", PrimaryStorage: "PENDING", RemainingCopies: "CDN_URL_VERSIONS_BACKUPS_NOT_VERIFIED"}, ObjectURL: m.URL, StorageNamespace: object.Namespace()}
		if err := tx.PutOperation(ctx, op); err != nil {
			return err
		}
		return tx.DeleteAttachment(ctx, m)
	})
	if err != nil {
		return Operation{}, err
	}
	if op.Receipt.State == Removed || op.Receipt.State == Rejected {
		return op.Receipt, nil
	}
	if s.Objects == nil {
		return op.Receipt, fail("storage_unavailable", 503)
	}
	// A request cancellation or ambiguous commit must never trigger a second
	// metadata removal. The client first reads the same durable request ID.
	if ctx.Err() != nil {
		return op.Receipt, ctx.Err()
	}
	// Never infer a missing historical namespace from the current configuration.
	// A URL alone can name another empty directory after a service restart.
	var bindErr error
	if object == nil && op.StorageNamespace != "" {
		object, bindErr = s.Objects.Bind(ctx, op.ObjectURL, op.StorageNamespace)
	}
	if op.StorageNamespace == "" || bindErr != nil || object == nil || object.Namespace() != op.StorageNamespace {
		err = s.transaction(ctx, a, func(tx Tx) error {
			current, err := tx.Operation(ctx, requestID)
			if err != nil {
				return err
			}
			if current.Receipt.State != Removed {
				current.Receipt.Reason = "storage_namespace_unavailable_or_changed"
				if current.StorageNamespace == "" {
					current.Receipt.Reason = "storage_namespace_unbound"
				}
				if err = tx.PutOperation(ctx, current); err != nil {
					return err
				}
			}
			op = current
			return nil
		})
		return op.Receipt, err
	}
	cleanupErr := object.Delete(ctx)
	verificationFailed := false
	if cleanupErr == nil {
		cleanupErr = object.VerifyDeleted(ctx)
		verificationFailed = cleanupErr != nil
	}
	err = s.transaction(ctx, a, func(tx Tx) error {
		current, err := tx.Operation(ctx, requestID)
		if err != nil {
			return err
		}
		if current.Receipt.State == Removed {
			op = current
			return nil
		}
		if cleanupErr != nil {
			current.Receipt.Reason = "primary_storage_cleanup_failed"
			if verificationFailed {
				current.Receipt.PrimaryStorage = "DELETE_ACKNOWLEDGED_UNVERIFIED"
				current.Receipt.Reason = "primary_storage_verification_failed"
			}
		} else {
			now := s.now()
			current.Receipt.State = Removed
			current.Receipt.PrimaryStorage = "ABSENCE_VERIFIED"
			current.Receipt.Finished = &now
			current.Receipt.Reason = ""
		}
		op = current
		return tx.PutOperation(ctx, current)
	})
	if err != nil {
		return op.Receipt, err
	}
	return op.Receipt, nil
}
