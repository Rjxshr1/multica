CREATE UNIQUE INDEX CONCURRENTLY workflow_node_dependency_edge_uidx ON workflow_node_dependency (run_id, predecessor_node_id, successor_node_id);
