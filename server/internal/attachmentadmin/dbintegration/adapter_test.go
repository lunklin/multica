//go:build attachmentadminintegration && (darwin || linux)

package dbintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/attachmentadmin"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/storage"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestOwnerAndPrivatePlan(t *testing.T) {
	s:=newFixture(t); before:=s.preserved(); p:=s.plan(s.ids[0])
	revision:=s.f.Count(t,`SELECT revision FROM comment WHERE id=$1`,s.comment)
	var stored []byte
	s.f.QueryRow(t,`SELECT data FROM attachment_admin_record WHERE kind='plan' AND id=$1`,p.ID).Scan(&stored)
	var wrapper struct { Plan attachmentadmin.Plan `json:"plan"`; Namespace string `json:"storage_namespace"` }
	if json.Unmarshal(stored,&wrapper)!=nil || wrapper.Namespace=="" || wrapper.Plan.ID!=p.ID { t.Fatal("private plan namespace did not persist") }
	public,_:=json.Marshal(p)
	if bytes.Contains(public,[]byte("storage_namespace")) || bytes.Contains(public,[]byte(s.dir)) { t.Fatal("private location exposed in public plan") }
	req:=id(); op:=s.confirm(s.ids[0],p.ID,req,200)
	if op.State!=attachmentadmin.Removed || !op.MetadataRemoved || op.PrimaryStorage!="ABSENCE_VERIFIED" || s.exists(s.ids[0]) || s.objectExists(s.ids[0]) { t.Fatal("owner removal not verified") }
	var get attachmentadmin.Operation; s.call("GET","operations/"+req,nil,200,&get)
	if get.ID!=op.ID || get.State!=op.State || s.preserved()!=before { t.Fatal("receipt or preserved rows changed") }
	if again:=s.confirm(s.ids[0],p.ID,req,200); again.ID!=req || again.State!=op.State { t.Fatal("same ID not idempotent") }
	if s.f.Count(t,`SELECT revision FROM comment WHERE id=$1`,s.comment)!=revision+1 { t.Fatal("original comment revision did not change exactly once") }
}

func TestHumanMembershipAndTaskBinding(t *testing.T) {
	s:=newFixture(t)
	path:="attachments/"+s.ids[0]+"/plans"
	scope:=attachmentadmin.Scope{Workspace:s.ws,Issue:s.issue,IDs:s.ids}
	// The foreign workspace contains a real member/owner, not a forged header.
	foreign:=s.f.Workspace(t,"SYN foreign",id(),testutil.Cols{"id":id(),"issue_prefix":"SYN"})
	s.f.Member(t,foreign,s.owner,"owner")
	runtime:=s.f.Runtime(t,"SYN runtime")
	agent:=s.f.Agent(t,"SYN agent",runtime)
	task:=s.f.Task(t,agent,testutil.Cols{"issue_id":s.issue})
	token,err:=auth.GenerateAgentTaskToken(); if err!=nil { t.Fatal("SYN token generation failed") }
	s.f.Insert(t,"task_token",testutil.Cols{"token_hash":auth.HashToken(token),"task_id":task,"agent_id":agent,"workspace_id":s.ws,"user_id":s.owner,"expires_at":time.Now().Add(time.Hour)})
	before:=image(t,s.ws)
	request(t,s.api,jwtFor(t,s.member),s.ws,"POST",path,scope,403,nil)
	request(t,s.api,jwtFor(t,s.owner),foreign,"GET","attachments/"+s.ids[0],nil,404,nil)
	request(t,s.api,token,s.ws,"POST",path,scope,403,nil)
	request(t,s.api,token,foreign,"POST",path,scope,403,nil)
	if image(t,s.ws)!=before || s.f.Count(t,`SELECT count(*) FROM attachment_admin_record WHERE workspace_id=$1`,s.ws)!=0 { t.Fatal("denied actor mutated business rows or private plans") }
}

