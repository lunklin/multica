// This small distribution shares the native multica command implementation.
// It deliberately never reads profiles or daemon state.
package main

import (
	"context"
	"github.com/multica-ai/multica/server/internal/attachmentadmin"
	"os"
)

func main() {
	os.Exit(attachmentadmin.RunCLI(context.Background(), os.Args[1:], attachmentadmin.CLIConfig{Server: os.Getenv("MULTICA_SERVER_URL"), Workspace: os.Getenv("MULTICA_WORKSPACE_ID"), Token: os.Getenv("MULTICA_TOKEN"), Machine: os.Getenv("MULTICA_AGENT_ID") != "" || os.Getenv("MULTICA_TASK_ID") != "" || os.Getenv("MULTICA_DAEMON_PORT") != ""}, os.Stdout, os.Stderr))
}
