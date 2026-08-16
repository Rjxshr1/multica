package workflowruntime

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestVerificationFailureLoopsAndReleasesDependency(t *testing.T) {
	store := NewMemoryEventStore()
	engine := NewEngine(store)
	spec := WorkflowSpec{ID: "run-1", Nodes: []NodeSpec{
		{ID: "implement", MaxAttempts: 2},
		{ID: "deliver", DependsOn: []string{"implement"}, MaxAttempts: 1},
	}}
	if err := engine.Create(spec, "create"); err != nil {
		t.Fatal(err)
	}

	first := mustClaim(t, engine, "run-1", "implement", "claim-1")
	mustReport(t, engine, first, "result-1")
	if err := engine.Verify(first, false, FailureCodeDefect, "verify-1"); err != nil {
		t.Fatal(err)
	}

	run, err := engine.Snapshot("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := run.Nodes["implement"].State; got != NodeReady {
		t.Fatalf("implement state = %s, want ready for retry", got)
	}
	if got := run.Nodes["deliver"].State; got != NodeBlocked {
		t.Fatalf("deliver state = %s, want blocked", got)
	}

	second := mustClaim(t, engine, "run-1", "implement", "claim-2")
	mustReport(t, engine, second, "result-2")
	if err := engine.Verify(second, true, "", "verify-2"); err != nil {
		t.Fatal(err)
	}

	run, err = engine.Snapshot("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := run.Nodes["deliver"].State; got != NodeReady {
		t.Fatalf("deliver state = %s, want ready", got)
	}

	deliver := mustClaim(t, engine, "run-1", "deliver", "claim-deliver")
	mustReport(t, engine, deliver, "result-deliver")
	if err := engine.Verify(deliver, true, "", "verify-deliver"); err != nil {
		t.Fatal(err)
	}
	run, err = engine.Snapshot("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != WorkflowSucceeded {
		t.Fatalf("workflow state = %s, want succeeded", run.State)
	}
}