func TestRequestIDCannotCrossWorkspaces(t *testing.T) {
	a:=newFixture(t); b:=newFixture(t); pa:=a.plan(a.ids[0]); pb:=b.plan(b.ids[0]); req:=id()
	if op:=a.confirm(a.ids[0],pa.ID,req,200); op.State!=attachmentadmin.Removed { t.Fatal("first workspace operation failed") }
	before:=image(t,b.ws)
	b.confirm(b.ids[0],pb.ID,req,409)
	if image(t,b.ws)!=before || !b.objectExists(b.ids[0]) { t.Fatal("cross-workspace request ID mutated the second target") }
	var op attachmentadmin.Operation; a.call("GET","operations/"+req,nil,200,&op)
	if op.Target.Workspace!=a.ws || op.Target.ID!=a.ids[0] { t.Fatal("original workspace receipt overwritten") }
}

func TestCapturedExpiryBuildAndDrift(t *testing.T) {
	for _,which:=range []string{"captured","expiry","build","metadata"} { t.Run(which,func(t *testing.T){
		s:=newFixture(t); x:=s.ids[0]; p:=s.plan(x)
		switch which {
		case "captured": s.f.Exec(t,`UPDATE attachment SET source_context_id=$1 WHERE id=$2`,id(),x)
		case "expiry": s.f.Exec(t,`UPDATE attachment_admin_record SET data=jsonb_set(data,'{plan,expires_at}',to_jsonb('2000-01-01T00:00:00Z'::text)) WHERE kind='plan' AND id=$1`,p.ID)
		case "build": s.api=makeAPI(pool,pool,s.objects,"dddddddddddddddddddddddddddddddddddddddd")
		case "metadata": s.f.Exec(t,`UPDATE attachment SET filename='SYN drift' WHERE id=$1`,x)
		}
		before:=image(t,s.ws); op:=s.confirm(x,p.ID,id(),200)
		want:="metadata_drift"
		if which=="captured" { want="captured_context_immutable" }
		if which=="expiry" || which=="build" { want="plan_expired_or_version_changed" }
		if op.State!=attachmentadmin.Rejected || op.Reason!=want || image(t,s.ws)!=before || !s.objectExists(x) { t.Fatal("guard did not retain metadata/object with a precise rejection") }
	}) }
	s:=newFixture(t)
	s.api=makeAPI(pool,pool,s.objects,"unknown")
	s.call("GET","version",nil,503,nil)
}

// Faults wrap real pgx transactions; neither fixture nor assertion implements
// the service's policy. The callback still executes all production SQL.
type commitFault struct {
	*pgxpool.Pool
	armed atomic.Bool
	after bool
	exitAfter bool
	observed atomic.Bool
	request, target string
}
func (f *commitFault) Begin(ctx context.Context)(pgx.Tx,error) {
	tx,err:=f.Pool.Begin(ctx); if err!=nil { return nil,err }; return faultTx{Tx:tx,f:f},nil
}
type faultTx struct { pgx.Tx; f *commitFault }
func (tx faultTx) Commit(ctx context.Context) error {
	if !tx.f.armed.CompareAndSwap(true,false) { return tx.Tx.Commit(ctx) }
	var ops,attachments int
	if err:=tx.QueryRow(ctx,`SELECT (SELECT count(*) FROM attachment_admin_record WHERE kind='operation' AND id=$1),(SELECT count(*) FROM attachment WHERE id=$2)`,tx.f.request,tx.f.target).Scan(&ops,&attachments); err!=nil { return err }
	tx.f.observed.Store(ops==1 && attachments==0)
	if tx.f.after { if err:=tx.Tx.Commit(ctx); err!=nil { return err }; if tx.f.exitAfter { os.Exit(86) } }
	return errors.New("SYN injected commit response unavailable")
}

func TestMetadataAndReceiptRollbackTogether(t *testing.T) {
	s:=newFixture(t); p:=s.plan(s.ids[0]); req:=id()
	f:=&commitFault{Pool:pool,request:req,target:s.ids[0]}
	s.api=makeAPI(pool,f,s.objects,build); before:=image(t,s.ws); f.armed.Store(true)
	s.confirm(s.ids[0],p.ID,req,500)
	if !f.observed.Load() || image(t,s.ws)!=before || !s.objectExists(s.ids[0]) || s.f.Count(t,`SELECT count(*) FROM attachment_admin_record WHERE kind='operation' AND id=$1`,req)!=0 { t.Fatal("rollback did not retain full before image and remove the tentative receipt") }
	// The same request may proceed after a known rollback; it must not require
	// a new ID or fabricate completion from the first 500.
	if op:=s.confirm(s.ids[0],p.ID,req,200); op.State!=attachmentadmin.Removed { t.Fatal("retry after rollback failed") }
}

