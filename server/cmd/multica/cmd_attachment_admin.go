package main

import (
	"os"

	"github.com/multica-ai/multica/server/internal/attachmentadmin"
	"github.com/spf13/cobra"
)

type attachmentAdminExit struct{ code int }

func (e attachmentAdminExit) Error() string {
	return "attachment administration failed; see JSON result (on timeout, read the same request ID)"
}
func (e attachmentAdminExit) ExitCode() int { return e.code }

func runAttachmentAdmin(cmd *cobra.Command, args []string) error {
	if err := requireHumanLocalCommand("attachment administration"); err != nil {
		attachmentadmin.RunCLI(cmd.Context(), args, attachmentadmin.CLIConfig{Machine: true}, os.Stdout, os.Stderr)
		return attachmentAdminExit{3}
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	exit := attachmentadmin.RunCLI(cmd.Context(), args, attachmentadmin.CLIConfig{Server: client.BaseURL, Workspace: client.WorkspaceID, Token: client.Token, Machine: inDaemonManagedExecutionContext(), Client: client.HTTPClient}, os.Stdout, os.Stderr)
	if exit != 0 {
		return attachmentAdminExit{exit}
	}
	return nil
}
func init() {
	inspect := &cobra.Command{Use: "inspect <attachment-id>", Short: "Inspect metadata and removal eligibility (human Owner only)", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return runAttachmentAdmin(cmd, []string{"attachment", "inspect", args[0]})
	}}
	remove := &cobra.Command{Use: "delete <attachment-id>", Short: "Preview or confirm a bounded human Owner removal", Long: attachmentadmin.Help, Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		values := []string{"attachment", "delete", args[0]}
		dry, _ := cmd.Flags().GetBool("dry-run")
		if dry {
			values = append(values, "--dry-run")
		}
		for _, name := range []string{"scope-file", "confirm-plan", "request-id"} {
			v, _ := cmd.Flags().GetString(name)
			if v != "" {
				values = append(values, "--"+name, v)
			}
		}
		return runAttachmentAdmin(cmd, values)
	}}
	remove.Flags().Bool("dry-run", false, "Persist metadata-only preview; no attachment removal")
	remove.Flags().String("scope-file", "", "JSON scope with workspace, issue and exactly 16 UUIDs")
	remove.Flags().String("confirm-plan", "", "Unexpired server plan UUID")
	remove.Flags().String("request-id", "", "Stable operation UUID; reuse it after reading an ambiguous result")
	operation := &cobra.Command{Use: "operation", Short: "Read durable attachment removal receipts"}
	operation.AddCommand(&cobra.Command{Use: "get <request-id>", Short: "Read the original operation; 404 does not prove removal", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return runAttachmentAdmin(cmd, []string{"attachment", "operation", "get", args[0]})
	}})
	attachmentCmd.AddCommand(inspect, remove, operation)
	versionCmd.Flags().Bool("server", false, "Read server build and attachment management capability (human Owner)")
}
