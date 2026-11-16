//go:build attachmentadminintegration && (darwin || linux)

// Package dbintegration exercises the production adapter without importing the
// handler package's database-creating/skipping TestMain. It is opt-in source,
// not a deployment command and not a replacement for the product CLI.
package dbintegration

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const harnessVersion = "0.1.1-source-candidate"

var targetFile = flag.String("syn-target", "", "absolute private INIT-RECEIPT.json; no default")
var consent = flag.String("syn-opt-in", "", "must equal CHEE-199-DEDICATED-SYN-ONLY")
var help = flag.Bool("syn-help", false, "print the harness contract without file/network access")
var version = flag.Bool("syn-version", false, "print the harness version without file/network access")
var offline = flag.Bool("syn-offline", false, "only TestTargetContract; no connection or fixture")
var resumeChild = flag.Bool("syn-resume-child", false, "internal same-binary continuation; SYN IDs on stdin")
var commitExitChild = flag.Bool("syn-commit-exit-child", false, "internal SYN process exit immediately after real commit")
var selected target
var pool *pgxpool.Pool

func summary(state string, code int) {
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"harness":harnessVersion,"state":state,"exit":code,"scope":"dedicated SYN PostgreSQL adapter; not deployment or real Owner login"})
}

func TestMain(m *testing.M) {
	flag.Parse()
	if *help {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"usage":"dbintegration.test -syn-target /private/SYN/INIT-RECEIPT.json -syn-opt-in CHEE-199-DEDICATED-SYN-ONLY -test.v", "version":harnessVersion, "exit_codes":map[string]int{"pass":0,"assertion_failure":1,"not_run_target_or_environment":78},"offline":"-syn-offline -test.run ^TestTargetContract$","json":"go test -json or go tool test2json; final summary is JSON","no_fallback":true})
		os.Exit(0)
	}
	if *version { summary("SOURCE_CANDIDATE",0); os.Exit(0) }
	// A successful m.Run can mean no assertion ran. Reject filtering/listing or
	// zero/repeated counts before any mode reaches m.Run or opens a target.
	for name, expected := range map[string]string{"test.skip":"", "test.list":"", "test.count":"1"} {
		selection := flag.Lookup(name)
		if selection == nil || selection.Value.String() != expected { summary("test_selection_would_change_required_coverage_NOT_RUN",78); os.Exit(78) }
	}
	if *offline {
		if flag.Lookup("test.run").Value.String() != "^TestTargetContract$" || *targetFile != "" || *resumeChild || *commitExitChild { summary("invalid_offline_selection_NOT_RUN",78); os.Exit(78) }
		os.Exit(m.Run())
	}
	if (*resumeChild || *commitExitChild) && (flag.Lookup("test.run").Value.String()!="^TestResumeChild$" || *resumeChild && *commitExitChild) { summary("invalid_child_selection_NOT_RUN",78); os.Exit(78) }
	if !*resumeChild && !*commitExitChild && flag.Lookup("test.run").Value.String()!="" { summary("DB_suite_requires_full_selection_NOT_RUN",78); os.Exit(78) }
	// Reject ambient credentials and service settings without printing values.
	// Future execution uses env -i with explicit PATH/temp/Go variables only.
	for _, entry := range os.Environ() {
		name := strings.SplitN(entry,"=",2)[0]
		if strings.HasPrefix(name,"PG") || strings.HasPrefix(name,"MULTICA_") || strings.HasPrefix(name,"AWS_") || strings.HasPrefix(name,"LOCAL_UPLOAD_") || strings.HasPrefix(name,"LLM_") || name=="DATABASE_URL" || name=="JWT_SECRET" { summary("ambient_service_configuration_NOT_RUN",78); os.Exit(78) }
	}
	var err error
	selected, err = loadTarget(*targetFile,*consent,time.Now().UTC())
	if err != nil { summary(err.Error(),78); os.Exit(78) }
	ctx, cancel := context.WithTimeout(context.Background(),10*time.Second)
	pool, err = selected.connect(ctx); cancel()
	if err != nil { summary(err.Error(),78); os.Exit(78) }
	// The secret is deliberately public SYN material, never a deployment value.
	os.Setenv("JWT_SECRET","chee199-dbintegration-public-synthetic-signing-only")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard,nil)))
	code := m.Run()
	pool.Close()
	state := "FAIL"; if code==0 { state="PASS" }
	summary(state,code)
	os.Exit(code)
}
