package service

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestWorkflowRuntimeOrphanedTaskFailureReentersNode reproduces the production
// sweeper path: SQL marks a stale task failed before HandleFailedTasks runs.
// The Workflow attempt must not remain running after its bound task is dead.
func TestWorkflowRuntimeOrphanedTaskFailureReentersNode(t *testing.T) {
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
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Workflow Orphan', $1) RETURNING id`, fmt.Sprintf("workflow-orphan-%d@example.test", suffix)).Scan(&userIDText); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ('Workflow Orphan', $1, '', 'WFO') RETURNING id`, fmt.Sprintf("workflow-orphan-%d", suffix)).Scan(&workspaceIDText); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, workspaceIDText, userIDText); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, owner_id) VALUES ($1, 'Workflow Orphan', 'cloud', 'workflow_e2e', 'offline', 'test', '{}'::jsonb, now() - interval '1 hour', 'private', $2) RETURNING id`, workspaceIDText, userIDText).Scan(&runtimeIDText); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id) VALUES ($1, 'Workflow Orphan', '', 'cloud', '{}'::jsonb, $2, 'private', 1, $3) RETURNING id`, workspaceIDText, runtimeIDText, userIDText).Scan(&agentIDText); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position) VALUES ($1, 'Workflow Orphan', 'in_progress', 'none', $2, 'member', 1, 0) RETURNING id`, workspaceIDText, userIDText).Scan(&issueIDText); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, started_at, max_attempts) VALUES ($1, $2, $3, 'running', now() - interval '1 hour', 3) RETURNING id`, agentIDText, runtimeIDText, issueIDText).Scan(&taskIDText); err != nil {
		t.Fatal(err)
	}

	queries := db.New(pool)
	workflow := NewWorkflowRuntimeService(queries, pool)
	workspaceID := util.MustParseUUID(workspaceIDText)
	created, err := workflow.CreateRun(ctx, CreateWorkflowRunInput{WorkspaceID: workspaceID, IdempotencyKey: fmt.Sprintf("orphan-%d", suffix), Plan: WorkflowPlan{DefinitionKey: "orphan", DefinitionVersion: "1", Nodes: []WorkflowNodeSpec{{Key: "execute", RetryPolicy: map[string]any{"max_attempts": 2, "retryable_failure_codes": []string{"runtime_offline"}}}}}, Actor: WorkflowActor{Type: "member", ID: util.MustParseUUID(userIDText)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = queries.DeleteWorkspaceWorkflowRuntime(context.Background(), workspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, workspaceIDText)
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userIDText)
	})
	lease, err := workflow.ClaimNode(ctx, workspaceID, created.Run.ID, "execute", "claim-orphan", WorkflowActor{Type: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.BindTask(ctx, workspaceID, lease, util.MustParseUUID(taskIDText)); err != nil {
		t.Fatal(err)
	}
	failedRows, err := pool.Query(ctx, `UPDATE agent_task_queue SET status='failed', completed_at=now(), error='runtime offline', failure_reason='runtime_offline' WHERE id=$1 RETURNING id`, taskIDText)
	if err != nil {
		t.Fatal(err)
	}
	failedRows.Close()
	failedTask, err := queries.GetAgentTask(ctx, util.MustParseUUID(taskIDText))
	if err != nil {
		t.Fatal(err)
	}

	taskService := NewTaskService(queries, pool, nil, events.New())
	taskService.WorkflowRuntime = workflow
	taskService.HandleFailedTasks(ctx, []db.AgentTaskQueue{failedTask})
	snapshot, err := workflow.Snapshot(ctx, workspaceID, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Nodes[0].Status != "ready" {
		t.Fatalf("orphaned task left workflow node %s, want ready for Workflow-owned retry", snapshot.Nodes[0].Status)
	}
	if snapshot.Attempts[0].Status != "execution_failed" {
		t.Fatalf("attempt status = %s, want execution_failed", snapshot.Attempts[0].Status)
	}

	// A duplicate sweeper delivery must be a no-op: the task remains owned by
	// the Workflow Runtime and no second state transition/event is appended.
	eventCount := len(snapshot.Events)
	taskService.HandleFailedTasks(ctx, []db.AgentTaskQueue{failedTask})
	duplicate, err := workflow.Snapshot(ctx, workspaceID, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Nodes[0].Status != "ready" || duplicate.Attempts[0].Status != "execution_failed" {
		t.Fatalf("duplicate failure changed settled state: node=%s attempt=%s", duplicate.Nodes[0].Status, duplicate.Attempts[0].Status)
	}
	if len(duplicate.Events) != eventCount {
		t.Fatalf("duplicate failure appended events: got %d, want %d", len(duplicate.Events), eventCount)
	}
}
