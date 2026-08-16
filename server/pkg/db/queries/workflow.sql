-- name: LockWorkflowRun :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(run_id)::text, 0));

-- name: GetWorkflowRun :one
SELECT * FROM workflow_run WHERE id = @id AND workspace_id = @workspace_id;

-- name: GetWorkflowRunByIdempotencyKey :one
SELECT * FROM workflow_run
WHERE workspace_id = @workspace_id AND idempotency_key = @idempotency_key;

-- name: CreateWorkflowRun :one
INSERT INTO workflow_run (
    id, workspace_id, status, revision, definition_key,
    definition_version, definition_digest, plan_snapshot, policy_snapshot,
    idempotency_key, created_by_type, started_at
) VALUES (
    sqlc.arg('id'), sqlc.arg('workspace_id'), 'running', 0,
    @definition_key, @definition_version, @definition_digest, @plan_snapshot,
    @policy_snapshot, sqlc.narg('idempotency_key'), @created_by_type, now()
)
RETURNING *;

-- name: IncrementWorkflowRunRevision :one
UPDATE workflow_run
SET revision = revision + 1, updated_at = now()
WHERE id = @id AND workspace_id = @workspace_id
RETURNING revision;

-- name: SetWorkflowRunStatus :one
UPDATE workflow_run
SET status = @status,
    completed_at = CASE WHEN @status IN ('succeeded', 'failed', 'cancelled') THEN now() ELSE completed_at END,
    updated_at = now()
WHERE id = @id AND workspace_id = @workspace_id AND status = @expected_status
RETURNING *;

-- name: CreateWorkflowNode :one
INSERT INTO workflow_node_execution (
    id, workspace_id, run_id, node_key, node_kind, status, executor_spec,
    retry_policy, verification_policy, input_spec, input_digest, ready_at
) VALUES (
    @id, @workspace_id, @run_id, @node_key, @node_kind, @status,
    @executor_spec, @retry_policy, @verification_policy, @input_spec,
    @input_digest, CASE WHEN @status = 'ready' THEN now() ELSE NULL END
)
RETURNING *;

-- name: CreateWorkflowDependency :one
INSERT INTO workflow_node_dependency (
    id, workspace_id, run_id, predecessor_node_id, successor_node_id, condition
) VALUES (
    @id, @workspace_id, @run_id, @predecessor_node_id, @successor_node_id, 'required_success'
)
RETURNING *;

-- name: ListWorkflowNodes :many
SELECT * FROM workflow_node_execution
WHERE run_id = @run_id AND workspace_id = @workspace_id
ORDER BY created_at, node_key;

-- name: ListWorkflowDependencies :many
SELECT * FROM workflow_node_dependency
WHERE run_id = @run_id AND workspace_id = @workspace_id
ORDER BY created_at, id;

-- name: GetWorkflowNodeForUpdate :one
SELECT * FROM workflow_node_execution
WHERE id = @id AND run_id = @run_id AND workspace_id = @workspace_id
FOR UPDATE;

-- name: GetWorkflowNodeByKeyForUpdate :one
SELECT * FROM workflow_node_execution
WHERE node_key = @node_key AND run_id = @run_id AND workspace_id = @workspace_id
FOR UPDATE;

-- name: ClaimWorkflowNode :one
UPDATE workflow_node_execution
SET status = 'running', revision = revision + 1,
    fence_token = fence_token + 1, attempt_count = attempt_count + 1,
    active_attempt_id = @attempt_id,
    started_at = COALESCE(started_at, now()), updated_at = now()
WHERE id = @id AND run_id = @run_id AND workspace_id = @workspace_id
  AND status = 'ready' AND revision = @expected_revision
RETURNING *;

-- name: CreateWorkflowAttempt :one
INSERT INTO workflow_attempt (
    id, workspace_id, run_id, node_id, attempt_no, fence_token,
    task_id, executor_id, runtime_id, status, deadline_at
) VALUES (
    @id, @workspace_id, @run_id, @node_id, @attempt_no, @fence_token,
    sqlc.narg(task_id), sqlc.narg(executor_id), sqlc.narg(runtime_id),
    'queued', sqlc.narg(deadline_at)
)
RETURNING *;

