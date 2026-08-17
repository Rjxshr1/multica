package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	workflowruntime "multica-expanded-eval/runtime"
)

func TestGuardAllowsSlowCommandWithProgress(t *testing.T) {
	workspace := t.TempDir()
	cmd := exec.Command("bash", "-lc", "echo one; sleep .3; echo two; sleep .3; echo three")
	observed := runObservedCommand(cmd, observedCommandOptions{
		Guard: true, HardTimeout: 2 * time.Second,
		FirstProgressTimeout: 500 * time.Millisecond, IdleTimeout: 500 * time.Millisecond,
		Workspace: workspace, SessionPath: filepath.Join(workspace, "session.jsonl"),
	})
	if observed.Err != nil || observed.Termination != "completed" {
		t.Fatalf("progressing command was terminated: %s %v", observed.Termination, observed.Err)
	}
	if observed.FirstProgressAt.IsZero() {
		t.Fatal("first progress was not recorded")
	}
}

func TestNoProgressFailureRoutesToBoundedRetry(t *testing.T) {
	if got := classifyExecutionFailure(errors.New("injected stall (no_first_progress): signal: killed")); got != workflowruntime.FailureNoProgress {
		t.Fatalf("classification=%s", got)
	}
	store := workflowruntime.NewMemoryEventStore()
	engine := workflowruntime.NewEngine(store)
	if err := engine.Create(workflowruntime.WorkflowSpec{ID: "run", Nodes: []workflowruntime.NodeSpec{{ID: "node", MaxAttempts: 2}}}, "create"); err != nil {
		t.Fatal(err)
	}
	lease, err := engine.Claim("run", "node", "claim-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.FailAttempt(lease, workflowruntime.FailureNoProgress, "fail-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Claim("run", "node", "claim-2"); err != nil {
		t.Fatalf("second claim rejected: %v", err)
	}
}

func TestGuardStopsOnlyAfterNoProgressBudget(t *testing.T) {
	workspace := t.TempDir()
	cmd := exec.Command("bash", "-lc", "sleep 2")
	observed := runObservedCommand(cmd, observedCommandOptions{
		Guard: true, HardTimeout: 3 * time.Second,
		FirstProgressTimeout: 200 * time.Millisecond, IdleTimeout: time.Second,
		Workspace: workspace, SessionPath: filepath.Join(workspace, "session.jsonl"),
	})
	if observed.Termination != "no_first_progress" {
		t.Fatalf("termination=%s, want no_first_progress", observed.Termination)
	}
	if observed.FinishedAt.Sub(observed.StartedAt) < 200*time.Millisecond {
		t.Fatal("guard fired before its configured evidence budget")
	}
}

func TestExperimentArmsAreCumulative(t *testing.T) {
	profiles, err := parseArms("original,runtime,retry,guard,reuse")
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 5 || profiles[0].Runtime || !profiles[1].Runtime || !profiles[2].ClassifiedRetry || !profiles[3].ProgressGuard || !profiles[4].SessionReuse {
		t.Fatalf("unexpected profiles: %#v", profiles)
	}
}

func TestInjectedStallIsRecoveredByProgressGuard(t *testing.T) {
	workspace := t.TempDir()
	e := &evaluator{
		profile: armCatalog["guard"], hardTimeout: 3 * time.Second,
		firstProgressTimeout: 100 * time.Millisecond, idleTimeout: time.Second,
		faultStall: time.Second, requestsByWorkspace: map[string][]requestMetric{},
	}
	_, err := e.runInjectedStall(workspace, "fix-a1")
	if err == nil {
		t.Fatal("injected stall must fail its first attempt")
	}
	requests := e.requestsByWorkspace[workspace]
	if len(requests) != 1 || requests[0].Termination != "no_first_progress" {
		t.Fatalf("requests=%#v", requests)
	}
}