func TestLostCommitResponseAndNewProcessResume(t *testing.T) {
	s:=newFixture(t); p:=s.plan(s.ids[0]); req:=id(); preserved:=s.preserved()
	f:=&commitFault{Pool:pool,after:true,request:req,target:s.ids[0]}
	s.api=makeAPI(pool,f,s.objects,build); f.armed.Store(true)
	s.confirm(s.ids[0],p.ID,req,500)
	if !f.observed.Load() || s.exists(s.ids[0]) || !s.objectExists(s.ids[0]) { t.Fatal("commit ambiguity boundary not reached") }
	var pending attachmentadmin.Operation
	s.call("GET","operations/"+req,nil,200,&pending)
	if pending.State!=attachmentadmin.Pending { t.Fatal("same request ID did not recover durable pending receipt") }
	var recipe struct { URL string `json:"object_url"`; Namespace string `json:"storage_namespace"` }
	var raw []byte
	s.f.QueryRow(t,`SELECT data FROM attachment_admin_record WHERE kind='operation' AND id=$1`,req).Scan(&raw)
	if json.Unmarshal(raw,&recipe)!=nil || recipe.URL=="" || recipe.Namespace=="" { t.Fatal("durable cleanup recipe missing") }
	input,_:=json.Marshal(resumeInput{s.ws,s.owner,s.ids[0],p.ID,req,s.dir})
	binary,err:=os.Executable(); if err!=nil { t.Fatal("self executable unavailable") }
	ctx,cancel:=context.WithTimeout(context.Background(),20*time.Second); defer cancel()
	cmd:=exec.CommandContext(ctx,binary,"-test.run=^TestResumeChild$","-syn-resume-child","-syn-target="+*targetFile,"-syn-opt-in="+optIn)
	cmd.Env=[]string{"PATH=/usr/bin:/bin","TMPDIR="+selected.Root,"GOMAXPROCS=2","GOMEMLIMIT=1GiB"}
	cmd.Dir=selected.Root; cmd.Stdin=bytes.NewReader(input)
	var stdout,stderr bytes.Buffer; cmd.Stdout=&stdout; cmd.Stderr=&stderr
	if err=cmd.Run(); err!=nil { t.Fatal("new process failed; child output retained by test runner only after SYN review") }
	t.Logf("SYN child pid=%d exit=%d stdout=%q stderr=%q",cmd.Process.Pid,cmd.ProcessState.ExitCode(),stdout.String(),stderr.String())
	s.call("GET","operations/"+req,nil,200,&pending)
	if pending.State!=attachmentadmin.Removed || s.objectExists(s.ids[0]) || s.preserved()!=preserved || s.f.Count(t,`SELECT count(*) FROM attachment_admin_record WHERE kind='operation' AND id=$1`,req)!=1 { t.Fatal("new-process continuation did not converge without collateral change") }
}

type resumeInput struct { Workspace,Owner,Target,Plan,Request,Directory string }
func TestResumeChild(t *testing.T) {
	if !*resumeChild && !*commitExitChild { return }
	var in resumeInput
	d:=json.NewDecoder(os.Stdin); d.DisallowUnknownFields()
	if d.Decode(&in)!=nil { t.Fatal("invalid SYN continuation input") }
	for _,v:=range []string{in.Workspace,in.Owner,in.Target,in.Plan,in.Request} { if !strings.HasPrefix(v,"dbee0199-") && v!=in.Plan { t.Fatal("non-SYN continuation ID") } }
	// Plan IDs are generated by the production service; all must still be UUIDs.
	for _,v:=range []string{in.Workspace,in.Owner,in.Target,in.Plan,in.Request} { if !attachmentadmin.ValidID(v) { t.Fatal("invalid continuation ID") } }
	if filepath.Dir(in.Directory)!=selected.Root || !strings.HasPrefix(filepath.Base(in.Directory),"objects-") { t.Fatal("continuation storage outside dedicated target") }
	if _,_,err:=directoryIdentity(in.Directory); err!=nil { t.Fatal("continuation storage identity invalid") }
	os.Setenv("LOCAL_UPLOAD_DIR",in.Directory); os.Setenv("LOCAL_UPLOAD_BASE_URL","")
	objects:=storage.NewLocalStorageFromEnv()
	var begin starter=pool
	if *commitExitChild { f:=&commitFault{Pool:pool,after:true,exitAfter:true,request:in.Request,target:in.Target}; f.armed.Store(true); begin=f }
	api:=makeAPI(pool,begin,objects,build)
	var op attachmentadmin.Operation
	if *commitExitChild {
		request(t,api,jwtFor(t,in.Owner),in.Workspace,"POST","attachments/"+in.Target+"/confirm",map[string]string{"plan_id":in.Plan,"request_id":in.Request},200,&op)
		t.Fatal("controlled post-commit process exit was not reached")
	}
	request(t,api,jwtFor(t,in.Owner),in.Workspace,"GET","operations/"+in.Request,nil,200,&op)
	if op.State!=attachmentadmin.Pending { t.Fatal("child did not read original pending state first") }
	request(t,api,jwtFor(t,in.Owner),in.Workspace,"POST","attachments/"+in.Target+"/confirm",map[string]string{"plan_id":in.Plan,"request_id":in.Request},200,&op)
	if op.State!=attachmentadmin.Removed { t.Fatal("child could not resume original recipe") }
}

