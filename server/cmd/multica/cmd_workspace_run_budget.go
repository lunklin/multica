package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

func init() {
	budget := &cobra.Command{Use: "run-budget", Short: "Read or configure a workspace's hard new-run allowance"}
	get := &cobra.Command{Use: "get [workspace]", Short: "Read remaining admissions and stop reason", Args: cobra.MaximumNArgs(1), RunE: runWorkspaceBudgetGet}
	set := &cobra.Command{Use: "set [workspace]", Short: "Human admin: configure allowance without resetting usage", Args: cobra.MaximumNArgs(1), RunE: runWorkspaceBudgetSet}
	set.Flags().Int64("max-runs", 0, "Total admissions since this budget was armed (including failed/cancelled/retry runs)")
	set.Flags().String("deadline", "", "Future admission deadline (RFC3339)")
	set.Flags().Int64("expected-revision", 0, "Observed revision; 0 arms a new budget")
	set.Flags().Bool("paused", false, "Pause new admissions; does not cancel existing work")
	for _, flag := range []string{"max-runs", "deadline", "expected-revision"} {
		_ = set.MarkFlagRequired(flag)
	}
	budget.AddCommand(get, set)
	workspaceCmd.AddCommand(budget)
}

func workspaceBudgetClient(cmd *cobra.Command, args []string) (*cli.APIClient, string, error) {
	ws, err := resolveWorkspaceArg(cmd, args)
	if err != nil {
		return nil, "", err
	}
	if ws == "" {
		return nil, "", fmt.Errorf("workspace is required")
	}
	c, err := newAPIClient(cmd)
	return c, "/api/workspaces/" + ws + "/run-budget", err
}

func runWorkspaceBudgetGet(cmd *cobra.Command, args []string) error {
	c, path, err := workspaceBudgetClient(cmd, args)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	if err := c.GetJSON(ctx, path, &result); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runWorkspaceBudgetSet(cmd *cobra.Command, args []string) error {
	c, path, err := workspaceBudgetClient(cmd, args)
	if err != nil {
		return err
	}
	maxRuns, _ := cmd.Flags().GetInt64("max-runs")
	revision, _ := cmd.Flags().GetInt64("expected-revision")
	deadline, _ := cmd.Flags().GetString("deadline")
	paused, _ := cmd.Flags().GetBool("paused")
	if _, err := time.Parse(time.RFC3339, deadline); err != nil {
		return fmt.Errorf("deadline must be RFC3339: %w", err)
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	if err := c.PutJSON(ctx, path, map[string]any{"max_runs": maxRuns, "expected_revision": revision, "deadline": deadline, "paused": paused}, &result); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, result)
}
