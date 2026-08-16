package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestWorkflowRuntimeDatabaseLoop(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for the workflow database test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var tablesReady bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('workflow_run') IS NOT NULL`).Scan(&tablesReady); err != nil || !tablesReady {
		t.Skip("workflow migrations are not applied")
	}

	queries := db.New(pool)
	runtime := NewWorkflowRuntimeService(queries, pool)
	workspaceID := newPGUUID()
	executorID := newPGUUID()
	verifierID := newPGUUID()
	created, err := runtime.CreateRun(ctx, CreateWorkflowRunInput{
		WorkspaceID: workspaceID, IdempotencyKey: "db-loop-create",
		Plan: WorkflowPlan{DefinitionKey: "acceptance", DefinitionVersion: "1", Nodes: []WorkflowNodeSpec{
			{Key: "implement", ExecutorSpec: map[string]any{"agent_id": util.UUIDToString(executorID)}, RetryPolicy: map[string]any{"max_attempts": 2, "retryable_failure_codes": []string{"code_defect"}}, VerificationPolicy: map[string]any{"independent": true}},
			{Key: "deliver", DependsOn: []string{"implement"}},
		}}, Actor: WorkflowActor{Type: "system"},
	})
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID
	t.Cleanup(func() { _ = queries.DeleteWorkspaceWorkflowRuntime(context.Background(), workspaceID) })

	first, err := runtime.ClaimNode(ctx, workspaceID, runID, "implement", "claim-implement-1", WorkflowActor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	firstTask := newPGUUID()
	bindSyntheticAttempt(t, ctx, queries, workspaceID, first, firstTask)
	settleSuccess(t, ctx, pool, runtime, firstTask, `{"commit":"bad"}`)
	if _, err := runtime.Verify(ctx, VerificationInput{WorkspaceID: workspaceID, RunID: runID, AttemptID: first.AttemptID, FenceToken: first.FenceToken, Passed: false, FailureCode: "code_defect", VerifierKind: "human", VerifierID: verifierID, IdempotencyKey: "verify-implement-1"}); err != nil {
		t.Fatal(err)
	}

	second, err := runtime.ClaimNode(ctx, workspaceID, runID, "implement", "claim-implement-2", WorkflowActor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if second.AttemptNo != 2 || second.FenceToken <= first.FenceToken {
		t.Fatalf("second lease = %+v, want attempt 2 with a newer fence", second)
	}
	secondTask := newPGUUID()
	bindSyntheticAttempt(t, ctx, queries, workspaceID, second, secondTask)
	settleSuccess(t, ctx, pool, runtime, secondTask, `{"commit":"fixed","tests":"passed"}`)
	if _, err := runtime.Verify(ctx, VerificationInput{WorkspaceID: workspaceID, RunID: runID, AttemptID: second.AttemptID, FenceToken: second.FenceToken, Passed: true, VerifierKind: "human", VerifierID: verifierID, IdempotencyKey: "verify-implement-2"}); err != nil {
		t.Fatal(err)
	}

	deliver, err := runtime.ClaimNode(ctx, workspaceID, runID, "deliver", "claim-deliver", WorkflowActor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	deliverTask := newPGUUID()
	bindSyntheticAttempt(t, ctx, queries, workspaceID, deliver, deliverTask)
	settleSuccess(t, ctx, pool, runtime, deliverTask, `{"delivered":true}`)
	final, err := runtime.Verify(ctx, VerificationInput{WorkspaceID: workspaceID, RunID: runID, AttemptID: deliver.AttemptID, FenceToken: deliver.FenceToken, Passed: true, VerifierKind: "human", VerifierID: verifierID, IdempotencyKey: "verify-deliver"})
	if err != nil {
		t.Fatal(err)
	}
	if final.Run.Status != "succeeded" {
		t.Fatalf("run status = %s, want succeeded", final.Run.Status)
	}
	if len(final.Attempts) != 3 || len(final.Verifications) != 3 {
		t.Fatalf("attempts/verifications = %d/%d, want 3/3", len(final.Attempts), len(final.Verifications))
	}
	if len(final.Events) != 12 {
		t.Fatalf("events = %d, want 12", len(final.Events))
	}
	var unpublished int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_outbox WHERE run_id = $1 AND status = 'pending'`, util.UUIDToString(runID)).Scan(&unpublished); err != nil {
		t.Fatal(err)
	}
	if unpublished != len(final.Events) {
		t.Fatalf("outbox rows = %d, want %d", unpublished, len(final.Events))
	}
	bus := events.New()
	delivered := 0
	bus.Subscribe(workflowOutboxEventType, func(event events.Event) {
		payload, _ := event.Payload.(map[string]any)
		if payload["run_id"] == util.UUIDToString(runID) {
			delivered++
		}
	})
	worker := NewWorkflowOutboxWorker(queries, bus)
	processed, err := worker.ProcessBatch(ctx, 64)
	if err != nil {
		t.Fatal(err)
	}
	if processed < len(final.Events) || delivered != len(final.Events) {
		t.Fatalf("outbox processed/this-run-delivered = %d/%d, want >=%d/%d", processed, delivered, len(final.Events), len(final.Events))
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_outbox WHERE run_id = $1 AND status = 'published'`, util.UUIDToString(runID)).Scan(&unpublished); err != nil {
		t.Fatal(err)
	}
	if unpublished != len(final.Events) {
		t.Fatalf("published outbox rows = %d, want %d", unpublished, len(final.Events))
	}
}

func TestWorkflowRuntimeDatabaseInsertNodeBefore(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for the workflow database test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var tablesReady bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('workflow_run') IS NOT NULL`).Scan(&tablesReady); err != nil || !tablesReady {
		t.Skip("workflow migrations are not applied")
	}

	queries := db.New(pool)
	runtime := NewWorkflowRuntimeService(queries, pool)
	workspaceID := newPGUUID()
	created, err := runtime.CreateRun(ctx, CreateWorkflowRunInput{
		WorkspaceID: workspaceID, IdempotencyKey: "db-amend-create",
		Plan: WorkflowPlan{DefinitionKey: "amendment", DefinitionVersion: "1", Nodes: []WorkflowNodeSpec{
			{Key: "prepare"},
			{Key: "integration", DependsOn: []string{"prepare"}, RetryPolicy: map[string]any{"max_attempts": 2}},
		}}, Actor: WorkflowActor{Type: "system"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queries.DeleteWorkspaceWorkflowRuntime(context.Background(), workspaceID) })
	input := InsertWorkflowNodeBeforeInput{WorkspaceID: workspaceID, RunID: created.Run.ID, TargetNodeKey: "integration", Node: WorkflowNodeSpec{Key: "review"}, IdempotencyKey: "insert-review", Actor: WorkflowActor{Type: "system"}}
	amended, err := runtime.InsertNodeBefore(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.InsertNodeBefore(ctx, input); err != nil {
		t.Fatalf("idempotent amendment: %v", err)
	}
	if len(amended.Nodes) != 3 || len(amended.Dependencies) != 2 {
		t.Fatalf("nodes/dependencies = %d/%d, want 3/2", len(amended.Nodes), len(amended.Dependencies))
	}
	status := map[string]string{}
	for _, node := range amended.Nodes {
		status[node.NodeKey] = node.Status
	}
	if status["prepare"] != "ready" || status["review"] != "waiting" || status["integration"] != "waiting" {
		t.Fatalf("amended states = %#v", status)
	}
	var plan WorkflowPlan
	if err := json.Unmarshal(amended.Run.PlanSnapshot, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 3 {
		t.Fatalf("plan node count = %d, want 3", len(plan.Nodes))
	}

	complete := func(nodeKey, command string) {
		t.Helper()
		lease, claimErr := runtime.ClaimNode(ctx, workspaceID, created.Run.ID, nodeKey, "claim-"+command, WorkflowActor{Type: "system"})
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		taskID := newPGUUID()
		bindSyntheticAttempt(t, ctx, queries, workspaceID, lease, taskID)
		settleSuccess(t, ctx, pool, runtime, taskID, `{"ok":true}`)
		if _, verifyErr := runtime.Verify(ctx, VerificationInput{WorkspaceID: workspaceID, RunID: created.Run.ID, AttemptID: lease.AttemptID, FenceToken: lease.FenceToken, Passed: true, VerifierKind: "system", IdempotencyKey: "verify-" + command}); verifyErr != nil {
			t.Fatal(verifyErr)
		}
	}
	complete("prepare", "prepare")
	afterPrepare, err := runtime.Snapshot(ctx, workspaceID, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range afterPrepare.Nodes {
		if node.NodeKey == "review" && node.Status != "ready" {
			t.Fatalf("review status = %s, want ready", node.Status)
		}
		if node.NodeKey == "integration" && node.Status != "waiting" {
			t.Fatalf("integration status = %s, want waiting", node.Status)
		}
	}
	complete("review", "review")
	complete("integration", "integration")
	final, err := runtime.Snapshot(ctx, workspaceID, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Run.Status != "succeeded" {
		t.Fatalf("run status = %s, want succeeded", final.Run.Status)
	}
}

func TestWorkflowTaskServiceCompletionEntersVerification(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for the workflow database test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var tablesReady bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('workflow_run') IS NOT NULL`).Scan(&tablesReady); err != nil || !tablesReady {
		t.Skip("workflow migrations are not applied")
	}

	suffix := time.Now().UnixNano()
	var userIDText, workspaceIDText, runtimeIDText, agentIDText, issueIDText, taskIDText string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Workflow E2E', $1) RETURNING id`, fmt.Sprintf("workflow-%d@example.test", suffix)).Scan(&userIDText); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ('Workflow E2E', $1, '', 'WFE') RETURNING id`, fmt.Sprintf("workflow-%d", suffix)).Scan(&workspaceIDText); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, workspaceIDText, userIDText); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id) VALUES ($1, 'Workflow E2E', 'cloud', 'workflow_e2e', 'online', 'test', '{}'::jsonb, now(), 'private', $2) RETURNING id`, workspaceIDText, userIDText).Scan(&runtimeIDText); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id) VALUES ($1, 'Workflow E2E', '', 'cloud', '{}'::jsonb, $2, 'private', 1, $3) RETURNING id`, workspaceIDText, runtimeIDText, userIDText).Scan(&agentIDText); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position) VALUES ($1, 'Workflow E2E', 'in_progress', 'none', $2, 'member', 1, 0) RETURNING id`, workspaceIDText, userIDText).Scan(&issueIDText); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, started_at, max_attempts) VALUES ($1, $2, $3, 'running', now(), 1) RETURNING id`, agentIDText, runtimeIDText, issueIDText).Scan(&taskIDText); err != nil {
		t.Fatal(err)
	}

	queries := db.New(pool)
	workflow := NewWorkflowRuntimeService(queries, pool)
	workspaceID := util.MustParseUUID(workspaceIDText)
	created, err := workflow.CreateRun(ctx, CreateWorkflowRunInput{WorkspaceID: workspaceID, IdempotencyKey: fmt.Sprintf("task-service-%d", suffix), Plan: WorkflowPlan{DefinitionKey: "task-service", DefinitionVersion: "1", Nodes: []WorkflowNodeSpec{{Key: "execute", ExecutorSpec: map[string]any{"agent_id": agentIDText, "runtime_id": runtimeIDText}, RetryPolicy: map[string]any{"max_attempts": 2}, VerificationPolicy: map[string]any{"independent": true}}}}, Actor: WorkflowActor{Type: "member", ID: util.MustParseUUID(userIDText)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = queries.DeleteWorkspaceWorkflowRuntime(context.Background(), workspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, workspaceIDText)
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userIDText)
	})
	lease, err := workflow.ClaimNode(ctx, workspaceID, created.Run.ID, "execute", fmt.Sprintf("claim-%d", suffix), WorkflowActor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.BindTask(ctx, workspaceID, lease, util.MustParseUUID(taskIDText)); err != nil {
		t.Fatal(err)
	}
	catalogBefore, err := workflow.ContextCatalog(ctx, workspaceID, util.MustParseUUID(taskIDText))
	if err != nil {
		t.Fatal(err)
	}
	if catalogBefore.ContextRevision <= 0 || catalogBefore.SourceDigest == "" || len(catalogBefore.Items) < 2 {
		t.Fatalf("context catalog was not frozen at bind: %+v", catalogBefore)
	}
	results, err := workflow.SearchContext(ctx, workspaceID, util.MustParseUUID(taskIDText), WorkflowContextSearchInput{Query: "execute", Kinds: []string{"task"}, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ReferenceKey != "task/current" {
		t.Fatalf("task context search = %+v, want task/current", results)
	}
	emptyResults, err := workflow.SearchContext(ctx, workspaceID, util.MustParseUUID(taskIDText), WorkflowContextSearchInput{Query: "  ", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyResults) != 1 || emptyResults[0].ReferenceKey != "task/current" {
		t.Fatalf("empty context search = %+v, want first frozen item", emptyResults)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_context_item
		SET search_text = CASE reference_key
			WHEN 'task/current' THEN 'marker_node_1'
			WHEN 'workflow/overview' THEN 'marker_nodeX1'
			ELSE search_text END
		WHERE snapshot_id = $1`, catalogBefore.SnapshotID); err != nil {
		t.Fatal(err)
	}
	literalResults, err := workflow.SearchContext(ctx, workspaceID, util.MustParseUUID(taskIDText), WorkflowContextSearchInput{Query: "marker_node_1", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(literalResults) != 1 || literalResults[0].ReferenceKey != "task/current" {
		t.Fatalf("literal context search = %+v, want only task/current", literalResults)
	}
	frozenTask, err := workflow.GetContextItem(ctx, workspaceID, util.MustParseUUID(taskIDText), "task/current")
	if err != nil {
		t.Fatal(err)
	}
	var frozenTaskContent map[string]any
	if err := json.Unmarshal(frozenTask.Content, &frozenTaskContent); err != nil {
		t.Fatal(err)
	}
	if frozenTaskContent["status"] != "running" {
		t.Fatalf("frozen task context missing bind-time state: %s", frozenTask.Content)
	}

	taskService := NewTaskService(queries, pool, nil, events.New())
	taskService.WorkflowRuntime = workflow
	completed, err := taskService.CompleteTask(ctx, util.MustParseUUID(taskIDText), []byte(`{"output":"implementation complete"}`), "", "", "", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" {
		t.Fatalf("task status = %s, want completed", completed.Status)
	}
	snapshot, err := workflow.Snapshot(ctx, workspaceID, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Nodes[0].Status != "verifying" || snapshot.Attempts[0].Status != "result_submitted" {
		t.Fatalf("workflow state after CompleteTask = node:%s attempt:%s", snapshot.Nodes[0].Status, snapshot.Attempts[0].Status)
	}
	if snapshot.Run.Status != "running" {
		t.Fatalf("task completion incorrectly completed run: %s", snapshot.Run.Status)
	}
	final, err := workflow.Verify(ctx, VerificationInput{WorkspaceID: workspaceID, RunID: created.Run.ID, AttemptID: lease.AttemptID, FenceToken: lease.FenceToken, Passed: true, VerifierKind: "human", VerifierID: util.MustParseUUID(userIDText), IdempotencyKey: fmt.Sprintf("verify-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	if final.Run.Status != "succeeded" {
		t.Fatalf("run status = %s, want succeeded", final.Run.Status)
	}
	catalogAfter, err := workflow.ContextCatalog(ctx, workspaceID, util.MustParseUUID(taskIDText))
	if err != nil {
		t.Fatal(err)
	}
	if catalogAfter.SourceDigest != catalogBefore.SourceDigest || catalogAfter.ContextRevision != catalogBefore.ContextRevision || len(catalogAfter.Items) != len(catalogBefore.Items) {
		t.Fatalf("attempt context mutated after completion: before=%+v after=%+v", catalogBefore, catalogAfter)
	}
}

func settleSuccess(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runtime *WorkflowRuntimeService, taskID pgtype.UUID, payload string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	owned, err := runtime.SettleTaskSuccessTx(ctx, runtime.Queries.WithTx(tx), taskID, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("task was not recognized as workflow-owned")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func bindSyntheticAttempt(t *testing.T, ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, lease WorkflowLease, taskID pgtype.UUID) {
	t.Helper()
	if _, err := queries.BindWorkflowAttemptTask(ctx, db.BindWorkflowAttemptTaskParams{TaskID: taskID, ID: lease.AttemptID, RunID: lease.RunID, WorkspaceID: workspaceID}); err != nil {
		t.Fatal(err)
	}
}
