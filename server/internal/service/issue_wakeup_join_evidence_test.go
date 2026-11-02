package service

import (
	"context"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

func TestJoinedWakeupKeepsInputArrivingAfterRunStarted(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous",
		EventTypes: []string{"comment.created"}, Instruction: "Review new comments"})
	first := wakeWaitingRun(t, f, issue, agent, f.UserID)
	f.Comment(t, util.UUIDToString(issue), "first input")
	if notes := wakeClaim(t, f, s, first); notes == "" {
		t.Fatal("first input was not joined")
	}
	wakeStart(t, f, first)
	secondComment := f.Comment(t, util.UUIDToString(issue), "arrived after the prompt was delivered")
	w = wakeTick(t, f, s, w.ID)
	followup, err := f.q.GetAgentTask(context.Background(), w.LastTaskID)
	if err != nil {
		t.Fatal(err)
	}
	// The active run cannot read an event that arrived after its claim. Queue
	// it for the normal serialized follow-up, rather than consume it unseen.
	if followup.ID == parseTestUUID(t, first) || !strings.Contains(followup.HandoffNote.String, secondComment) {
		t.Fatalf("new comment was not delivered to a follow-up: task=%s note=%q", util.UUIDToString(followup.ID), followup.HandoffNote.String)
	}
	f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE id=$1", first)
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("new input started %d follow-up runs, want 1", n)
	}
}