-- name: BindWorkflowAttemptTask :one
UPDATE workflow_attempt
SET task_id = @task_id, updated_at = now()
WHERE id = @id AND run_id = @run_id AND workspace_id = @workspace_id
  AND task_id IS NULL AND status = 'queued'
RETURNING *;

-- name: SetAgentTaskWorkflowRetryOwnership :one
UPDATE agent_task_queue
SET max_attempts = 1
WHERE id = @id AND status IN ('queued', 'dispatched', 'running')
RETURNING *;

-- name: GetWorkflowAttemptByTaskForUpdate :one
SELECT * FROM workflow_attempt WHERE task_id = @task_id FOR UPDATE;

-- name: GetWorkflowAttemptByTask :one
SELECT * FROM workflow_attempt WHERE task_id = @task_id;

-- name: GetWorkflowAttempt :one
SELECT * FROM workflow_attempt
WHERE id = @id AND run_id = @run_id AND workspace_id = @workspace_id;

-- name: ListWorkflowAttempts :many
SELECT * FROM workflow_attempt
WHERE run_id = @run_id AND workspace_id = @workspace_id
ORDER BY created_at, attempt_no;

-- name: SubmitWorkflowAttemptResult :one
UPDATE workflow_attempt
SET status = 'result_submitted', result_payload = @result_payload,
    result_digest = @result_digest, completed_at = now(), updated_at = now()
WHERE id = @id AND run_id = @run_id AND workspace_id = @workspace_id
  AND fence_token = @fence_token AND status IN ('queued', 'dispatched', 'running')
RETURNING *;

-- name: FailWorkflowAttempt :one
UPDATE workflow_attempt
SET status = 'execution_failed', failure_code = @failure_code,
    failure_detail = sqlc.narg(failure_detail), completed_at = now(), updated_at = now()
WHERE id = @id AND run_id = @run_id AND workspace_id = @workspace_id
  AND fence_token = @fence_token AND status IN ('queued', 'dispatched', 'running')
RETURNING *;

-- name: SetWorkflowNodeVerifying :one
UPDATE workflow_node_execution
SET status = 'verifying', revision = revision + 1, updated_at = now()
WHERE id = @id AND run_id = @run_id AND workspace_id = @workspace_id
  AND active_attempt_id = @attempt_id AND fence_token = @fence_token
  AND status = 'running'
RETURNING *;

-- name: FinishWorkflowNode :one
UPDATE workflow_node_execution
SET status = @status, revision = revision + 1, active_attempt_id = NULL,
    failure_code = sqlc.narg(failure_code), failure_detail = sqlc.narg(failure_detail),
    completed_at = CASE WHEN @status IN ('succeeded', 'failed', 'cancelled') THEN now() ELSE NULL END,
    next_retry_at = sqlc.narg(next_retry_at), updated_at = now()
WHERE id = @id AND run_id = @run_id AND workspace_id = @workspace_id
  AND active_attempt_id = @attempt_id AND fence_token = @fence_token
  AND status = @expected_status
RETURNING *;

-- name: ReleaseReadyWorkflowNodes :many
UPDATE workflow_node_execution AS successor
SET status = 'ready', revision = successor.revision + 1,
    ready_at = now(), updated_at = now()
WHERE successor.run_id = @run_id AND successor.workspace_id = @workspace_id
  AND successor.status = 'waiting'
  AND NOT EXISTS (
      SELECT 1
      FROM workflow_node_dependency dependency
      JOIN workflow_node_execution predecessor ON predecessor.id = dependency.predecessor_node_id
      WHERE dependency.run_id = successor.run_id
        AND dependency.successor_node_id = successor.id
        AND predecessor.status <> 'succeeded'
  )
RETURNING successor.*;

-- name: CountIncompleteWorkflowNodes :one
SELECT count(*) FROM workflow_node_execution
WHERE run_id = @run_id AND workspace_id = @workspace_id AND status <> 'succeeded';

