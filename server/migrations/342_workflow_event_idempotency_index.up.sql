CREATE UNIQUE INDEX CONCURRENTLY workflow_event_run_idempotency_uidx ON workflow_event (run_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
