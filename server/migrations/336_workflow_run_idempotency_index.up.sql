CREATE UNIQUE INDEX CONCURRENTLY workflow_run_workspace_idempotency_uidx ON workflow_run (workspace_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