func TestWorkerExitAfterCommit(t *testing.T) {
	s:=newFixture(t); p:=s.plan(s.ids[0]); req:=id(); preserved:=s.preserved()
	input,_:=json.Marshal(resumeInput{s.ws,s.owner,s.ids[0],p.ID,req,s.dir})
	binary,err:=os.Executable(); if err!=nil { t.Fatal("self executable unavailable") }
	for _,phase:=range []string{"-syn-commit-exit-child","-syn-resume-child"} {
		ctx,cancel:=context.WithTimeout(context.Background(),20*time.Second)
		cmd:=exec.CommandContext(ctx,binary,"-test.run=^TestResumeChild$",phase,"-syn-target="+*targetFile,"-syn-opt-in="+optIn)
		cmd.Env=[]string{"PATH=/usr/bin:/bin","TMPDIR="+selected.Root,"GOMAXPROCS=2","GOMEMLIMIT=1GiB"}
		cmd.Dir=selected.Root; cmd.Stdin=bytes.NewReader(input)
		var stdout,stderr bytes.Buffer; cmd.Stdout=&stdout; cmd.Stderr=&stderr
		err=cmd.Run(); cancel()
		if cmd.ProcessState==nil { t.Fatal("SYN worker did not start") }
		want:=0; if phase=="-syn-commit-exit-child" { want=86 }
		if cmd.ProcessState.ExitCode()!=want || want==0 && err!=nil { t.Fatalf("SYN worker exit %d, want %d",cmd.ProcessState.ExitCode(),want) }
		t.Logf("SYN worker pid=%d exit=%d stdout=%q stderr=%q",cmd.Process.Pid,cmd.ProcessState.ExitCode(),stdout.String(),stderr.String())
		var op attachmentadmin.Operation; s.call("GET","operations/"+req,nil,200,&op)
		if want==86 && (op.State!=attachmentadmin.Pending || s.exists(s.ids[0]) || !s.objectExists(s.ids[0])) { t.Fatal("post-commit process exit lost pending recipe") }
		if want==0 && (op.State!=attachmentadmin.Removed || s.objectExists(s.ids[0])) { t.Fatal("fresh process failed continuation") }
	}
	if s.preserved()!=preserved || s.f.Count(t,`SELECT count(*) FROM attachment_admin_record WHERE kind='operation' AND id=$1`,req)!=1 { t.Fatal("worker continuation changed unrelated rows or duplicated receipt") }
}

