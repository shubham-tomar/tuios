package tuie2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gaurav-Gosain/tuitest"
)

// The cards that move on their own (internal/session/task_auto.go), through
// the binary a person runs. Each test leaves the history of the cards it
// moved as its artifact.

// taskFanRow is one card of tuios task ls --json, with its fan.
type taskFanRow struct {
	ID     string `json:"id"`
	Stage  string `json:"stage"`
	Holder *struct {
		Window string `json:"window"`
	} `json:"holder"`
	Fan *struct {
		Fan   string `json:"fan"`
		FanID string `json:"fan_id"`
	} `json:"fan"`
}

func taskFanRows(t *testing.T, base, session string) map[string]taskFanRow {
	t.Helper()
	var l struct {
		Tasks []taskFanRow `json:"tasks"`
	}
	out := mustCLI(t, base, "task", "ls", "-s", session, "--json")
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatalf("task ls --json: %v\n%s", err, out)
	}
	rows := map[string]taskFanRow{}
	for _, r := range l.Tasks {
		rows[r.ID] = r
	}
	return rows
}

// stageLine is how tuios task get prints a stage.
func stageLine(stage string) string { return "stage   " + stage + "\n" }

// stayed reads the card again after the moves before it had time to land,
// and fails when it is not in stage.
func stayed(t *testing.T, base, session, id, stage, why string) string {
	t.Helper()
	time.Sleep(time.Second)
	got := mustCLI(t, base, "task", "get", "-s", session, id)
	if !strings.Contains(got, stageLine(stage)) {
		t.Fatalf("ASSERTION: %s left %s, though %s:\n%s", id, stage, why, got)
	}
	return got
}

// TestTaskFollowsAPlanHoldAndItsAgent: a pane holds three cards. When its
// agent's plan is held for the person, the card in doing moves to plan, the
// card set to review by hand stays in review, and the blocked card stays in
// doing. When the agent then works, which it does once the plan is let
// through, the planned card goes back to doing. Each move's history entry is
// by tuios and names why.
//
// How it could pass wrongly: a card claimed in doing that never moved would
// read doing at the end too, so the card must be seen in plan first, with the
// plan's reason in its history.
//
// Negative controls: with the plan hold not moving cards (the autoStage call
// in verbRequestApproval), t1 never reaches plan. With stagedTaskEvents
// returning nothing, t1 stays in plan after the agent works.
func TestTaskFollowsAPlanHoldAndItsAgent(t *testing.T) {
	base, _ := approvalsClient(t)
	const sess = "e2e-agent"
	pane := windowIDs(t, base, sess)[0]
	mustCLI(t, base, "task", "add", "-s", sess, "plan the retry")
	mustCLI(t, base, "task", "add", "-s", sess, "--stage", "review", "already in review")
	mustCLI(t, base, "task", "add", "-s", sess, "waiting on a key")
	for _, id := range []string{"t1", "t2", "t3"} {
		mustCLI(t, base, "task", "claim", "-s", sess, id, "-w", pane)
	}
	mustCLI(t, base, "task", "block", "-s", sess, "t3", "the staging key")

	startHeldHook(t, base, `{"hook_event_name":"PermissionRequest","session_id":"e2e-plan","permission_mode":"plan","tool_name":"ExitPlanMode","tool_input":{"plan":"# Retry\n1. Add backoff.","planFilePath":"/tmp/plan.md"}}`)
	var report strings.Builder
	planned := waitForTaskText(t, base, sess, "t1", stageLine("plan"), "staged    by tuios: plan held for approval")
	fmt.Fprintf(&report, "t1 with the plan held:\n%s\n\n", planned)
	fmt.Fprintf(&report, "t2 with the plan held:\n%s\n\n", stayed(t, base, sess, "t2", "review", "it was put in review by hand"))
	fmt.Fprintf(&report, "t3 with the plan held:\n%s\n\n", stayed(t, base, sess, "t3", "doing", "it is blocked"))

	mustCLI(t, base, "set-agent-state", "-s", sess, "-w", pane, "working", "--harness", "claude-code")
	fmt.Fprintf(&report, "t1 with the agent working:\n%s\n", waitForTaskText(t, base, sess, "t1", stageLine("doing"), "staged    by tuios: agent working"))
	saveText(t, "tasks-auto-plan", report.String())
}

