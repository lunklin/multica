package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeGuardianUsageRollout(t *testing.T, home, id, parent, kind, model string, at time.Time, input, cached, output int64) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "11", "04")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]any{"id": id, "parent_thread_id": parent, "source": map[string]any{"subagent": map[string]any{"other": kind}}}
	meta, err := json.Marshal(map[string]any{"type": "session_meta", "payload": metadata})
	if err != nil {
		t.Fatal(err)
	}
	data := string(meta) + "\n" + fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":%q}}`+"\n"+
		`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}}`+"\n",
		at.Format(time.RFC3339Nano), model, at.Format(time.RFC3339Nano), input, cached, output)
	path := filepath.Join(dir, "rollout-example-"+id+".jsonl")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexResultUsageIncludesOwnedGuardian(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	start := time.Now().Add(-time.Minute)
	writeGuardianUsageRollout(t, home, "review-thread", "main-thread", "guardian", "original-model", start.Add(time.Second), 100, 40, 7)
	primary := TokenUsage{InputTokens: 300, OutputTokens: 20, CacheReadTokens: 500, CostUSDTicks: 42}
	got, accounting := codexResultUsageMap(start, time.Now(), home, "main-thread", false, "original-model", primary)
	want := map[string]TokenUsage{"original-model": {InputTokens: 360, OutputTokens: 27, CacheReadTokens: 540}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage excludes owned Guardian inference: got %#v, want %#v", got, want)
	}
	if accounting.Sessions != 1 || accounting.Models["original-model"] != (TokenUsage{InputTokens: 60, OutputTokens: 7, CacheReadTokens: 40}) {
		t.Fatalf("separate safety-review breakdown missing: %+v", accounting)
	}
}

