package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type codexGuardianMetadata struct {
	ID             string `json:"id"`
	ParentThreadID string `json:"parent_thread_id"`
	Source         struct {
		Subagent struct {
			Other string `json:"other"`
		} `json:"subagent"`
	} `json:"source"`
}

type codexGuardianAccounting struct {
	Sessions int                   `json:"sessions"`
	Models   map[string]TokenUsage `json:"models"`
}

// addCodexGuardianUsage includes safety-review inference omitted from the
// parent's RPC counters. Only explicit Guardian children of this task's thread
// qualify. Restored history is delta-filtered even on a fresh parent launch.
func addCodexGuardianUsage(result map[string]TokenUsage, start, end time.Time, home, parent string) (map[string]TokenUsage, codexGuardianAccounting) {
	accounting := codexGuardianAccounting{}
	if strings.TrimSpace(home) == "" || strings.TrimSpace(parent) == "" {
		return result, accounting
	}
	root := filepath.Join(home, "sessions")
	type candidate struct {
		path string
		at   time.Time
	}
	children := make(map[string]candidate)
	for _, pattern := range []string{filepath.Join(root, "rollout-*.jsonl"), filepath.Join(root, "*", "*", "*", "rollout-*.jsonl")} {
		paths, _ := filepath.Glob(pattern)
		for _, path := range paths {
			info, err := os.Stat(path)
			if err != nil || info.ModTime().Before(start) {
				continue
			}
			meta, ok := readCodexGuardianMetadata(path)
			if !ok || meta.ID == parent || meta.ParentThreadID != parent || meta.Source.Subagent.Other != "guardian" {
				continue
			}
			previous, found := children[meta.ID]
			if !found || info.ModTime().After(previous.at) || (info.ModTime().Equal(previous.at) && path > previous.path) {
				children[meta.ID] = candidate{path: path, at: info.ModTime()}
			}
		}
	}
	for _, child := range children {
		usage := parseCodexSessionFileWindow(child.path, start, end, true)
		if usage == nil || (usage.usage.InputTokens == 0 && usage.usage.OutputTokens == 0 && usage.usage.CacheReadTokens == 0 && usage.usage.CacheWriteTokens == 0) {
			continue
		}
		model := usage.model
		if model == "" {
			model = "unknown"
		}
		accounting.Sessions++
		if accounting.Models == nil {
			accounting.Models = make(map[string]TokenUsage)
		}
		part := accounting.Models[model]
		part.InputTokens += usage.usage.InputTokens
		part.OutputTokens += usage.usage.OutputTokens
		part.CacheReadTokens += usage.usage.CacheReadTokens
		part.CacheWriteTokens += usage.usage.CacheWriteTokens
		accounting.Models[model] = part
		if result == nil {
			result = make(map[string]TokenUsage)
		}
		total := result[model]
		total.InputTokens += usage.usage.InputTokens
		total.OutputTokens += usage.usage.OutputTokens
		total.CacheReadTokens += usage.usage.CacheReadTokens
		total.CacheWriteTokens += usage.usage.CacheWriteTokens
		// The rollout has no provider invoice. A known primary cost alone must
		// not masquerade as the full cost after adding unpriced review usage.
		total.CostUSDTicks = 0
		result[model] = total
	}
	return result, accounting
}

func readCodexGuardianMetadata(path string) (codexGuardianMetadata, bool) {
	var result codexGuardianMetadata
	f, err := os.Open(path)
	if err != nil {
		return result, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 0; n < 64 && scanner.Scan(); n++ {
		if !bytesContainsStr(scanner.Bytes(), "session_meta") {
			continue
		}
		var event struct {
			Type    string                `json:"type"`
			Payload codexGuardianMetadata `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "session_meta" && event.Payload.ID != "" {
			return event.Payload, true
		}
	}
	return result, false
}
