CREATE UNIQUE INDEX CONCURRENTLY workflow_verification_run_idempotency_uidx ON workflow_verification (run_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
