CREATE UNIQUE INDEX CONCURRENTLY workflow_event_run_sequence_uidx ON workflow_event (run_id, sequence);