func TestCodexExecutePublishesGuardianUsageBreakdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake transport")
	}
	home := t.TempDir()
	path := writeGuardianUsageRollout(t, home, "review", "main", "guardian", "model", time.Now().Add(-time.Minute), 100, 40, 7)
	body := `read line
echo '{"jsonrpc":"2.0","id":1,"result":{}}'
read line
read line
echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"main"}}}'
read line
echo '{"jsonrpc":"2.0","id":3,"result":{}}'
echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"main","turn":{"id":"turn"}}}'
sleep 1.1
stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
` + fmt.Sprintf(`printf '{"timestamp":"%%s","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":200,"cached_input_tokens":80,"output_tokens":14}}}}\n' "$stamp" >> %q`, path) + "\n" +
		`echo '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"main","turnId":"turn","item":{"type":"agentMessage","id":"reply","text":"Delivered result"}}}'` + "\n" +
		`echo '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"main","turnId":"turn","tokenUsage":{"total":{"inputTokens":100,"cachedInputTokens":40,"outputTokens":3},"last":{"inputTokens":100,"cachedInputTokens":40,"outputTokens":3}}}}'` + "\n" +
		`echo '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"review","turnId":"review-turn","tokenUsage":{"total":{"inputTokens":100,"cachedInputTokens":40,"outputTokens":7},"last":{"inputTokens":100,"cachedInputTokens":40,"outputTokens":7}}}}'` + "\n" +
		`echo '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"review","turnId":"review-turn","item":{"type":"agentMessage","id":"private-reply","text":"Unrelated child output"}}}'` + "\n" +
		`echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"main","turn":{"id":"turn","status":"completed"}}}'` + "\n"
	backend, err := New("codex", Config{ExecutablePath: writeFakeCodexAppServer(t, body), Env: map[string]string{"CODEX_HOME": home}, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "synthetic prompt", ExecOptions{Model: "model", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var accountingMessages int
	for message := range session.Messages {
		if strings.HasPrefix(message.Content, "[Multica Runtime] Guardian safety-review usage") {
			accountingMessages++
			if !strings.Contains(message.Content, `"sessions":1`) || !strings.Contains(message.Content, "cost unreported") {
				t.Fatalf("incomplete breakdown: %s", message.Content)
			}
		}
	}
	result := <-session.Result
	if result.Status != "completed" || result.Usage["model"] != (TokenUsage{InputTokens: 120, OutputTokens: 10, CacheReadTokens: 80}) || accountingMessages != 1 {
		t.Fatalf("result=%+v; breakdown messages=%d", result, accountingMessages)
	}
	if result.Output != "Delivered result" {
		t.Fatalf("runtime or child output replaced the deliverable: %q", result.Output)
	}
}

func TestCodexGuardianUsageRequiresExactOwnershipAndSource(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	start := time.Now().Add(-time.Minute)
	for _, row := range []struct{ id, parent, kind string }{
		{"unrelated", "other-thread", "guardian"},
		{"ordinary-child", "main-thread", "explore"},
		{"missing-parent", "", "guardian"},
		{"main-thread", "main-thread", "guardian"},
	} {
		writeGuardianUsageRollout(t, home, row.id, row.parent, row.kind, "model", start.Add(time.Second), 100, 40, 7)
	}
	primary := TokenUsage{InputTokens: 20, OutputTokens: 2, CostUSDTicks: 42}
	got, _ := codexResultUsageMap(start, time.Now(), home, "main-thread", false, "model", primary)
	want := map[string]TokenUsage{"model": primary}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unowned/non-Guardian usage or primary price changed: got %#v, want %#v", got, want)
	}
}

func TestCodexGuardianUsageDeduplicatesRestoredRollouts(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	start := time.Now().Add(-time.Minute)
	path := writeGuardianUsageRollout(t, home, "review", "main", "guardian", "review-model", start.Add(time.Second), 100, 40, 7)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sessions", "rollout-restored.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	primary := TokenUsage{InputTokens: 20, OutputTokens: 2, CostUSDTicks: 42}
	got, _ := codexResultUsageMap(start, time.Now(), home, "main", false, "primary-model", primary)
	want := map[string]TokenUsage{"primary-model": primary, "review-model": {InputTokens: 60, OutputTokens: 7, CacheReadTokens: 40}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("duplicate child or distinct model misattributed: got %#v, want %#v", got, want)
	}
}

func TestCodexGuardianUsageSelectsLatestRestoredCounters(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	start := time.Now().Add(-time.Minute)
	old := writeGuardianUsageRollout(t, home, "review", "main", "guardian", "model", start.Add(time.Second), 100, 40, 7)
	current := filepath.Join(home, "sessions", "rollout-current.jsonl")
	data, err := os.ReadFile(old)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte(fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":180,"cached_input_tokens":70,"output_tokens":12}}}}`+"\n", start.Add(2*time.Second).Format(time.RFC3339Nano)))...)
	if err := os.WriteFile(current, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, start.Add(time.Second), start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(current, start.Add(2*time.Second), start.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	got, _ := codexResultUsageMap(start, time.Now(), home, "main", false, "model", TokenUsage{})
	want := map[string]TokenUsage{"model": {InputTokens: 110, OutputTokens: 12, CacheReadTokens: 70}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("latest counters not selected: got %#v, want %#v", got, want)
	}
}

func TestCodexGuardianUsageExcludesLaterExecutions(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	start := time.Now().Add(-time.Minute)
	end := start.Add(3 * time.Second)
	writeGuardianUsageRollout(t, home, "owned", "main", "guardian", "model", start.Add(time.Second), 100, 40, 7)
	writeGuardianUsageRollout(t, home, "later", "main", "guardian", "model", end.Add(time.Second), 900, 800, 70)
	got, _ := codexResultUsageMap(start, end, home, "main", false, "model", TokenUsage{})
	want := map[string]TokenUsage{"model": {InputTokens: 60, OutputTokens: 7, CacheReadTokens: 40}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("later execution charged to this run: got %#v, want %#v", got, want)
	}
}

func TestCodexGuardianUsageSubtractsHistoryOnResume(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	start := time.Now().Add(-time.Minute)
	writeGuardianUsageRollout(t, home, "old-review", "main", "guardian", "model", start.Add(-time.Second), 900, 800, 70)
	path := writeGuardianUsageRollout(t, home, "continued-review", "main", "guardian", "model", start.Add(-time.Second), 100, 40, 7)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(f, `{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":180,"cached_input_tokens":70,"output_tokens":12}}}}`+"\n", start.Add(time.Second).Format(time.RFC3339Nano))
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatalf("append: %v; close: %v", err, closeErr)
	}
	got, _ := codexResultUsageMap(start, time.Now(), home, "main", true, "model", TokenUsage{InputTokens: 20, OutputTokens: 2})
	want := map[string]TokenUsage{"model": {InputTokens: 70, OutputTokens: 7, CacheReadTokens: 30}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history double-billed: got %#v, want %#v", got, want)
	}
}

func TestCodexGuardianUsagePreservesOnlyObservedModel(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	start := time.Now().Add(-time.Minute)
	writeGuardianUsageRollout(t, home, "review", "main", "guardian", "", start.Add(time.Second), 100, 40, 7)
	got, _ := codexResultUsageMap(start, time.Now(), home, "main", false, "primary-model", TokenUsage{})
	want := map[string]TokenUsage{"unknown": {InputTokens: 60, OutputTokens: 7, CacheReadTokens: 40}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unreported model was guessed or review lost: got %#v, want %#v", got, want)
	}
}

func TestCodexGuardianUsageReadsLinkedTaskSessions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory symlinks require Windows developer mode")
	}
	home, store := t.TempDir(), t.TempDir()
	start := time.Now().Add(-time.Minute)
	writeGuardianUsageRollout(t, store, "review", "main", "guardian", "model", start.Add(time.Second), 100, 40, 7)
	if err := os.Symlink(filepath.Join(store, "sessions"), filepath.Join(home, "sessions")); err != nil {
		t.Fatal(err)
	}
	got, _ := codexResultUsageMap(start, time.Now(), home, "main", false, "model", TokenUsage{InputTokens: 20})
	want := map[string]TokenUsage{"model": {InputTokens: 80, OutputTokens: 7, CacheReadTokens: 40}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("linked rollout not accounted: got %#v, want %#v", got, want)
	}
}

func TestCodexGuardianUsageDoesNotDiscoverAmbientHomes(t *testing.T) {
	ambient := t.TempDir()
	start := time.Now().Add(-time.Minute)
	writeGuardianUsageRollout(t, ambient, "review", "main", "guardian", "model", start.Add(time.Second), 100, 40, 7)
	t.Setenv("CODEX_HOME", ambient)
	if got, _ := addCodexGuardianUsage(nil, start, time.Now(), "", "main"); got != nil {
		t.Fatalf("scanned ambient user home instead of explicit task home: %#v", got)
	}
}