-- name: CreateWorkflowVerification :one
INSERT INTO workflow_verification (
    id, workspace_id, run_id, node_id, attempt_id, verification_no,
    verifier_kind, verifier_id, status, result, failure_code, idempotency_key,
    started_at, completed_at
) VALUES (
    @id, @workspace_id, @run_id, @node_id, @attempt_id, @verification_no,
    @verifier_kind, sqlc.narg(verifier_id), @status, @result,
    sqlc.narg(failure_code), @idempotency_key, now(), now()
)
RETURNING *;

-- name: ListWorkflowVerifications :many
SELECT * FROM workflow_verification
WHERE run_id = @run_id AND workspace_id = @workspace_id
ORDER BY created_at, verification_no;

-- name: AppendWorkflowEvent :one
INSERT INTO workflow_event (
    id, workspace_id, run_id, sequence, aggregate_type, aggregate_id,
    event_type, from_state, to_state, aggregate_revision, actor_type,
    actor_id, attempt_id, verification_id, idempotency_key, payload
) VALUES (
    @id, @workspace_id, @run_id, @sequence, @aggregate_type, @aggregate_id,
    @event_type, sqlc.narg(from_state), sqlc.narg(to_state), @aggregate_revision,
    @actor_type, sqlc.narg(actor_id), sqlc.narg(attempt_id),
    sqlc.narg(verification_id), @idempotency_key, @payload
)
RETURNING *;

-- name: ListWorkflowEvents :many
SELECT * FROM workflow_event
WHERE run_id = @run_id AND workspace_id = @workspace_id
ORDER BY sequence;

-- name: GetWorkflowEventByIdempotencyKey :one
SELECT * FROM workflow_event
WHERE run_id = @run_id AND workspace_id = @workspace_id
  AND idempotency_key = @idempotency_key;

-- name: EnqueueWorkflowOutbox :one
INSERT INTO workflow_outbox (
    id, workspace_id, run_id, event_id, topic, event_key, payload
) VALUES (@id, @workspace_id, @run_id, @event_id, @topic, @event_key, @payload)
RETURNING *;

-- name: LeaseWorkflowOutbox :many
WITH candidates AS (
    SELECT id FROM workflow_outbox
    WHERE status IN ('pending', 'leased')
      AND available_at <= now()
      AND (lease_expires_at IS NULL OR lease_expires_at < now())
    ORDER BY available_at, created_at
    FOR UPDATE SKIP LOCKED
    LIMIT @batch_size
)
UPDATE workflow_outbox AS item
SET status = 'leased', lease_token = @lease_token,
    lease_expires_at = now() + (CAST(sqlc.arg('lease_seconds') AS bigint) * interval '1 second'),
    attempts = attempts + 1, updated_at = now()
FROM candidates WHERE item.id = candidates.id
RETURNING item.*;

-- name: MarkWorkflowOutboxPublished :execrows
UPDATE workflow_outbox
SET status = 'published', published_at = now(), lease_token = NULL,
    lease_expires_at = NULL, updated_at = now()
WHERE id = @id AND lease_token = @lease_token AND status = 'leased';

-- name: RetryWorkflowOutbox :execrows
UPDATE workflow_outbox
SET status = CASE WHEN attempts >= @max_attempts THEN 'dead' ELSE 'pending' END,
    available_at = @available_at, last_error = @last_error,
    lease_token = NULL, lease_expires_at = NULL, updated_at = now()
WHERE id = @id AND lease_token = @lease_token AND status = 'leased';

-- name: DeleteWorkspaceWorkflowRuntime :exec
WITH deleted_outbox AS (DELETE FROM workflow_outbox WHERE workspace_id = @workspace_id),
deleted_events AS (DELETE FROM workflow_event WHERE workspace_id = @workspace_id),
deleted_verifications AS (DELETE FROM workflow_verification WHERE workspace_id = @workspace_id),
deleted_artifacts AS (DELETE FROM workflow_artifact WHERE workspace_id = @workspace_id),
deleted_attempts AS (DELETE FROM workflow_attempt WHERE workspace_id = @workspace_id),
deleted_dependencies AS (DELETE FROM workflow_node_dependency WHERE workspace_id = @workspace_id),
deleted_nodes AS (DELETE FROM workflow_node_execution WHERE workspace_id = @workspace_id)
DELETE FROM workflow_run WHERE workflow_run.workspace_id = @workspace_id;
