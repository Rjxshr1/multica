CREATE UNIQUE INDEX CONCURRENTLY workflow_artifact_attempt_digest_uidx ON workflow_artifact (attempt_id, digest);
