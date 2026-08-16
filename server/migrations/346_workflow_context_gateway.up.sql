-- Immutable, task-scoped context snapshots for Workflow Attempts. Agents read
-- these through the Context Gateway; they never mutate authoritative state.

CREATE TABLE workflow_context_snapshot (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    run_id UUID NOT NULL,
    node_id UUID NOT NULL,
    attempt_id UUID NOT NULL,
    task_id UUID NOT NULL,
    run_revision BIGINT NOT NULL CHECK (run_revision >= 0),
    digest TEXT NOT NULL,
    manifest JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(manifest) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX workflow_context_snapshot_attempt_idx
    ON workflow_context_snapshot (attempt_id);
CREATE UNIQUE INDEX workflow_context_snapshot_task_idx
    ON workflow_context_snapshot (task_id);
CREATE INDEX workflow_context_snapshot_workspace_idx
    ON workflow_context_snapshot (workspace_id);

CREATE TABLE workflow_context_item (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    snapshot_id UUID NOT NULL,
    ordinal INT NOT NULL CHECK (ordinal >= 0),
    reference_key TEXT NOT NULL CHECK (char_length(reference_key) BETWEEN 1 AND 512),
    kind TEXT NOT NULL CHECK (kind IN ('task', 'workflow', 'dependency', 'attempt', 'verification', 'event', 'artifact')),
    title TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 512),
    content JSONB NOT NULL CHECK (jsonb_typeof(content) = 'object'),
    search_text TEXT NOT NULL,
    source_type TEXT NOT NULL,
    source_id UUID,
    source_digest TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX workflow_context_item_reference_idx
    ON workflow_context_item (snapshot_id, reference_key);
CREATE INDEX workflow_context_item_snapshot_ordinal_idx
    ON workflow_context_item (snapshot_id, ordinal);
CREATE INDEX workflow_context_item_workspace_idx
    ON workflow_context_item (workspace_id);
