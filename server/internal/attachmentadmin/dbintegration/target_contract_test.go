//go:build attachmentadminintegration && (darwin || linux)

package dbintegration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicitly selectable offline controls. They do not connect, open sockets,
// initialize a database or silently skip the DB suite when a target is absent.
func TestTargetContract(t *testing.T) {
	if _,err:=loadTarget("","",time.Now()); err==nil { t.Fatal("missing opt-in/target accepted") }
	if _,err:=loadTarget(filepath.Join(t.TempDir(),"missing.json"),optIn,time.Now()); err==nil { t.Fatal("unknown target accepted") }
	dir:=t.TempDir(); file:=filepath.Join(dir,"INIT-RECEIPT.json")
	bad:=target{Format:1,Purpose:optIn,Root:dir,Database:"multica",Role:"multica",Nonce:strings.Repeat("a",64),Port:55432,PID:123,SystemID:"1234567890123456789",SchemaSHA:schemaSHA,Expires:time.Now().Add(time.Hour)}
	b,_:=json.Marshal(bad)
	if err:=os.WriteFile(file,b,0600); err!=nil { t.Fatal(err) }
	if _,err:=loadTarget(file,optIn,time.Now()); err==nil { t.Fatal("non-SYN namespace accepted") }
	link:=filepath.Join(dir,"link.json"); if err:=os.Symlink(file,link); err!=nil { t.Fatal(err) }
	if _,err:=loadTarget(link,optIn,time.Now()); err==nil { t.Fatal("symlink receipt accepted") }
	if err:=os.Chmod(file,0644); err!=nil { t.Fatal(err) }
	if _,err:=loadTarget(file,optIn,time.Now()); err==nil { t.Fatal("public receipt accepted") }
}