func TestAddNodeBeforeWaitingTarget(t *testing.T) {
	store := NewMemoryEventStore()
	engine := NewEngine(store)
	if err := engine.Create(WorkflowSpec{ID: "run", Nodes: []NodeSpec{
		{ID: "build", MaxAttempts: 1},
		{ID: "integration", DependsOn: []string{"build"}, MaxAttempts: 2},
	}}, "create"); err != nil {
		t.Fatal(err)
	}

	build, err := engine.Claim("run", "build", "claim-build")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ReportTaskSucceeded(build, "done-build"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Verify(build, true, "", "verify-build"); err != nil {
		t.Fatal(err)
	}
	integration, err := engine.Claim("run", "integration", "claim-integration")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ReportTaskSucceeded(integration, "done-integration"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Verify(integration, false, FailureDependencyGap, "reject-integration"); err != nil {
		t.Fatal(err)
	}

	if err := engine.AddNodeBefore("run", "integration", NodeSpec{ID: "review", MaxAttempts: 1}, "insert-review"); err != nil {
		t.Fatal(err)
	}
	if err := engine.AddNodeBefore("run", "integration", NodeSpec{ID: "review", MaxAttempts: 1}, "insert-review"); err != nil {
		t.Fatalf("idempotent insert: %v", err)
	}
	run, err := engine.Snapshot("run")
	if err != nil {
		t.Fatal(err)
	}
	if run.Nodes["review"].State != NodeReady {
		t.Fatalf("review state = %s, want ready", run.Nodes["review"].State)
	}
	if run.Nodes["integration"].State != NodeBlocked {
		t.Fatalf("integration state = %s, want blocked", run.Nodes["integration"].State)
	}
	if got := run.Nodes["integration"].Spec.DependsOn; len(got) != 1 || got[0] != "review" {
		t.Fatalf("integration dependencies = %v, want [review]", got)
	}

	review, err := engine.Claim("run", "review", "claim-review")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ReportTaskSucceeded(review, "done-review"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Verify(review, true, "", "verify-review"); err != nil {
		t.Fatal(err)
	}
	run, err = engine.Snapshot("run")
	if err != nil {
		t.Fatal(err)
	}
	if run.Nodes["integration"].State != NodeReady {
		t.Fatalf("integration state after review = %s, want ready", run.Nodes["integration"].State)
	}
}

func TestAddNodeBeforeRejectsRunningTarget(t *testing.T) {
	engine := NewEngine(NewMemoryEventStore())
	if err := engine.Create(WorkflowSpec{ID: "run", Nodes: []NodeSpec{{ID: "target", MaxAttempts: 1}}}, "create"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Claim("run", "target", "claim"); err != nil {
		t.Fatal(err)
	}
	if err := engine.AddNodeBefore("run", "target", NodeSpec{ID: "review"}, "insert"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("error = %v, want ErrInvalidState", err)
	}
}

func TestDuplicateCommandIsIdempotent(t *testing.T) {
	store := NewMemoryEventStore()
	engine := NewEngine(store)
	if err := engine.Create(singleNodeSpec("run-duplicate", 1), "create"); err != nil {
		t.Fatal(err)
	}
	lease := mustClaim(t, engine, "run-duplicate", "work", "claim")
	mustReport(t, engine, lease, "same-result-command")
	mustReport(t, engine, lease, "same-result-command")

	events := engine.Events("run-duplicate")
	accepted := 0
	for _, event := range events {
		if event.Type == EventTaskResultAccepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("task result events = %d, want 1", accepted)
	}
}

func TestRestartReplaysDurableState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow-events.json")
	store, err := OpenFileEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeRestart := NewEngine(store)
	if err := beforeRestart.Create(singleNodeSpec("run-restart", 1), "create"); err != nil {
		t.Fatal(err)
	}
	lease := mustClaim(t, beforeRestart, "run-restart", "work", "claim")
	mustReport(t, beforeRestart, lease, "result")

	reopened, err := OpenFileEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart := NewEngine(reopened)
	if err := afterRestart.Verify(lease, true, "", "verify"); err != nil {
		t.Fatal(err)
	}
	run, err := afterRestart.Snapshot("run-restart")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != WorkflowSucceeded {
		t.Fatalf("workflow state = %s, want succeeded", run.State)
	}
}

func TestStaleFenceCannotOverwriteNewAttempt(t *testing.T) {
	store := NewMemoryEventStore()
	engine := NewEngine(store)
	if err := engine.Create(singleNodeSpec("run-fence", 2), "create"); err != nil {
		t.Fatal(err)
	}
	first := mustClaim(t, engine, "run-fence", "work", "claim-1")
	mustReport(t, engine, first, "result-1")
	if err := engine.Verify(first, false, FailureCodeDefect, "verify-1"); err != nil {
		t.Fatal(err)
	}
	second := mustClaim(t, engine, "run-fence", "work", "claim-2")

	if err := engine.ReportTaskSucceeded(first, "late-result"); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("late result error = %v, want ErrStaleFence", err)
	}
	mustReport(t, engine, second, "result-2")
	if err := engine.Verify(second, true, "", "verify-2"); err != nil {
		t.Fatal(err)
	}
}

func TestNoProgressTimeoutRetriesWithNewFence(t *testing.T) {
	store := NewMemoryEventStore()
	engine := NewEngine(store)
	if err := engine.Create(singleNodeSpec("run-timeout", 2), "create"); err != nil {
		t.Fatal(err)
	}
	first := mustClaim(t, engine, "run-timeout", "work", "claim-1")
	if err := engine.FailAttempt(first, FailureNoProgress, "timeout-1"); err != nil {
		t.Fatal(err)
	}

	run, err := engine.Snapshot("run-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if got := run.Nodes["work"].State; got != NodeReady {
		t.Fatalf("node state = %s, want ready for bounded retry", got)
	}
	if got := run.Nodes["work"].Attempts[0].FailureClass; got != FailureNoProgress {
		t.Fatalf("failure class = %s, want %s", got, FailureNoProgress)
	}

	second := mustClaim(t, engine, "run-timeout", "work", "claim-2")
	if second.Fence <= first.Fence {
		t.Fatalf("second fence = %d, want greater than %d", second.Fence, first.Fence)
	}
	if err := engine.ReportProgress(first, "late-progress"); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("late progress error = %v, want ErrStaleFence", err)
	}
}

func TestProgressIsDurableAndIdempotent(t *testing.T) {
	store := NewMemoryEventStore()
	engine := NewEngine(store)
	if err := engine.Create(singleNodeSpec("run-progress", 1), "create"); err != nil {
		t.Fatal(err)
	}
	lease := mustClaim(t, engine, "run-progress", "work", "claim")
	if err := engine.ReportProgress(lease, "progress-1"); err != nil {
		t.Fatal(err)
	}
	if err := engine.ReportProgress(lease, "progress-1"); err != nil {
		t.Fatalf("idempotent progress: %v", err)
	}

	run, err := engine.Snapshot("run-progress")
	if err != nil {
		t.Fatal(err)
	}
	attempt := run.Nodes["work"].Attempts[0]
	if attempt.ProgressCount != 1 || attempt.LastProgressAt == nil {
		t.Fatalf("progress = count %d at %v, want one durable progress event", attempt.ProgressCount, attempt.LastProgressAt)
	}
}

func TestExecutionBudgetIsDurableAndValidated(t *testing.T) {
	store := NewMemoryEventStore()
	engine := NewEngine(store)
	spec := singleNodeSpec("run-budget", 1)
	spec.Nodes[0].ExecutionBudget = &ExecutionBudgetSpec{
		FirstProgressTimeoutSeconds: 600,
		IdleTimeoutSeconds:          300,
		HardTimeoutSeconds:          3600,
	}
	if err := engine.Create(spec, "create-budget"); err != nil {
		t.Fatal(err)
	}
	run, err := engine.Snapshot("run-budget")
	if err != nil {
		t.Fatal(err)
	}
	if got := run.Nodes["work"].Spec.ExecutionBudget.HardTimeoutSeconds; got != 3600 {
		t.Fatalf("hard timeout = %d, want 3600", got)
	}

	invalid := singleNodeSpec("run-invalid-budget", 1)
	invalid.Nodes[0].ExecutionBudget = &ExecutionBudgetSpec{IdleTimeoutSeconds: -1}
	if err := engine.Create(invalid, "create-invalid"); err == nil {
		t.Fatal("negative execution budget should be rejected")
	}
}

func TestVersionConflictRejectsConcurrentWriter(t *testing.T) {
	store := NewMemoryEventStore()
	engine := NewEngine(store)
	if err := engine.Create(singleNodeSpec("run-cas", 1), "create"); err != nil {
		t.Fatal(err)
	}
	run, err := engine.Snapshot("run-cas")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Claim("run-cas", "work", "claim"); err != nil {
		t.Fatal(err)
	}
	_, err = store.Append("run-cas", run.Version, "stale-writer", Event{Type: EventTaskResultAccepted})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale append error = %v, want ErrVersionConflict", err)
	}
}

func TestRejectsCyclicWorkflow(t *testing.T) {
	engine := NewEngine(NewMemoryEventStore())
	err := engine.Create(WorkflowSpec{ID: "cycle", Nodes: []NodeSpec{
		{ID: "a", DependsOn: []string{"b"}},
		{ID: "b", DependsOn: []string{"a"}},
	}}, "create")
	if err == nil {
		t.Fatal("expected cyclic workflow to be rejected")
	}
}

func singleNodeSpec(id string, maxAttempts int) WorkflowSpec {
	return WorkflowSpec{ID: id, Nodes: []NodeSpec{{ID: "work", MaxAttempts: maxAttempts}}}
}

func mustClaim(t *testing.T, engine *Engine, runID, nodeID, commandID string) Lease {
	t.Helper()
	lease, err := engine.Claim(runID, nodeID, commandID)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func mustReport(t *testing.T, engine *Engine, lease Lease, commandID string) {
	t.Helper()
	if err := engine.ReportTaskSucceeded(lease, commandID); err != nil {
		t.Fatal(err)
	}
}