// TestTaskFollowsItsFan: fan --task stamps a card of the conductor's session
// with the fan, moves it to doing, and leaves it unheld, and claim-task next
// takes the card after it instead. A fan check that fails in both attempts
// moves the card to verify and back to doing. A check that passes in one
// leaves it in verify.
//
// How it could pass wrongly: the stamp could be read from a card added inside
// a fan session, so the card lives in a session of no fan; next could skip it
// for being in doing, so the card after it is in todo and would be taken
// first only by a claim that skips the stamped card.
//
// Negative controls: with nextFreeTask not skipping a card a live fan works
// on, next takes t1. With noteFanVerifyEnded doing nothing, t1 stays in
// verify after the failed check.
func TestTaskFollowsItsFan(t *testing.T) {
	base, repo := fanFixture(t)
	const conductor = "conductor"
	mustCLI(t, base, "new", conductor, "--detach")
	waitForSessionInfo(t, base, conductor)
	mustCLI(t, base, "task", "add", "-s", conductor, "add a retry")
	mustCLI(t, base, "task", "add", "-s", conductor, "add a timeout")

	out := mustCLI(t, base, "fan", "2", "--agent", "claude", "--repo", repo, "--name", "try/task", "--task", "t1", "--task-session", conductor, "Add a retry.")
	if !strings.Contains(out, "Task t1 follows the fan.") {
		t.Fatalf("ASSERTION: fan --task did not say it stamped t1:\n%s", out)
	}
	rows := taskFanRows(t, base, conductor)
	if r := rows["t1"]; r.Fan == nil || r.Fan.Fan != "try/task" || r.Stage != "doing" || r.Holder != nil {
		t.Fatalf("ASSERTION: t1 after fan --task = %+v, want doing in fan try/task with no holder", r)
	}
	worker := strings.TrimSpace(mustCLI(t, base, "new-window", "-s", conductor, "worker", "--no-focus", "--print-id"))
	if got := strings.TrimSpace(mustCLI(t, base, "task", "claim", "-s", conductor, "--next", "-w", worker)); !strings.HasPrefix(got, "t2") {
		t.Fatalf("ASSERTION: claim --next took %q, want t2: t1 is the fan's", got)
	}

	var report strings.Builder
	if out, err := tuiosCLI(t, base, "fan", "verify", "repo-try-task", "--", "false"); err == nil {
		t.Fatalf("fan verify exited 0 with a check that fails:\n%s", out)
	}
	failed := waitForTaskText(t, base, conductor, "t1", stageLine("doing"), "by tuios: fan check started", "by tuios: fan check failed in every attempt")
	fmt.Fprintf(&report, "t1 after a check that failed everywhere:\n%s\n\n", failed)

	if out, err := tuiosCLI(t, base, "fan", "verify", "repo-try-task", "--", "test", "-f", "done.txt"); err == nil {
		t.Fatalf("fan verify exited 0 before any attempt passed:\n%s", out)
	}
	var winner string
	for _, r := range worktreeRows(t, base, "--group", "try/task") {
		if r["session"] == "repo-try-task-2" {
			winner = r["path"].(string)
		}
	}
	if err := os.WriteFile(filepath.Join(winner, "done.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForTaskText(t, base, conductor, "t1", stageLine("doing"))
	out, _ = tuiosCLI(t, base, "fan", "verify", "repo-try-task", "--", "test", "-f", "done.txt")
	fmt.Fprintf(&report, "fan verify with one attempt passing:\n%s\n\n", out)
	fmt.Fprintf(&report, "t1 after a check that passed in one attempt:\n%s\n", stayed(t, base, conductor, "t1", "verify", "one attempt passed"))
	saveText(t, "tasks-auto-fan", report.String())
}

// TestTaskFollowsItsPullRequest: a card held by a pane of a worktree session
// moves to review when the branch's pull request is found open, to shipped
// when the poll reads it merged, and stays there. Put back in review by hand,
// it goes to doing when the pull request reads closed without a merge.
//
// How it could pass wrongly: the poll could never run, and shipped come from
// another path, so the merge is written only after the card is in review and
// read only by the poll; the history must name the pull request.
//
// Negative control: with notePR not moving cards, t1 stays in doing after the
// pull request is found.
func TestTaskFollowsItsPullRequest(t *testing.T) {
	f := newShipFixture(t)
	base := f.base
	pane := strings.TrimSpace(mustCLI(t, base, "new-window", "-s", f.session, "worker", "--no-focus", "--print-id"))
	mustCLI(t, base, "task", "add", "-s", f.session, "ship the note")
	mustCLI(t, base, "task", "claim", "-s", f.session, "t1", "-w", pane)

	pr := func(state string) {
		t.Helper()
		body := `{"number":7,"url":"https://github.example/o/r/pull/7","state":"` + state + `","statusCheckRollup":[]}`
		if err := os.WriteFile(filepath.Join(f.fakes, "pr.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var report strings.Builder
	pr("OPEN")
	mustCLI(t, base, "ship", "status", "-s", f.session, "--refresh")
	fmt.Fprintf(&report, "t1 with the pull request open:\n%s\n\n", waitForTaskText(t, base, f.session, "t1", stageLine("review"), "by tuios: pull request #7 opened"))

	// The poll runs while a client is attached.
	term := startIn(t, base, startOpts{args: []string{"attach", f.session}})
	if err := term.WaitFor(func(s tuitest.Screen) bool { return countWindows(s) >= 1 }, bootTimeout); err != nil {
		t.Fatalf("client never attached: %v\n%s", err, term.Snapshot())
	}
	pr("MERGED")
	fmt.Fprintf(&report, "t1 once the poll read it merged:\n%s\n\n", waitForTaskText(t, base, f.session, "t1", stageLine("shipped"), "by tuios: pull request #7 merged"))

	mustCLI(t, base, "task", "stage", "-s", f.session, "t1", "review")
	pr("CLOSED")
	mustCLI(t, base, "ship", "status", "-s", f.session, "--refresh")
	fmt.Fprintf(&report, "t1 with the pull request closed:\n%s\n", waitForTaskText(t, base, f.session, "t1", stageLine("doing"), "by tuios: pull request #7 closed without a merge"))
	saveText(t, "tasks-auto-pr", report.String())
}

// TestTaskAutoStageCanBeTurnedOff: with [tasks] auto_stage = false, a card in
// plan stays there when its holder's agent works, and start-agent --task
// still claims the card for the pane it opens.
//
// How it could pass wrongly: the agent state could never reach the daemon,
// so the pane's state is read back as working before the card is checked.
//
// Negative control: with the daemon not reading [tasks] auto_stage
// (DaemonConfigFromUser), t1 moves to doing.
func TestTaskAutoStageCanBeTurnedOff(t *testing.T) {
	base := t.TempDir()
	killDaemon(t, base)
	dir := filepath.Join(xdgDir(base, "XDG_CONFIG_HOME"), "tuios")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[tasks]\nauto_stage = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const sess = "e2e-tasks-off"
	mustCLI(t, base, "new", sess, "--detach")
	waitForSessionInfo(t, base, sess)
	pane := windowIDs(t, base, sess)[0]
	mustCLI(t, base, "task", "add", "-s", sess, "plan by hand")
	mustCLI(t, base, "task", "claim", "-s", sess, "t1", "-w", pane)
	mustCLI(t, base, "task", "stage", "-s", sess, "t1", "plan")
	mustCLI(t, base, "set-agent-state", "-s", sess, "-w", pane, "working", "--harness", "claude-code")
	if got := agentWindows(t, base, "-s", sess, "--select", "state:working"); len(got) != 1 || got[0] != pane {
		t.Fatalf("the pane never read working: %v", got)
	}
	var report strings.Builder
	fmt.Fprintf(&report, "t1 with auto_stage off and its agent working:\n%s\n\n", stayed(t, base, sess, "t1", "plan", "auto_stage is off"))

	mustCLI(t, base, "task", "add", "-s", sess, "for a new agent")
	out, _ := tuiosCLI(t, base, "start-agent", "sh", "-s", sess, "--name", "t2-worker", "--task", "t2", "--ready-timeout", "500")
	fmt.Fprintf(&report, "start-agent --task t2:\n%s\n", out)
	if !strings.Contains(out, "holds task t2") {
		t.Fatalf("ASSERTION: start-agent --task did not say the pane holds t2:\n%s", out)
	}
	rows := taskFanRows(t, base, sess)
	if r := rows["t2"]; r.Holder == nil || r.Holder.Window != windowIDByName(t, base, sess, "t2-worker") || r.Stage != "doing" {
		t.Fatalf("ASSERTION: t2 after start-agent --task = %+v, want doing, held by t2-worker", r)
	}
	saveText(t, "tasks-auto-off", report.String())
}
