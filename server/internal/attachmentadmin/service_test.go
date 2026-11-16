package attachmentadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// This disk fixture exercises the service's transactional contract and process
// boundaries. It is not PostgreSQL, S3, or a substitute for adapter integration.
type image struct {
	Attachments map[string]Metadata
	Plans       map[string]Plan
	Operations  map[string]StoredOperation
	Owner       string
	Comments    map[string]string
}
type diskImage struct {
	Image image
	URLs  map[string]string
}
type fixtureStore struct {
	sync.Mutex
	image      image
	path       string
	commitFail bool
	ambiguous  bool
	writes     int
}
type fixtureTx struct{ image *image }

func id(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
func clone(i image) image {
	j := i
	j.Attachments = map[string]Metadata{}
	j.Plans = map[string]Plan{}
	j.Operations = map[string]StoredOperation{}
	j.Comments = map[string]string{}
	for k, v := range i.Attachments {
		j.Attachments[k] = v
	}
	for k, v := range i.Plans {
		j.Plans[k] = v
	}
	for k, v := range i.Operations {
		j.Operations[k] = v
	}
	for k, v := range i.Comments {
		j.Comments[k] = v
	}
	return j
}
func (f *fixtureStore) Transaction(ctx context.Context, ws string, fn func(Tx) error) error {
	f.Lock()
	defer f.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	next := clone(f.image)
	if err := fn(&fixtureTx{&next}); err != nil {
		return err
	}
	if f.commitFail {
		f.commitFail = false
		return errors.New("SYN transaction rejected")
	}
	if f.path != "" {
		urls := map[string]string{}
		for k, v := range next.Attachments {
			urls[k] = v.URL
		}
		b, _ := json.Marshal(diskImage{next, urls})
		file, err := os.CreateTemp(filepath.Dir(f.path), "state-")
		if err != nil {
			return err
		}
		name := file.Name()
		defer os.Remove(name)
		if _, err = file.Write(b); err != nil {
			file.Close()
			return err
		}
		if err = file.Sync(); err != nil {
			file.Close()
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		if err = os.Rename(name, f.path); err != nil {
			return err
		}
	}
	f.image = next
	f.writes++
	if f.ambiguous {
		f.ambiguous = false
		return errors.New("SYN committed response lost")
	}
	return nil
}
func loadFixture(path string) *fixtureStore {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	var d diskImage
	if err = json.Unmarshal(b, &d); err != nil {
		panic(err)
	}
	for k, url := range d.URLs {
		m := d.Image.Attachments[k]
		m.URL = url
		d.Image.Attachments[k] = m
	}
	return &fixtureStore{path: path, image: d.Image}
}
func (t *fixtureTx) Owner(_ context.Context, a Actor) error {
	if a.ID != t.image.Owner || a.Workspace != id(1) {
		return fail("human_owner_required", 403)
	}
	return nil
}
func (t *fixtureTx) Attachment(_ context.Context, key string) (Metadata, error) {
	m, ok := t.image.Attachments[key]
	if !ok {
		return m, ErrMissing
	}
	return m, nil
}
func (t *fixtureTx) Plan(_ context.Context, key string) (Plan, error) {
	p, ok := t.image.Plans[key]
	if !ok {
		return p, ErrMissing
	}
	return p, nil
}
func (t *fixtureTx) PutPlan(_ context.Context, p Plan) error { t.image.Plans[p.ID] = p; return nil }
func (t *fixtureTx) Operation(_ context.Context, key string) (StoredOperation, error) {
	p, ok := t.image.Operations[key]
	if !ok {
		return p, ErrMissing
	}
	return p, nil
}
func (t *fixtureTx) PutOperation(_ context.Context, p StoredOperation) error {
	t.image.Operations[p.Receipt.ID] = p
	return nil
}
func (t *fixtureTx) DeleteAttachment(_ context.Context, m Metadata) error {
	delete(t.image.Attachments, m.ID)
	return nil
}

type fixtureObjects struct {
	sync.Mutex
	objects       map[string]bool
	fail          bool
	calls         int
	crash         bool
	verifyFail    bool
	namespace     string
	binds, closed int
	after         func()
}

func (o *fixtureObjects) Delete(_ context.Context, url string) error {
	o.Lock()
	defer o.Unlock()
	o.calls++
	if o.crash {
		os.Exit(27)
	}
	if o.fail {
		return errors.New("SYN object service failed")
	}
	delete(o.objects, url)
	if o.after != nil {
		o.after()
	}
	return nil
}
func setup(t *testing.T) (*Service, *fixtureStore, *fixtureObjects, Actor, Scope) {
	t.Helper()
	f := &fixtureStore{image: image{Attachments: map[string]Metadata{}, Plans: map[string]Plan{}, Operations: map[string]StoredOperation{}, Owner: id(3), Comments: map[string]string{id(4): "Original comment and reply preserved"}}}
	o := &fixtureObjects{objects: map[string]bool{}}
	scope := Scope{Workspace: id(1), Issue: id(2)}
	for n := 10; n < 26; n++ {
		m := Metadata{ID: id(n), Workspace: id(1), Issue: id(2), Filename: fmt.Sprintf("SYN-%d.txt", n), URL: fmt.Sprintf("syn-object-%d", n), Size: 9, UploaderType: "agent", UploaderID: id(90)}
		if n == 10 {
			m.Comment = id(4)
		}
		f.image.Attachments[m.ID] = m
		o.objects[m.URL] = true
		scope.IDs = append(scope.IDs, m.ID)
	}
	return &Service{Store: f, Objects: o, Version: "SYN-fixed-build/" + Capability, Commit: "1f515002a9940b6fb3cb9658822970ef9426c91c"}, f, o, Actor{ID: id(3), Workspace: id(1), Human: true}, scope
}
func mustPlan(t *testing.T, s *Service, a Actor, scope Scope) Plan {
	t.Helper()
	p, err := s.Preview(context.Background(), a, id(10), scope)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func unchanged(t *testing.T, f *fixtureStore, before image) {
	t.Helper()
	after := clone(f.image)
	before.Plans = after.Plans
	before.Operations = after.Operations
	if !reflect.DeepEqual(before, after) {
		t.Fatal("original comments/other attachments changed")
	}

}

func TestNormalBoundedRemoval(t *testing.T) {
	for _, target := range []string{id(10), id(11)} {
		t.Run(target, func(t *testing.T) {
			s, f, o, a, scope := setup(t)
			before := clone(f.image)
			p, err := s.Preview(context.Background(), a, target, scope)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.image.Attachments) != 16 || o.calls != 0 {
				t.Fatal("preview changed attachments")
			}
			op, err := s.Confirm(context.Background(), a, target, p.ID, id(100))
			if err != nil || op.State != Removed || !op.MetadataRemoved || op.PrimaryStorage != "ABSENCE_VERIFIED" {
				t.Fatalf("%+v %v", op, err)
			}
			delete(before.Attachments, target)
			unchanged(t, f, before)
			if len(o.objects) != 15 || o.calls != 1 {
				t.Fatal("wrong object delta")
			}
			again, err := s.Confirm(context.Background(), a, target, p.ID, id(100))
			if err != nil || again.State != Removed || o.calls != 1 {
				t.Fatal("reentry repeated cleanup")
			}
		})
	}
}
func TestPermissionAndDriftRejectWithoutMutation(t *testing.T) {
	for _, name := range []string{"non-owner", "agent", "task-with-owner", "cross-workspace", "cross-issue", "outside-list", "duplicate-list", "short-list", "captured", "metadata-drift", "expired", "version", "request-reuse"} {
		t.Run(name, func(t *testing.T) {
			s, f, o, a, scope := setup(t)
			p := mustPlan(t, s, a, scope)
			switch name {
			case "non-owner":
				a.ID = id(99)
			case "agent", "task-with-owner":
				a.Human = false
			case "cross-workspace":
				a.Workspace = id(99)
			case "cross-issue":
				m := f.image.Attachments[id(10)]
				m.Issue = id(88)
				f.image.Attachments[id(10)] = m
			case "captured":
				m := f.image.Attachments[id(10)]
				m.Captured = true
				f.image.Attachments[id(10)] = m
			case "metadata-drift":
				m := f.image.Attachments[id(10)]
				m.Filename = "drift"
				f.image.Attachments[id(10)] = m
			case "expired":
				s.Now = func() time.Time { return p.Expires }
			case "version":
				s.Version = "different"
			case "outside-list":
				scope.IDs[0] = id(99)
			case "duplicate-list":
				scope.IDs[1] = scope.IDs[0]
			case "short-list":
				scope.IDs = scope.IDs[:15]
			case "request-reuse":
				f.image.Operations[id(100)] = StoredOperation{Receipt: Operation{ID: id(100), Actor: id(999)}}
			}
			before := clone(f.image)
			var err error
			var receipt Operation
			if name == "outside-list" || name == "duplicate-list" || name == "short-list" {
				_, err = s.Preview(context.Background(), a, id(10), scope)
			} else {
				receipt, err = s.Confirm(context.Background(), a, id(10), p.ID, id(100))
			}
			if err == nil && receipt.State != Rejected {
				t.Fatal("expected refusal")
			}
			unchanged(t, f, before)
			if o.calls != 0 {
				t.Fatal("storage called on refusal")
			}
		})
	}
}
func TestTransactionFailureAndAmbiguousCommit(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint(ambiguous), func(t *testing.T) {
			s, f, o, a, scope := setup(t)
			p := mustPlan(t, s, a, scope)
			f.commitFail = !ambiguous
			f.ambiguous = ambiguous
			_, err := s.Confirm(context.Background(), a, id(10), p.ID, id(100))
			if err == nil || o.calls != 0 {
				t.Fatal("commit failure must not start storage")
			}
			op, getErr := s.Get(context.Background(), a, id(100))
			if ambiguous {
				if getErr != nil || op.State != Pending || len(f.image.Attachments) != 15 {
					t.Fatal("committed intent lost")
				}
				done, err := s.Confirm(context.Background(), a, id(10), p.ID, id(100))
				if err != nil || done.State != Removed {
					t.Fatal("same-ID resume failed")
				}
			} else {
				if !errors.Is(getErr, ErrMissing) || len(f.image.Attachments) != 16 {
					t.Fatal("transaction was not atomic")
				}
			}
		})
	}
}
func TestStorageFailureAndReceiptFailureResume(t *testing.T) {
	for _, name := range []string{"storage", "receipt"} {
		t.Run(name, func(t *testing.T) {
			s, f, o, a, scope := setup(t)
			p := mustPlan(t, s, a, scope)
			if name == "storage" {
				o.fail = true
			} else {
				o.after = func() { f.commitFail = true }
			}
			op, err := s.Confirm(context.Background(), a, id(10), p.ID, id(100))
			if name == "storage" && (err != nil || op.State != Pending) {
				t.Fatal(op, err)
			}
			saved, err := s.Get(context.Background(), a, id(100))
			if err != nil || saved.State != Pending || len(f.image.Attachments) != 15 {
				t.Fatal("pending recovery recipe missing")
			}
			o.fail = false
			o.after = nil
			done, err := s.Confirm(context.Background(), a, id(10), p.ID, id(100))
			if err != nil || done.State != Removed || len(f.image.Attachments) != 15 || len(f.image.Comments) != 1 {
				t.Fatal("resume changed unrelated data")
			}
		})
	}
}
func TestConcurrentSameRequest(t *testing.T) {
	s, f, _, a, scope := setup(t)
	p := mustPlan(t, s, a, scope)
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := s.Confirm(context.Background(), a, id(10), p.ID, id(100))
			if err != nil || op.State != Removed {
				t.Error(op, err)
			}
		}()
	}
	wg.Wait()
	if len(f.image.Attachments) != 15 || len(f.image.Operations) != 1 {
		t.Fatal("duplicate operation")
	}
}
func TestProcessRecovery(t *testing.T) {
	if path := os.Getenv("SYN_RECOVERY_CHILD"); path != "" {
		s, f, o, a, scope := setup(t)
		f.path = path
		p := mustPlan(t, s, a, scope)
		o.crash = true
		_, _ = s.Confirm(context.Background(), a, id(10), p.ID, id(100))
		t.Fatal("child should exit after durable commit")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessRecovery$")
	cmd.Env = append(os.Environ(), "SYN_RECOVERY_CHILD="+path)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 27 {
		t.Fatalf("child %s %v", out, err)
	}
	f := loadFixture(path)
	_, _, o, a, _ := setup(t)
	s := &Service{Store: f, Objects: o, Version: "SYN-fixed-build/" + Capability, Commit: "1f515002a9940b6fb3cb9658822970ef9426c91c"}
	saved, err := s.Get(context.Background(), a, id(100))
	if err != nil || saved.State != Pending || len(f.image.Attachments) != 15 {
		t.Fatal("process-boundary receipt absent")
	}
	done, err := s.Confirm(context.Background(), a, id(10), saved.PlanID, id(100))
	if err != nil || done.State != Removed {
		t.Fatal("process-boundary resume failed")
	}
	again := loadFixture(path)
	if again.image.Operations[id(100)].Receipt.State != Removed || again.image.Comments[id(4)] != "Original comment and reply preserved" {
		t.Fatal("durable result lost")
	}
}

func (o *fixtureObjects) VerifyDeleted(_ context.Context, url string) error {
	o.Lock()
	defer o.Unlock()
	if o.verifyFail {
		return errors.New("SYN absence check unavailable")
	}
	if o.objects[url] {
		return errors.New("SYN primary still exists")
	}
	return nil
}

func TestUnverifiedStorageMustRemainPending(t *testing.T) {
	s, f, o, a, scope := setup(t)
	p := mustPlan(t, s, a, scope)
	o.verifyFail = true
	op, err := s.Confirm(context.Background(), a, id(10), p.ID, id(100))
	if err != nil || op.State != Pending || op.PrimaryStorage != "DELETE_ACKNOWLEDGED_UNVERIFIED" || len(f.image.Attachments) != 15 {
		t.Fatal(op, err)
	}
	o.verifyFail = false
	done, err := s.Confirm(context.Background(), a, id(10), p.ID, id(100))
	if err != nil || done.State != Removed {
		t.Fatal(done, err)
	}
}

// These fixtures are SYN namespaces; real namespace binding is tested against
// LocalStorage and its operation-owned directory handle separately.
func (o *fixtureObjects) Bind(ctx context.Context, url, expected string) (ObjectTarget, error) {
	o.Lock()
	defer o.Unlock()
	ns := o.namespace
	if ns == "" {
		ns = "SYN-object-namespace-v1"
	}
	if expected != "" && expected != ns {
		return nil, errors.New("SYN namespace changed")
	}
	o.binds++
	return fixtureTarget{o, url, ns}, nil
}

type fixtureTarget struct {
	o              *fixtureObjects
	url, namespace string
}

func (t fixtureTarget) Namespace() string { return t.namespace }

func (t fixtureTarget) Delete(ctx context.Context) error        { return t.o.Delete(ctx, t.url) }
func (t fixtureTarget) VerifyDeleted(ctx context.Context) error { return t.o.VerifyDeleted(ctx, t.url) }
func (t fixtureTarget) Close() error                            { t.o.Lock(); defer t.o.Unlock(); t.o.closed++; return nil }

func TestPendingAndPlanStorageNamespaceDrift(t *testing.T) {
	for _, when := range []string{"after-preview", "after-pending", "legacy-unbound"} {
		t.Run(when, func(t *testing.T) {
			s, f, o, a, scope := setup(t)
			p := mustPlan(t, s, a, scope)
			if when != "after-preview" {
				o.fail = true
				op, err := s.Confirm(context.Background(), a, id(10), p.ID, id(100))
				if err != nil || op.State != Pending {
					t.Fatal(op, err)
				}
				o.fail = false
			}
			if when == "legacy-unbound" {
				op := f.image.Operations[id(100)]
				op.StorageNamespace = ""
				f.image.Operations[id(100)] = op
			}
			before := clone(f.image)
			beforeCalls := o.calls
			o.namespace = "SYN-another-storage-root"
			op, err := s.Confirm(context.Background(), a, id(10), p.ID, id(100))
			if err != nil || op.State == Removed || o.calls != beforeCalls {
				t.Fatal("namespace drift caused cleanup", op, err, o.calls, beforeCalls)
			}
			unchanged(t, f, before)
			if when == "after-preview" && op.State != Rejected {
				t.Fatal("drift before metadata change was not rejected")
			}
			if when != "after-preview" && op.State != Pending {
				t.Fatal("existing pending lost")
			}
			o.namespace = ""
			if when == "after-pending" {
				done, err := s.Confirm(context.Background(), a, id(10), p.ID, id(100))
				if err != nil || done.State != Removed {
					t.Fatal("stable original source could not resume", done, err)
				}
			}
			if o.binds != o.closed {
				t.Fatal("operation targets were not closed", o.binds, o.closed)
			}
		})
	}
}