func TestTwoConnectionsCompete(t *testing.T) {
	for _,sameID:=range []bool{true,false} { name:="different-ID"; if sameID { name="same-ID" }; t.Run(name,func(t *testing.T){
		s:=newFixture(t); p:=s.plan(s.ids[0]); preserved:=s.preserved()
		ctx,cancel:=context.WithTimeout(context.Background(),10*time.Second); defer cancel()
		second,err:=selected.connect(ctx); if err!=nil { t.Fatal("second verified pool unavailable") }; defer second.Close()
		var firstPID,secondPID int
		if pool.QueryRow(ctx,`SELECT pg_backend_pid()`).Scan(&firstPID)!=nil || second.QueryRow(ctx,`SELECT pg_backend_pid()`).Scan(&secondPID)!=nil || firstPID==secondPID { t.Fatal("two independent PostgreSQL connections not observed") }
		t.Logf("SYN PostgreSQL backends %d %d",firstPID,secondPID)
		apis:=[]http.Handler{s.api,makeAPI(second,second,s.objects,build)}
		reqs:=[]string{id(),id()}; if sameID { reqs[1]=reqs[0] }
		start:=make(chan struct{}); results:=make(chan *httptest.ResponseRecorder,2); var wg sync.WaitGroup
		for i:=range 2 { wg.Add(1); go func(i int){ defer wg.Done(); <-start
			r:=testutil.JSONRequest("POST",attachmentadmin.Prefix+"attachments/"+s.ids[0]+"/confirm",map[string]string{"plan_id":p.ID,"request_id":reqs[i]})
			testutil.WithHeaders(r,"Authorization","Bearer "+jwtFor(t,s.owner),"X-Workspace-ID",s.ws)
			w:=httptest.NewRecorder(); apis[i].ServeHTTP(w,r); results<-w
		}(i) }
		close(start); wg.Wait(); close(results)
		success:=0
		for w:=range results { if w.Code==200 { var op attachmentadmin.Operation; if json.Unmarshal(w.Body.Bytes(),&op)!=nil || op.State!=attachmentadmin.Removed { t.Fatal("concurrent receipt not complete") }; success++ } else if sameID || w.Code!=404 { t.Fatalf("unexpected concurrent status %d",w.Code) } }
		want:=1; if sameID { want=2 }
		if success!=want || s.exists(s.ids[0]) || s.objectExists(s.ids[0]) || s.preserved()!=preserved || s.f.Count(t,`SELECT count(*) FROM attachment_admin_record WHERE kind='operation' AND workspace_id=$1`,s.ws)!=1 { t.Fatal("competing requests did not produce exactly one durable operation") }
	}) }
}

type failingStorage struct { storage.Storage }
func (s failingStorage) BindDeletion(ctx context.Context,key string)(storage.DeletionTarget,error) {
	target,err:=s.Storage.(storage.DeletionBinder).BindDeletion(ctx,key)
	if err!=nil { return nil,err }; return failedDelete{target},nil
}
type failedDelete struct { storage.DeletionTarget }
func (failedDelete) Delete(context.Context) error { return errors.New("SYN primary storage unavailable") }

func TestStoragePendingAndOriginalNamespace(t *testing.T) {
	s:=newFixture(t); p:=s.plan(s.ids[0]); req:=id(); preserved:=s.preserved()
	s.api=makeAPI(pool,pool,failingStorage{s.objects},build)
	op:=s.confirm(s.ids[0],p.ID,req,200)
	if op.State!=attachmentadmin.Pending || op.Reason!="primary_storage_cleanup_failed" || s.exists(s.ids[0]) || !s.objectExists(s.ids[0]) { t.Fatal("storage failure was treated as completion") }
	// Reopen at the same URL against another physical directory. The original
	// durable namespace must reject it and leave both directories untouched.
	other:=ownedDirectory(t,"replacement-")
	t.Setenv("LOCAL_UPLOAD_DIR",other)
	replacement:=storage.NewLocalStorageFromEnv()
	if _,err:=replacement.Upload(context.Background(),s.ids[0],[]byte("SYN unrelated replacement"),"application/octet-stream",""); err!=nil { t.Fatal("SYN replacement upload failed") }
	s.api=makeAPI(pool,pool,replacement,build)
	op=s.confirm(s.ids[0],p.ID,req,200)
	if op.State!=attachmentadmin.Pending || op.Reason!="storage_namespace_unavailable_or_changed" || !s.objectExists(s.ids[0]) { t.Fatal("wrong namespace was accepted") }
	if b,err:=os.ReadFile(filepath.Join(other,s.ids[0])); err!=nil || string(b)!="SYN unrelated replacement" { t.Fatal("unrelated replacement object changed") }
	s.api=makeAPI(pool,pool,s.objects,build)
	if op=s.confirm(s.ids[0],p.ID,req,200); op.State!=attachmentadmin.Removed || s.objectExists(s.ids[0]) || s.preserved()!=preserved { t.Fatal("original namespace continuation failed") }
}
