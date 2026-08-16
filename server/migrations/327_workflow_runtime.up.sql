-- Authoritative Workflow Runtime state. Relationships are validated by the
-- service layer; no foreign keys are used so workspace teardown remains explicit.

CREATE TABLE workflow_run (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    root_issue_id UUID,
    status TEXT NOT NULL DEFAULT 'running'
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')),
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0),
    definition_key TEXT NOT NULL,
    definition_version TEXT NOT NULL,
    definition_digest TEXT NOT NULL,
    plan_snapshot JSONB NOT NULL CHECK (jsonb_typeof(plan_snapshot) = 'object'),
    policy_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(policy_snapshot) = 'object'),
    idempotency_key TEXT,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('member', 'agent', 'system')),
    created_by_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (idempotency_key IS NULL OR char_length(idempotency_key) BETWEEN 1 AND 256)
);

CREATE TABLE workflow_node_execution (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    run_id UUID NOT NULL,
    node_key TEXT NOT NULL CHECK (char_length(node_key) BETWEEN 1 AND 256),
    issue_id UUID,
    node_kind TEXT NOT NULL CHECK (node_kind IN ('agent_task', 'human_gate', 'external_check')),
    status TEXT NOT NULL CHECK (status IN ('waiting', 'ready', 'running', 'verifying', 'retry_wait', 'succeeded', 'failed', 'cancelled')),
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0),
    fence_token BIGINT NOT NULL DEFAULT 0 CHECK (fence_token >= 0),
    active_attempt_id UUID,
    attempt_count INT NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    executor_spec JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(executor_spec) = 'object'),
    retry_policy JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(retry_policy) = 'object'),
    verification_policy JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(verification_policy) = 'object'),
    input_spec JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(input_spec) = 'object'),
    input_digest TEXT NOT NULL,
    ready_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    next_retry_at TIMESTAMPTZ,
    failure_code TEXT,
    failure_detail TEXT CHECK (failure_detail IS NULL OR char_length(failure_detail) <= 4096),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE workflow_node_dependency (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    run_id UUID NOT NULL,
    predecessor_node_id UUID NOT NULL,
    successor_node_id UUID NOT NULL,
    condition TEXT NOT NULL DEFAULT 'required_success' CHECK (condition = 'required_success'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (predecessor_node_id <> successor_node_id)
);

CREATE TABLE workflow_attempt (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    run_id UUID NOT NULL,
    node_id UUID NOT NULL,
    attempt_no INT NOT NULL CHECK (attempt_no > 0),
    fence_token BIGINT NOT NULL CHECK (fence_token > 0),
    task_id UUID,
    executor_id UUID,
    runtime_id UUID,
    status TEXT NOT NULL CHECK (status IN ('queued', 'dispatched', 'running', 'result_submitted', 'execution_failed', 'expired', 'cancelled')),
    result_payload JSONB,
    result_digest TEXT,
    failure_code TEXT,
    failure_detail TEXT CHECK (failure_detail IS NULL OR char_length(failure_detail) <= 4096),
    deadline_at TIMESTAMPTZ,
    next_retry_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (result_payload IS NULL OR jsonb_typeof(result_payload) = 'object')
);

CREATE TABLE workflow_artifact (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    run_id UUID NOT NULL,
    node_id UUID NOT NULL,
    attempt_id UUID NOT NULL,
    kind TEXT NOT NULL,
    uri TEXT NOT NULL,
    digest TEXT NOT NULL,
    manifest JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(manifest) = 'object'),
    created_by_task_id UUID,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('member', 'agent', 'system')),
    created_by_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE workflow_verification (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    run_id UUID NOT NULL,
    node_id UUID NOT NULL,
    attempt_id UUID NOT NULL,
    verification_no INT NOT NULL CHECK (verification_no > 0),
    verifier_kind TEXT NOT NULL CHECK (verifier_kind IN ('agent', 'system', 'human', 'external_ci')),
    verifier_id UUID,
    verification_task_id UUID,
    status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'passed', 'failed', 'error', 'cancelled')),
    input_artifact_digest TEXT,
    result JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(result) = 'object'),
    failure_code TEXT,
    idempotency_key TEXT,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE workflow_event (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    run_id UUID NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    aggregate_type TEXT NOT NULL CHECK (aggregate_type IN ('run', 'node', 'attempt', 'verification')),
    aggregate_id UUID NOT NULL,
    event_type TEXT NOT NULL,
    from_state TEXT,
    to_state TEXT,
    aggregate_revision BIGINT NOT NULL CHECK (aggregate_revision >= 0),
    actor_type TEXT NOT NULL CHECK (actor_type IN ('member', 'agent', 'system')),
    actor_id UUID,
    attempt_id UUID,
    verification_id UUID,
    idempotency_key TEXT,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE workflow_outbox (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    run_id UUID NOT NULL,
    event_id UUID NOT NULL,
    topic TEXT NOT NULL,
    event_key TEXT NOT NULL,
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'leased', 'published', 'dead')),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    attempts INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT CHECK (last_error IS NULL OR char_length(last_error) <= 4096),
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
