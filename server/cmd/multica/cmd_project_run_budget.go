package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

func newProjectRunBudgetCommand() *cobra.Command {
	budget := &cobra.Command{Use: "run-budget", Short: "Read or configure a project's hard new-run allowance"}
	get := &cobra.Command{Use: "get <project>", Short: "Read remaining admissions and stop reason", Args: exactArgs(1), RunE: runProjectBudgetGet}
	set := &cobra.Command{Use: "set <project>", Short: "Human admin: configure allowance without resetting usage", Args: exactArgs(1), RunE: runProjectBudgetSet}
	set.Flags().Int64("max-runs", 0, "Total admissions since this budget was armed (including failed/cancelled/retry runs)")
	set.Flags().String("deadline", "", "Future admission deadline (RFC3339)")
	set.Flags().Int64("expected-revision", 0, "Observed revision; 0 arms a new budget")
	set.Flags().Bool("paused", false, "Pause admissions; explicit --paused=false acknowledges a failure stop without resetting usage")
	set.Flags().Bool("stop-on-failure", false, "Pause this project after an actual Run failure; omit to preserve the policy")
	for _, flag := range []string{"max-runs", "deadline", "expected-revision"} {
		_ = set.MarkFlagRequired(flag)
	}
	budget.AddCommand(get, set)
	return budget
}

func projectBudgetClient(cmd *cobra.Command, project string) (*cli.APIClient, string, error) {
	c, err := newAPIClient(cmd)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	ref, err := resolveProjectID(ctx, c, project)
	if err != nil {
		return nil, "", fmt.Errorf("resolve project: %w", err)
	}
	return c, "/api/projects/" + ref.ID + "/run-budget", nil
}

func runProjectBudgetGet(cmd *cobra.Command, args []string) error {
	c, path, err := projectBudgetClient(cmd, args[0])
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

func runProjectBudgetSet(cmd *cobra.Command, args []string) error {
	maxRuns, _ := cmd.Flags().GetInt64("max-runs")
	revision, _ := cmd.Flags().GetInt64("expected-revision")
	deadline, _ := cmd.Flags().GetString("deadline")
	paused, _ := cmd.Flags().GetBool("paused")
	if maxRuns < 1 || maxRuns > 1000000 || revision < 0 {
		return fmt.Errorf("max-runs must be 1-1000000 and expected-revision must be non-negative")
	}
	if _, err := time.Parse(time.RFC3339, deadline); err != nil {
		return fmt.Errorf("deadline must be RFC3339: %w", err)
	}
	c, path, err := projectBudgetClient(cmd, args[0])
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var result map[string]any
	body := map[string]any{"max_runs": maxRuns, "expected_revision": revision, "deadline": deadline}
	if cmd.Flags().Changed("paused") {
		body["paused"] = paused
	}
	if cmd.Flags().Changed("stop-on-failure") {
		stopOnFailure, _ := cmd.Flags().GetBool("stop-on-failure")
		body["stop_on_failure"] = stopOnFailure
	}
	if err := c.PutJSON(ctx, path, body, &result); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, result)
}
