package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

func newProjectWorkflowCommand() *cobra.Command {
	workflow := &cobra.Command{Use: "workflow", Short: "Read a project's effective workflow"}
	get := &cobra.Command{
		Use: "get <project>", Short: "Read the bound workflow or workspace Default, including step handlers",
		Args: exactArgs(1), RunE: runProjectWorkflowGet,
	}
	get.Flags().String("output", "json", "Output format: json")
	workflow.AddCommand(get)
	return workflow
}

// Keep the server workflow intact, including unknown fields on steps and handlers.
// Names are a separate lookup so long step instructions are emitted only once.
type projectWorkflowReadback struct {
	ProjectID      string            `json:"project_id"`
	ResolvedSource string            `json:"resolved_source"`
	Workflow       json.RawMessage   `json:"workflow"`
	StatusNames    map[string]string `json:"status_names"`
}

func runProjectWorkflowGet(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")
	if output != "json" {
		return fmt.Errorf("project workflow get supports only --output json")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	ref, err := resolveProjectID(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}
	var project struct {
		ID         string          `json:"id"`
		WorkflowID json.RawMessage `json:"workflow_id"`
	}
	if err := client.GetJSON(ctx, "/api/projects/"+ref.ID, &project); err != nil {
		return fmt.Errorf("get project: %w", err)
	}
	if project.ID != ref.ID || len(project.WorkflowID) == 0 {
		return fmt.Errorf("invalid project response: matching id and workflow_id are required")
	}
	var workflowID string
	if err := json.Unmarshal(project.WorkflowID, &workflowID); err != nil {
		return fmt.Errorf("invalid project workflow_id: %w", err)
	}
	result := projectWorkflowReadback{ProjectID: project.ID, ResolvedSource: "project"}
	if workflowID != "" {
		if err := client.GetJSON(ctx, "/api/issue-workflows/"+workflowID, &result.Workflow); err != nil {
			return fmt.Errorf("get project workflow: %w", err)
		}
	} else {
		result.ResolvedSource = "default"
		var list struct {
			Workflows []json.RawMessage `json:"workflows"`
		}
		if err := client.GetJSON(ctx, "/api/issue-workflows", &list); err != nil {
			return fmt.Errorf("list workflows for Default: %w", err)
		}
		count := 0
		for _, raw := range list.Workflows {
			var entry struct {
				IsDefault *bool `json:"is_default"`
			}
			if err := json.Unmarshal(raw, &entry); err != nil || entry.IsDefault == nil {
				return fmt.Errorf("invalid workflow list: is_default is required")
			}
			if *entry.IsDefault {
				count++
				result.Workflow = raw
			}
		}
		if count != 1 {
			return fmt.Errorf("expected exactly one workspace Default workflow, found %d", count)
		}
	}
	keys, err := projectWorkflowStatusKeys(result.Workflow, workflowID)
	if err != nil {
		return err
	}
	var catalog struct {
		Statuses []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"statuses"`
	}
	if err := client.GetJSON(ctx, "/api/issue-statuses?include_archived=true", &catalog); err != nil {
		return fmt.Errorf("get workflow status names: %w", err)
	}
	result.StatusNames = make(map[string]string, len(keys))
	for _, status := range catalog.Statuses {
		if !keys[status.Key] {
			continue
		}
		if strings.TrimSpace(status.Name) == "" || result.StatusNames[status.Key] != "" {
			return fmt.Errorf("invalid status catalog entry for workflow step %q", status.Key)
		}
		result.StatusNames[status.Key] = status.Name
	}
	for key := range keys {
		if result.StatusNames[key] == "" {
			return fmt.Errorf("status catalog is missing workflow step %q", key)
		}
	}
	return cli.PrintJSON(os.Stdout, result)
}

func projectWorkflowStatusKeys(raw json.RawMessage, expectedID string) (map[string]bool, error) {
	var workflow struct {
		ID        string            `json:"id"`
		IsDefault *bool             `json:"is_default"`
		Steps     []json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(raw, &workflow); err != nil {
		return nil, fmt.Errorf("invalid workflow response: %w", err)
	}
	if workflow.ID == "" || workflow.IsDefault == nil || len(workflow.Steps) == 0 || (expectedID != "" && workflow.ID != expectedID) {
		return nil, fmt.Errorf("invalid workflow response: matching id, is_default and nonempty steps are required")
	}
	keys := make(map[string]bool, len(workflow.Steps))
	for _, rawStep := range workflow.Steps {
		var step struct {
			StatusKey string `json:"status_key"`
			Handler   *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"handler"`
			Instructions *string `json:"instructions"`
			Next         string  `json:"next_status_key"`
			Back         string  `json:"back_status_key"`
		}
		if err := json.Unmarshal(rawStep, &step); err != nil {
			return nil, fmt.Errorf("invalid workflow step: %w", err)
		}
		if strings.TrimSpace(step.StatusKey) == "" || keys[step.StatusKey] || step.Handler == nil || strings.TrimSpace(step.Handler.Type) == "" || step.Instructions == nil {
			return nil, fmt.Errorf("invalid workflow step: unique status_key, handler and instructions are required")
		}
		switch step.Handler.Type {
		case "agent", "member", "squad":
			if strings.TrimSpace(step.Handler.ID) == "" {
				return nil, fmt.Errorf("invalid workflow step %q: handler id is required", step.StatusKey)
			}
		}
		keys[step.StatusKey] = true
	}
	return keys, nil
}
