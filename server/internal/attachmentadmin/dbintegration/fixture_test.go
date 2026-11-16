//go:build attachmentadminintegration && (darwin || linux)

package dbintegration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/attachmentadmin"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/storage"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// All fixture IDs use a separate namespace, never any of the incident IDs.
var sequence atomic.Uint64
func id() string { return fmt.Sprintf("dbee0199-0000-4000-8000-%012x",sequence.Add(1)) }
const build = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

type fixture struct {
	t *testing.T
	f *testutil.Fixture
	ws, owner, member, issue, comment, other, dir string
	ids []string
	objects storage.Storage
	api http.Handler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if pool==nil { t.Fatal("dedicated target NOT RUN") }
	f := testutil.New(pool,"","")
	owner := f.User(t,"SYN Owner",id()+"@example.invalid",testutil.Cols{"id":id()})
	ws := f.Workspace(t,"SYN attachment admin",id(),testutil.Cols{"id":id(),"issue_prefix":"SYN"})
	f.WorkspaceID=ws; f.UserID=owner
	f.Member(t,ws,owner,"owner")
	member := f.User(t,"SYN member",id()+"@example.invalid",testutil.Cols{"id":id()})
	f.Member(t,ws,member,"member")
	issue := f.Issue(t,"SYN incident",testutil.Cols{"id":id()})
	comment := f.Comment(t,issue,"SYN original comment: must survive",testutil.Cols{"id":id()})
	f.Comment(t,issue,"SYN original reply: must survive",testutil.Cols{"id":id(),"parent_id":comment})
	dir:=ownedDirectory(t,"objects-")
	t.Setenv("LOCAL_UPLOAD_DIR",dir); t.Setenv("LOCAL_UPLOAD_BASE_URL","")
	objects:=storage.NewLocalStorageFromEnv(); if objects==nil { t.Fatal("SYN storage unavailable") }
	s:=&fixture{t:t,f:f,ws:ws,owner:owner,member:member,issue:issue,comment:comment,dir:dir,objects:objects}
	for range 16 { s.ids=append(s.ids,s.attachment(comment)) }
	s.other=s.attachment(comment)
	s.api=makeAPI(pool,pool,objects,build)
	// Delete only this fixture's private records; no database reset or schema DDL.
	t.Cleanup(func(){
		if _,err:=pool.Exec(context.Background(),`DELETE FROM attachment_admin_record WHERE workspace_id=$1`,ws); err!=nil { t.Error("private fixture record cleanup failed") }
	})
	return s
}

func ownedDirectory(t *testing.T,prefix string) string {
	t.Helper()
	dir,err:=os.MkdirTemp(selected.Root,prefix); if err!=nil { t.Fatal("SYN object directory unavailable") }
	d,ino,err:=directoryIdentity(dir); if err!=nil { t.Fatal("new SYN directory identity unavailable") }
	t.Cleanup(func(){
		currentD,currentI,err:=directoryIdentity(dir)
		if err!=nil || d!=currentD || ino!=currentI { t.Error("owned directory drift; cleanup refused"); return }
		if err=os.RemoveAll(dir); err!=nil { t.Error("owned SYN objects cleanup failed") }
	})
	return dir
}

func (s *fixture) attachment(comment string) string {
	x:=id(); data:=[]byte("SYN business receipt "+x)
	url,err:=s.objects.Upload(context.Background(),x,data,"application/octet-stream","SYN.bin")
	if err!=nil { s.t.Fatal("SYN upload failed") }
	s.f.Insert(s.t,"attachment",testutil.Cols{"id":x,"workspace_id":s.ws,"issue_id":s.issue,"comment_id":comment,"uploader_type":"member","uploader_id":s.owner,"filename":"SYN.bin","url":url,"content_type":"application/octet-stream","size_bytes":len(data)})
	return x
}

type starter interface { Begin(context.Context)(pgx.Tx,error) }
func makeAPI(p *pgxpool.Pool, begin starter, objects storage.Storage, commit string) http.Handler {
	q:=db.New(p)
	h:=handler.New(q,begin,nil,events.New(),nil,objects,nil,analytics.NoopClient{},handler.Config{ServerVersion:"SYN-integration",ServerCommit:commit})
	return middleware.Auth(q,nil,nil,nil)(middleware.RequireWorkspaceMember(q)(handler.RequireHumanActor(http.HandlerFunc(h.AttachmentAdmin))))
}

func jwtFor(t *testing.T, who string) string {
	t.Helper()
	v,err:=jwt.NewWithClaims(jwt.SigningMethodHS256,jwt.MapClaims{"sub":who,"email":"SYN@example.invalid","exp":time.Now().Add(time.Hour).Unix()}).SignedString(auth.JWTSecret())
	if err!=nil { t.Fatal("SYN signing failed") }; return v
}
func request(t *testing.T, api http.Handler, token, ws, method, path string, input any, want int, out any) []byte {
	t.Helper()
	r:=testutil.JSONRequest(method,attachmentadmin.Prefix+path,input)
	testutil.WithHeaders(r,"Authorization","Bearer "+token,"X-Workspace-ID",ws)
	response:=testutil.Call(t,api.ServeHTTP,r).Want(want)
	b:=append([]byte(nil),response.Body.Bytes()...)
	if out!=nil { if err:=json.Unmarshal(b,out); err!=nil { t.Fatal("invalid response JSON") } }
	return b
}
func (s *fixture) call(method,path string,input any,want int,out any) []byte {
	return request(s.t,s.api,jwtFor(s.t,s.owner),s.ws,method,path,input,want,out)
}
func (s *fixture) plan(target string) attachmentadmin.Plan {
	var p attachmentadmin.Plan
	s.call("POST","attachments/"+target+"/plans",attachmentadmin.Scope{Workspace:s.ws,Issue:s.issue,IDs:s.ids},200,&p)
	return p
}
func (s *fixture) confirm(target,plan,req string,want int) attachmentadmin.Operation {
	var op attachmentadmin.Operation
	s.call("POST","attachments/"+target+"/confirm",map[string]string{"plan_id":plan,"request_id":req},want,&op)
	return op
}
func image(t *testing.T, ws string) string {
	t.Helper(); var data []byte
	err:=pool.QueryRow(context.Background(),`SELECT jsonb_build_object('attachments',COALESCE((SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM attachment a WHERE workspace_id=$1),'[]'::jsonb),'comments',COALESCE((SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM comment c WHERE workspace_id=$1),'[]'::jsonb),'issues',COALESCE((SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM issue i WHERE workspace_id=$1),'[]'::jsonb))`,ws).Scan(&data)
	if err!=nil { t.Fatal("physical image unavailable") }; return string(data)
}
func (s *fixture) preserved() string {
	var b []byte
	// Production deletion bumps comment/issue revisions. Preserve every other
	// comment field, including original content, author, timestamps and replies.
	s.f.QueryRow(s.t,`SELECT jsonb_build_object('comments',(SELECT jsonb_agg(to_jsonb(c)-'revision' ORDER BY id) FROM comment c WHERE issue_id=$1),'other',(SELECT to_jsonb(a) FROM attachment a WHERE id=$2))`,s.issue,s.other).Scan(&b)
	return string(b)
}
func (s *fixture) exists(x string) bool { return s.f.Count(s.t,`SELECT count(*) FROM attachment WHERE id=$1 AND workspace_id=$2`,x,s.ws)==1 }
func (s *fixture) objectExists(x string) bool {
	_,err:=os.Stat(filepath.Join(s.dir,x)); if os.IsNotExist(err) { return false }; if err!=nil { s.t.Fatal("SYN object stat failed") }; return true
}
