//go:build attachmentadminintegration && (darwin || linux)

package dbintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"
)

type childReceipt struct {
	Argv []string `json:"argv"`
	Cwd string `json:"cwd"`
	Started time.Time `json:"utc_start"`
	Finished time.Time `json:"utc_end"`
	PID *int `json:"pid"`
	ExitCode *int `json:"exit_code"`
	Terminal string `json:"terminal"`
	ProcessState string `json:"process_state"`
	RunError string `json:"run_error"`
	ContextError string `json:"context_error"`
	Stdout []byte `json:"stdout_base64"`
	Stderr []byte `json:"stderr_base64"`
}

// This records only this harness's explicit SYN child. Byte slices are encoded
// losslessly by JSON, including invalid UTF-8. Never pass an Auth collector or
// a command containing credentials to this helper. Emit before any assertion:
// a failing parent test prints its log even without -test.v.
func recordChild(t *testing.T, ctx context.Context, cmd *exec.Cmd) childReceipt {
	t.Helper()
	r := childReceipt{Argv:append([]string(nil),cmd.Args...), Cwd:cmd.Dir, Started:time.Now().UTC(), Terminal:"NOT_STARTED"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout=&stdout; cmd.Stderr=&stderr
	err := cmd.Run()
	r.Finished=time.Now().UTC()
	r.Stdout=append([]byte{},stdout.Bytes()...); r.Stderr=append([]byte{},stderr.Bytes()...)
	if err!=nil { r.RunError=err.Error() }
	if ctx.Err()!=nil { r.ContextError=ctx.Err().Error() }
	if cmd.Process!=nil { pid:=cmd.Process.Pid; r.PID=&pid; r.Terminal="UNKNOWN" }
	if cmd.ProcessState!=nil {
		r.ProcessState=cmd.ProcessState.String()
		r.Terminal="TERMINAL_WITHOUT_EXIT_CODE"
		if code:=cmd.ProcessState.ExitCode(); code>=0 { r.ExitCode=&code; r.Terminal="EXITED" }
	}
	encoded, encodeErr:=json.Marshal(r)
	if encodeErr!=nil { t.Fatal("SYN child receipt encoding failed") }
	t.Logf("SYN_CHILD_RECEIPT %s",encoded)
	return r
}
