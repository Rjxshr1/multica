CREATE INDEX CONCURRENTLY workflow_outbox_claim_idx ON workflow_outbox (status, available_at);
