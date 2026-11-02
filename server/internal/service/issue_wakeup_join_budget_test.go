package service

import (
	"context"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

func TestJoinedWakeupCannotExceedCapBeforeSchedulerSettles(t *testing.T) {
	for _, tc := range []struct {
		name, mode   string
		cap, allowed int32
	}{
		{"repeating_one", "continuous", 1, 1},
		{"repeating_two", "continuous", 2, 2},
		{"one_shot", "once", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: tc.mode, MaxFires: tc.cap,
				EventTypes: []string{"comment.created", "comment.updated"}, Instruction: "Review new evidence"})
			comment := ""
			for i := int32(0); i <= tc.allowed; i++ {
				waiting := wakeWaitingRun(t, f, issue, agent, f.UserID)
				if i == 1 {
					f.Exec(t, "UPDATE comment SET content='updated evidence' WHERE id=$1", comment)
				} else {
					comment = f.Comment(t, util.UUIDToString(issue), "new input")
				}
				notes := wakeClaim(t, f, s, waiting)
				if i < tc.allowed {
					if !strings.Contains(notes, "Review new evidence") {
						t.Fatalf("legitimate firing %d missing: %q", i, notes)
					}
					// A repeated claim is not another firing. No scheduler tick
					// occurs between these short runs.
					if again := wakeClaim(t, f, s, waiting); again != notes {
						t.Fatal("repeated claim changed reserved evidence")
					}
				} else if notes != "" {
					t.Fatalf("rule handed an excess firing to an ordinary run: %q", notes)
				}
				// The ordinary run itself remains allowed, including after the cap.
				wakeStart(t, f, waiting)
				f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE id=$1", waiting)
			}
			wakeTick(t, f, s, w.ID)
			got, err := f.q.LocklessWakeup(context.Background(), w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Enabled || got.FireCount != tc.allowed {
				t.Fatalf("cap not enforced: enabled=%t fires=%d", got.Enabled, got.FireCount)
			}
			if tc.mode == "continuous" && got.PausedReason.String != wakeupPausedMaxFires {
				t.Fatalf("missing visible pause: %q", got.PausedReason.String)
			}
		})
	}
}

func TestJoinedWakeupMultipleReceiptsReserveOneFire(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", MaxFires: 2,
		EventTypes: []string{"comment.created", "comment.updated"}, Instruction: "Review evidence"})
	first := wakeWaitingRun(t, f, issue, agent, f.UserID)
	comment := f.Comment(t, util.UUIDToString(issue), "first")
	f.Exec(t, "UPDATE comment SET content='edited' WHERE id=$1", comment)
	if notes := wakeClaim(t, f, s, first); !strings.Contains(notes, "comment.created") || !strings.Contains(notes, "comment.updated") {
		t.Fatalf("missing first inputs: %q", notes)
	}
	wakeStart(t, f, first)
	f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE id=$1", first)
	second := wakeWaitingRun(t, f, issue, agent, f.UserID)
	f.Comment(t, util.UUIDToString(issue), "second")
	if notes := wakeClaim(t, f, s, second); notes == "" {
		t.Fatal("two receipts incorrectly used two firing slots")
	}
	wakeStart(t, f, second)
	got := wakeTick(t, f, s, w.ID)
	if got.FireCount != 2 || got.Enabled {
		t.Fatalf("fires=%d enabled=%t", got.FireCount, got.Enabled)
	}
}

func TestJoinedWakeupFailedPreparationDoesNotSpendTheCap(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", MaxFires: 1,
		EventTypes: []string{"comment.created"}, Instruction: "Review evidence"})
	first := wakeWaitingRun(t, f, issue, agent, f.UserID)
	comment := f.Comment(t, util.UUIDToString(issue), "pending input")
	if notes := wakeClaim(t, f, s, first); notes == "" {
		t.Fatal("input was not reserved")
	}
	f.Exec(t, "UPDATE agent_task_queue SET status='failed',completed_at=clock_timestamp() WHERE id=$1", first)
	second := wakeWaitingRun(t, f, issue, agent, f.UserID)
	// Recovery returns undelivered inputs before the next claim.
	wakeTick(t, f, s, w.ID)
	if notes := wakeClaim(t, f, s, second); !strings.Contains(notes, comment) {
		t.Fatalf("failed preparation spent capacity or lost input: %q", notes)
	}
	wakeStart(t, f, second)
	got := wakeTick(t, f, s, w.ID)
	if got.FireCount != 1 || got.Enabled {
		t.Fatalf("fires=%d enabled=%t", got.FireCount, got.Enabled)
	}
}
