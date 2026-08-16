CREATE UNIQUE INDEX CONCURRENTLY workflow_attempt_node_number_uidx ON workflow_attempt (node_id, attempt_no);
