CREATE UNIQUE INDEX CONCURRENTLY workflow_node_run_key_uidx ON workflow_node_execution (run_id, node_key);
