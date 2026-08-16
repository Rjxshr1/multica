CREATE UNIQUE INDEX CONCURRENTLY workflow_attempt_task_uidx ON workflow_attempt (task_id) WHERE task_id IS NOT NULL;
