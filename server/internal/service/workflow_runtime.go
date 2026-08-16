package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	ErrWorkflowStaleRevision = errors.New("stale workflow revision")
	ErrWorkflowStaleFence    = errors.New("stale workflow fence")
	ErrWorkflowInvalidState  = errors.New("invalid workflow state")
)

type WorkflowNodeSpec struct {
	Key                string         `json:"key"`
	Kind               string         `json:"kind"`
	DependsOn          []string       `json:"depends_on,omitempty"`
	ExecutorSpec       map[string]any `json:"executor_spec,omitempty"`
	RetryPolicy        map[string]any `json:"retry_policy,omitempty"`
	VerificationPolicy map[string]any `json:"verification_policy,omitempty"`
	Input              map[string]any `json:"input,omitempty"`
}

type WorkflowPlan struct {
	DefinitionKey     string             `json:"definition_key"`
	DefinitionVersion string             `json:"definition_version"`
	Nodes             []WorkflowNodeSpec `json:"nodes"`
}

type WorkflowActor struct {
	Type string
	ID   pgtype.UUID
}

type CreateWorkflowRunInput struct {
	WorkspaceID    pgtype.UUID
	IdempotencyKey string
	Plan           WorkflowPlan
	Policy         map[string]any
	Actor          WorkflowActor
}

type WorkflowSnapshot struct {
	Run           db.WorkflowRun              `json:"run"`
	Nodes         []db.WorkflowNodeExecution  `json:"nodes"`
	Dependencies  []db.WorkflowNodeDependency `json:"dependencies"`
	Attempts      []db.WorkflowAttempt        `json:"attempts"`
	Verifications []db.WorkflowVerification   `json:"verifications"`
	Events        []db.WorkflowEvent          `json:"events"`
}

type WorkflowLease struct {
	RunID      pgtype.UUID `json:"run_id"`
	NodeID     pgtype.UUID `json:"node_id"`
	AttemptID  pgtype.UUID `json:"attempt_id"`
	AttemptNo  int32       `json:"attempt_no"`
	FenceToken int64       `json:"fence_token"`
}

type VerificationInput struct {
	WorkspaceID    pgtype.UUID
	RunID          pgtype.UUID
	AttemptID      pgtype.UUID
	FenceToken     int64
	Passed         bool
	FailureCode    string
	Result         map[string]any
	VerifierKind   string
	VerifierID     pgtype.UUID
	IdempotencyKey string
}

type InsertWorkflowNodeBeforeInput struct {
	WorkspaceID    pgtype.UUID
	RunID          pgtype.UUID
	TargetNodeKey  string
	Node           WorkflowNodeSpec
	IdempotencyKey string
	Actor          WorkflowActor
}

type WorkflowRuntimeService struct {
	Queries   *db.Queries
	TxStarter TxStarter
}

func NewWorkflowRuntimeService(queries *db.Queries, txStarter TxStarter) *WorkflowRuntimeService {
	return &WorkflowRuntimeService{Queries: queries, TxStarter: txStarter}
}

func (s *WorkflowRuntimeService) CreateRun(ctx context.Context, input CreateWorkflowRunInput) (*WorkflowSnapshot, error) {
	if !input.WorkspaceID.Valid {
		return nil, fmt.Errorf("workspace_id is required")
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return nil, fmt.Errorf("idempotency_key is required")
	}
	if err := validateWorkflowPlan(input.Plan); err != nil {
		return nil, err
	}
	if input.Actor.Type == "" {
		input.Actor.Type = "system"
	}

	planJSON, err := json.Marshal(input.Plan)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow plan: %w", err)
	}
	policyJSON, err := marshalObject(input.Policy)
	if err != nil {
		return nil, err
	}
	digest := digestBytes(planJSON)
	runID := newPGUUID()

	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	_, err = qtx.CreateWorkflowRun(ctx, db.CreateWorkflowRunParams{
		ID: runID, WorkspaceID: input.WorkspaceID,
		DefinitionKey: input.Plan.DefinitionKey, DefinitionVersion: input.Plan.DefinitionVersion,
		DefinitionDigest: digest, PlanSnapshot: planJSON, PolicySnapshot: policyJSON,
		IdempotencyKey: textValue(input.IdempotencyKey), CreatedByType: input.Actor.Type,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			_ = tx.Rollback(ctx)
			existing, lookupErr := s.Queries.GetWorkflowRunByIdempotencyKey(ctx, db.GetWorkflowRunByIdempotencyKeyParams{
				WorkspaceID: input.WorkspaceID, IdempotencyKey: textValue(input.IdempotencyKey),
			})
			if lookupErr != nil {
				return nil, fmt.Errorf("resolve idempotent workflow run: %w", lookupErr)
			}
			return s.Snapshot(ctx, input.WorkspaceID, existing.ID)
		}
		return nil, fmt.Errorf("create workflow run: %w", err)
	}

	nodeIDs := make(map[string]pgtype.UUID, len(input.Plan.Nodes))
	for _, spec := range input.Plan.Nodes {
		nodeIDs[spec.Key] = newPGUUID()
	}
	for _, spec := range input.Plan.Nodes {
		status := "waiting"
		if len(spec.DependsOn) == 0 {
			status = "ready"
		}
		executorJSON, _ := marshalObject(spec.ExecutorSpec)
		retryJSON, _ := marshalObject(withDefaultRetryPolicy(spec.RetryPolicy))
		verificationJSON, _ := marshalObject(spec.VerificationPolicy)
		inputJSON, _ := marshalObject(spec.Input)
		if _, err := qtx.CreateWorkflowNode(ctx, db.CreateWorkflowNodeParams{
			ID: nodeIDs[spec.Key], WorkspaceID: input.WorkspaceID, RunID: runID,
			NodeKey: spec.Key, NodeKind: defaultNodeKind(spec.Kind), Status: status,
			ExecutorSpec: executorJSON, RetryPolicy: retryJSON,
			VerificationPolicy: verificationJSON, InputSpec: inputJSON,
			InputDigest: digestBytes(inputJSON),
		}); err != nil {
			return nil, fmt.Errorf("create workflow node %q: %w", spec.Key, err)
		}
	}
	for _, spec := range input.Plan.Nodes {
		for _, predecessor := range spec.DependsOn {
			if _, err := qtx.CreateWorkflowDependency(ctx, db.CreateWorkflowDependencyParams{
				ID: newPGUUID(), WorkspaceID: input.WorkspaceID, RunID: runID,
				PredecessorNodeID: nodeIDs[predecessor], SuccessorNodeID: nodeIDs[spec.Key],
			}); err != nil {
				return nil, fmt.Errorf("create workflow dependency %q -> %q: %w", predecessor, spec.Key, err)
			}
		}
	}
	if err := s.appendEvent(ctx, qtx, eventInput{
		WorkspaceID: input.WorkspaceID, RunID: runID, AggregateType: "run", AggregateID: runID,
		EventType: "workflow.created", FromState: "", ToState: "running", AggregateRevision: 0,
		Actor: input.Actor, IdempotencyKey: input.IdempotencyKey,
		Payload: map[string]any{"definition_digest": digest},
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit workflow run: %w", err)
	}
	return s.Snapshot(ctx, input.WorkspaceID, runID)
}

func (s *WorkflowRuntimeService) Snapshot(ctx context.Context, workspaceID, runID pgtype.UUID) (*WorkflowSnapshot, error) {
	run, err := s.Queries.GetWorkflowRun(ctx, db.GetWorkflowRunParams{ID: runID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	nodes, err := s.Queries.ListWorkflowNodes(ctx, db.ListWorkflowNodesParams{RunID: runID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	deps, err := s.Queries.ListWorkflowDependencies(ctx, db.ListWorkflowDependenciesParams{RunID: runID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	attempts, err := s.Queries.ListWorkflowAttempts(ctx, db.ListWorkflowAttemptsParams{RunID: runID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	verifications, err := s.Queries.ListWorkflowVerifications(ctx, db.ListWorkflowVerificationsParams{RunID: runID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	events, err := s.Queries.ListWorkflowEvents(ctx, db.ListWorkflowEventsParams{RunID: runID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	return &WorkflowSnapshot{Run: run, Nodes: nodes, Dependencies: deps, Attempts: attempts, Verifications: verifications, Events: events}, nil
}

// InsertNodeBefore atomically amends a running plan. The new gate inherits the
// target's predecessors, and the target is rewired to depend only on the gate.
func (s *WorkflowRuntimeService) InsertNodeBefore(ctx context.Context, input InsertWorkflowNodeBeforeInput) (*WorkflowSnapshot, error) {
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return nil, fmt.Errorf("idempotency_key is required")
	}
	if strings.TrimSpace(input.TargetNodeKey) == "" || strings.TrimSpace(input.Node.Key) == "" {
		return nil, fmt.Errorf("target_node_key and node.key are required")
	}
	if input.TargetNodeKey == input.Node.Key {
		return nil, fmt.Errorf("inserted node cannot target itself")
	}
	if len(input.Node.DependsOn) != 0 {
		return nil, fmt.Errorf("inserted node dependencies are derived from target")
	}
	err := s.inTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.LockWorkflowRun(ctx, util.UUIDToString(input.RunID)); err != nil {
			return err
		}
		if _, err := qtx.GetWorkflowEventByIdempotencyKey(ctx, db.GetWorkflowEventByIdempotencyKeyParams{RunID: input.RunID, WorkspaceID: input.WorkspaceID, IdempotencyKey: textValue(input.IdempotencyKey)}); err == nil {
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		run, err := qtx.GetWorkflowRun(ctx, db.GetWorkflowRunParams{ID: input.RunID, WorkspaceID: input.WorkspaceID})
		if err != nil {
			return err
		}
		if run.Status != "running" {
			return fmt.Errorf("%w: workflow is %s", ErrWorkflowInvalidState, run.Status)
		}
		target, err := qtx.GetWorkflowNodeByKeyForUpdate(ctx, db.GetWorkflowNodeByKeyForUpdateParams{NodeKey: input.TargetNodeKey, RunID: input.RunID, WorkspaceID: input.WorkspaceID})
		if err != nil {
			return err
		}
		if target.Status != "ready" && target.Status != "waiting" {
			return fmt.Errorf("%w: target node %s is %s", ErrWorkflowInvalidState, input.TargetNodeKey, target.Status)
		}
		if _, err := qtx.GetWorkflowNodeByKeyForUpdate(ctx, db.GetWorkflowNodeByKeyForUpdateParams{NodeKey: input.Node.Key, RunID: input.RunID, WorkspaceID: input.WorkspaceID}); err == nil {
			return fmt.Errorf("node key %q already exists", input.Node.Key)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		dependencies, err := qtx.ListWorkflowDependencies(ctx, db.ListWorkflowDependenciesParams{RunID: input.RunID, WorkspaceID: input.WorkspaceID})
		if err != nil {
			return err
		}
		predecessors := make([]pgtype.UUID, 0)
		for _, dependency := range dependencies {
			if sameUUID(dependency.SuccessorNodeID, target.ID) {
				predecessors = append(predecessors, dependency.PredecessorNodeID)
			}
		}
		status := "ready"
		if len(predecessors) > 0 {
			status = "waiting"
			nodes, listErr := qtx.ListWorkflowNodes(ctx, db.ListWorkflowNodesParams{RunID: input.RunID, WorkspaceID: input.WorkspaceID})
			if listErr != nil {
				return listErr
			}
			states := make(map[[16]byte]string, len(nodes))
			for _, node := range nodes {
				states[node.ID.Bytes] = node.Status
			}
			allSucceeded := true
			for _, predecessor := range predecessors {
				allSucceeded = allSucceeded && states[predecessor.Bytes] == "succeeded"
			}
			if allSucceeded {
				status = "ready"
			}
		}
		nodeID := newPGUUID()
		executorJSON, _ := marshalObject(input.Node.ExecutorSpec)
		retryJSON, _ := marshalObject(withDefaultRetryPolicy(input.Node.RetryPolicy))
		verificationJSON, _ := marshalObject(input.Node.VerificationPolicy)
		inputJSON, _ := marshalObject(input.Node.Input)
		inserted, err := qtx.CreateWorkflowNode(ctx, db.CreateWorkflowNodeParams{ID: nodeID, WorkspaceID: input.WorkspaceID, RunID: input.RunID, NodeKey: input.Node.Key, NodeKind: defaultNodeKind(input.Node.Kind), Status: status, ExecutorSpec: executorJSON, RetryPolicy: retryJSON, VerificationPolicy: verificationJSON, InputSpec: inputJSON, InputDigest: digestBytes(inputJSON)})
		if err != nil {
			return err
		}
		if err := qtx.DeleteWorkflowDependenciesForSuccessor(ctx, db.DeleteWorkflowDependenciesForSuccessorParams{RunID: input.RunID, WorkspaceID: input.WorkspaceID, SuccessorNodeID: target.ID}); err != nil {
			return err
		}
		for _, predecessor := range predecessors {
			if _, err := qtx.CreateWorkflowDependency(ctx, db.CreateWorkflowDependencyParams{ID: newPGUUID(), WorkspaceID: input.WorkspaceID, RunID: input.RunID, PredecessorNodeID: predecessor, SuccessorNodeID: nodeID}); err != nil {
				return err
			}
		}
		if _, err := qtx.CreateWorkflowDependency(ctx, db.CreateWorkflowDependencyParams{ID: newPGUUID(), WorkspaceID: input.WorkspaceID, RunID: input.RunID, PredecessorNodeID: nodeID, SuccessorNodeID: target.ID}); err != nil {
			return err
		}
		updatedTarget, err := qtx.ResetWorkflowNodeForPlanAmendment(ctx, db.ResetWorkflowNodeForPlanAmendmentParams{ID: target.ID, RunID: input.RunID, WorkspaceID: input.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkflowStaleRevision
		}
		if err != nil {
			return err
		}
		var plan WorkflowPlan
		if err := json.Unmarshal(run.PlanSnapshot, &plan); err != nil {
			return fmt.Errorf("decode workflow plan snapshot: %w", err)
		}
		foundTarget := false
		for index := range plan.Nodes {
			if plan.Nodes[index].Key == input.TargetNodeKey {
				foundTarget = true
				input.Node.DependsOn = append([]string(nil), plan.Nodes[index].DependsOn...)
				plan.Nodes[index].DependsOn = []string{input.Node.Key}
				break
			}
		}
		if !foundTarget {
			return fmt.Errorf("workflow plan snapshot is missing target node %q", input.TargetNodeKey)
		}
		plan.Nodes = append(plan.Nodes, input.Node)
		planJSON, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		if _, err := qtx.UpdateWorkflowRunPlanSnapshot(ctx, db.UpdateWorkflowRunPlanSnapshotParams{PlanSnapshot: planJSON, DefinitionDigest: digestBytes(planJSON), ID: input.RunID, WorkspaceID: input.WorkspaceID}); err != nil {
			return err
		}
		return s.appendEvent(ctx, qtx, eventInput{WorkspaceID: input.WorkspaceID, RunID: input.RunID, AggregateType: "node", AggregateID: inserted.ID, EventType: "node.inserted", FromState: "", ToState: status, AggregateRevision: inserted.Revision, Actor: input.Actor, IdempotencyKey: input.IdempotencyKey, Payload: map[string]any{"node_key": input.Node.Key, "before_node_key": input.TargetNodeKey, "target_revision": updatedTarget.Revision}})
	})
	if err != nil {
		return nil, err
	}
	return s.Snapshot(ctx, input.WorkspaceID, input.RunID)
}

func (s *WorkflowRuntimeService) ClaimNode(ctx context.Context, workspaceID, runID pgtype.UUID, nodeKey, idempotencyKey string, actor WorkflowActor) (WorkflowLease, error) {
	var lease WorkflowLease
	if strings.TrimSpace(idempotencyKey) == "" {
		return lease, fmt.Errorf("idempotency_key is required")
	}
	err := s.inTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.LockWorkflowRun(ctx, util.UUIDToString(runID)); err != nil {
			return err
		}
		if _, err := qtx.GetWorkflowEventByIdempotencyKey(ctx, db.GetWorkflowEventByIdempotencyKeyParams{RunID: runID, WorkspaceID: workspaceID, IdempotencyKey: textValue(idempotencyKey)}); err == nil {
			node, err := qtx.GetWorkflowNodeByKeyForUpdate(ctx, db.GetWorkflowNodeByKeyForUpdateParams{NodeKey: nodeKey, RunID: runID, WorkspaceID: workspaceID})
			if err != nil {
				return err
			}
			attempt, err := qtx.GetWorkflowAttempt(ctx, db.GetWorkflowAttemptParams{ID: node.ActiveAttemptID, RunID: runID, WorkspaceID: workspaceID})
			if err != nil {
				return err
			}
			lease = leaseFromAttempt(attempt)
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		node, err := qtx.GetWorkflowNodeByKeyForUpdate(ctx, db.GetWorkflowNodeByKeyForUpdateParams{NodeKey: nodeKey, RunID: runID, WorkspaceID: workspaceID})
		if err != nil {
			return err
		}
		if node.Status != "ready" {
			return fmt.Errorf("%w: node %s is %s", ErrWorkflowInvalidState, nodeKey, node.Status)
		}
		if node.AttemptCount >= maxAttempts(node.RetryPolicy) {
			return fmt.Errorf("%w: retry budget exhausted", ErrWorkflowInvalidState)
		}
		attemptID := newPGUUID()
		claimed, err := qtx.ClaimWorkflowNode(ctx, db.ClaimWorkflowNodeParams{AttemptID: attemptID, ID: node.ID, RunID: runID, WorkspaceID: workspaceID, ExpectedRevision: node.Revision})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkflowStaleRevision
		}
		if err != nil {
			return err
		}
		executorID, runtimeID := executionIDs(node.ExecutorSpec)
		attempt, err := qtx.CreateWorkflowAttempt(ctx, db.CreateWorkflowAttemptParams{
			ID: attemptID, WorkspaceID: workspaceID, RunID: runID, NodeID: node.ID,
			AttemptNo: claimed.AttemptCount, FenceToken: claimed.FenceToken,
			ExecutorID: executorID, RuntimeID: runtimeID,
		})
		if err != nil {
			return err
		}
		lease = leaseFromAttempt(attempt)
		return s.appendEvent(ctx, qtx, eventInput{
			WorkspaceID: workspaceID, RunID: runID, AggregateType: "node", AggregateID: node.ID,
			EventType: "node.claimed", FromState: "ready", ToState: "running", AggregateRevision: claimed.Revision,
			Actor: actor, AttemptID: attemptID, IdempotencyKey: idempotencyKey,
			Payload: map[string]any{"attempt_no": attempt.AttemptNo, "fence_token": attempt.FenceToken},
		})
	})
	return lease, err
}

func (s *WorkflowRuntimeService) BindTask(ctx context.Context, workspaceID pgtype.UUID, lease WorkflowLease, taskID pgtype.UUID) error {
	return s.inTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.LockWorkflowRun(ctx, util.UUIDToString(lease.RunID)); err != nil {
			return err
		}
		if _, err := qtx.SetAgentTaskWorkflowRetryOwnership(ctx, taskID); err != nil {
			return fmt.Errorf("bind workflow task retry ownership: %w", err)
		}
		attempt, err := qtx.BindWorkflowAttemptTask(ctx, db.BindWorkflowAttemptTaskParams{TaskID: taskID, ID: lease.AttemptID, RunID: lease.RunID, WorkspaceID: workspaceID})
		if err != nil {
			return err
		}
		return s.createContextSnapshotTx(ctx, qtx, attempt, taskID)
	})
}

// SettleTaskSuccessTx records executor output without declaring the node successful.
// It is called from TaskService's completion transaction.
func (s *WorkflowRuntimeService) SettleTaskSuccessTx(ctx context.Context, qtx *db.Queries, taskID pgtype.UUID, result []byte) (bool, error) {
	candidate, err := qtx.GetWorkflowAttemptByTask(ctx, taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := qtx.LockWorkflowRun(ctx, util.UUIDToString(candidate.RunID)); err != nil {
		return true, err
	}
	attempt, err := qtx.GetWorkflowAttemptByTaskForUpdate(ctx, taskID)
	if err != nil {
		return true, err
	}
	node, err := qtx.GetWorkflowNodeForUpdate(ctx, db.GetWorkflowNodeForUpdateParams{ID: attempt.NodeID, RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID})
	if err != nil {
		return true, err
	}
	if !sameUUID(node.ActiveAttemptID, attempt.ID) || node.FenceToken != attempt.FenceToken {
		return true, ErrWorkflowStaleFence
	}
	payload := normalizeResultPayload(result)
	updatedAttempt, err := qtx.SubmitWorkflowAttemptResult(ctx, db.SubmitWorkflowAttemptResultParams{
		ResultPayload: payload, ResultDigest: textValue(digestBytes(payload)), ID: attempt.ID,
		RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID, FenceToken: attempt.FenceToken,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, ErrWorkflowStaleFence
	}
	if err != nil {
		return true, err
	}
	updatedNode, err := qtx.SetWorkflowNodeVerifying(ctx, db.SetWorkflowNodeVerifyingParams{ID: node.ID, RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID, AttemptID: attempt.ID, FenceToken: attempt.FenceToken})
	if err != nil {
		return true, err
	}
	err = s.appendEvent(ctx, qtx, eventInput{
		WorkspaceID: attempt.WorkspaceID, RunID: attempt.RunID, AggregateType: "attempt", AggregateID: attempt.ID,
		EventType: "task.result_accepted", FromState: attempt.Status, ToState: updatedAttempt.Status,
		AggregateRevision: updatedNode.Revision, Actor: WorkflowActor{Type: "agent", ID: attempt.ExecutorID},
		AttemptID: attempt.ID, IdempotencyKey: "task-complete:" + util.UUIDToString(taskID),
		Payload: map[string]any{"result_digest": updatedAttempt.ResultDigest.String},
	})
	return true, err
}

// SettleTaskFailureTx is the sole retry decision for Workflow-owned tasks.
func (s *WorkflowRuntimeService) SettleTaskFailureTx(ctx context.Context, qtx *db.Queries, taskID pgtype.UUID, failureCode, detail string) (bool, error) {
	candidate, err := qtx.GetWorkflowAttemptByTask(ctx, taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := qtx.LockWorkflowRun(ctx, util.UUIDToString(candidate.RunID)); err != nil {
		return true, err
	}
	attempt, err := qtx.GetWorkflowAttemptByTaskForUpdate(ctx, taskID)
	if err != nil {
		return true, err
	}
	// Failure reconciliation is deliberately idempotent. The normal daemon
	// failure path settles the attempt in the same transaction as the task,
	// while timeout/runtime sweepers may later surface the same failed row via
	// HandleFailedTasks. Once an attempt is terminal there is nothing left to
	// settle, but the task is still Workflow-owned and must never enter the
	// legacy task retry path.
	if attempt.Status != "queued" && attempt.Status != "dispatched" && attempt.Status != "running" {
		return true, nil
	}
	node, err := qtx.GetWorkflowNodeForUpdate(ctx, db.GetWorkflowNodeForUpdateParams{ID: attempt.NodeID, RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID})
	if err != nil {
		return true, err
	}
	if !sameUUID(node.ActiveAttemptID, attempt.ID) || node.FenceToken != attempt.FenceToken {
		return true, ErrWorkflowStaleFence
	}
	if _, err := qtx.FailWorkflowAttempt(ctx, db.FailWorkflowAttemptParams{FailureCode: textValue(failureCode), FailureDetail: textValue(truncateWorkflowDetail(detail)), ID: attempt.ID, RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID, FenceToken: attempt.FenceToken}); err != nil {
		return true, err
	}
	next := "failed"
	if retryable(node.RetryPolicy, failureCode) && node.AttemptCount < maxAttempts(node.RetryPolicy) {
		next = "ready"
	}
	updated, err := qtx.FinishWorkflowNode(ctx, db.FinishWorkflowNodeParams{Status: next, FailureCode: textValue(failureCode), FailureDetail: textValue(truncateWorkflowDetail(detail)), ID: node.ID, RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID, AttemptID: attempt.ID, FenceToken: attempt.FenceToken, ExpectedStatus: "running"})
	if err != nil {
		return true, err
	}
	if err := s.appendEvent(ctx, qtx, eventInput{WorkspaceID: attempt.WorkspaceID, RunID: attempt.RunID, AggregateType: "attempt", AggregateID: attempt.ID, EventType: "attempt.execution_failed", FromState: attempt.Status, ToState: "execution_failed", AggregateRevision: updated.Revision, Actor: WorkflowActor{Type: "agent", ID: attempt.ExecutorID}, AttemptID: attempt.ID, IdempotencyKey: "task-fail:" + util.UUIDToString(taskID), Payload: map[string]any{"failure_code": failureCode, "node_state": next}}); err != nil {
		return true, err
	}
	if next == "failed" {
		run, statusErr := qtx.SetWorkflowRunStatus(ctx, db.SetWorkflowRunStatusParams{Status: "failed", ID: attempt.RunID, WorkspaceID: attempt.WorkspaceID, ExpectedStatus: "running"})
		if statusErr != nil {
			return true, statusErr
		}
		err = s.appendEvent(ctx, qtx, eventInput{WorkspaceID: attempt.WorkspaceID, RunID: attempt.RunID, AggregateType: "run", AggregateID: attempt.RunID, EventType: "workflow.failed", FromState: "running", ToState: "failed", AggregateRevision: run.Revision, Actor: WorkflowActor{Type: "system"}, IdempotencyKey: "workflow-fail:" + util.UUIDToString(taskID), Payload: map[string]any{"failure_code": failureCode}})
	}
	return true, err
}

// SettleTaskFailure reconciles a task that was already marked failed outside
// TaskService.FailTask's transaction (for example by a timeout or offline
// runtime sweeper). Workflow-owned tasks re-enter the node-level retry policy;
// unbound tasks are left for the legacy task retry path.
func (s *WorkflowRuntimeService) SettleTaskFailure(ctx context.Context, taskID pgtype.UUID, failureCode, detail string) (bool, error) {
	var owned bool
	err := s.inTx(ctx, func(qtx *db.Queries) error {
		var err error
		owned, err = s.SettleTaskFailureTx(ctx, qtx, taskID, failureCode, detail)
		return err
	})
	return owned, err
}

func (s *WorkflowRuntimeService) Verify(ctx context.Context, input VerificationInput) (*WorkflowSnapshot, error) {
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return nil, fmt.Errorf("idempotency_key is required")
	}
	err := s.inTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.LockWorkflowRun(ctx, util.UUIDToString(input.RunID)); err != nil {
			return err
		}
		if _, err := qtx.GetWorkflowEventByIdempotencyKey(ctx, db.GetWorkflowEventByIdempotencyKeyParams{RunID: input.RunID, WorkspaceID: input.WorkspaceID, IdempotencyKey: textValue(input.IdempotencyKey)}); err == nil {
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		attempt, err := qtx.GetWorkflowAttempt(ctx, db.GetWorkflowAttemptParams{ID: input.AttemptID, RunID: input.RunID, WorkspaceID: input.WorkspaceID})
		if err != nil {
			return err
		}
		node, err := qtx.GetWorkflowNodeForUpdate(ctx, db.GetWorkflowNodeForUpdateParams{ID: attempt.NodeID, RunID: input.RunID, WorkspaceID: input.WorkspaceID})
		if err != nil {
			return err
		}
		if input.FenceToken != attempt.FenceToken || !sameUUID(node.ActiveAttemptID, attempt.ID) {
			return ErrWorkflowStaleFence
		}
		if node.Status != "verifying" || attempt.Status != "result_submitted" {
			return ErrWorkflowInvalidState
		}
		if independentVerification(node.VerificationPolicy) {
			if !attempt.ExecutorID.Valid {
				return fmt.Errorf("independent verification requires an executor identity")
			}
			if sameUUID(input.VerifierID, attempt.ExecutorID) {
				return fmt.Errorf("independent verifier must differ from executor")
			}
		}
		status := "failed"
		if input.Passed {
			status = "passed"
		}
		verificationID := newPGUUID()
		resultJSON, _ := marshalObject(input.Result)
		verification, err := qtx.CreateWorkflowVerification(ctx, db.CreateWorkflowVerificationParams{ID: verificationID, WorkspaceID: input.WorkspaceID, RunID: input.RunID, NodeID: node.ID, AttemptID: attempt.ID, VerificationNo: 1, VerifierKind: defaultVerifierKind(input.VerifierKind), VerifierID: input.VerifierID, Status: status, Result: resultJSON, FailureCode: nullableText(input.FailureCode), IdempotencyKey: textValue(input.IdempotencyKey)})
		if err != nil {
			return err
		}
		next := "succeeded"
		if !input.Passed {
			next = "failed"
			if retryable(node.RetryPolicy, input.FailureCode) && node.AttemptCount < maxAttempts(node.RetryPolicy) {
				next = "ready"
			}
		}
		updated, err := qtx.FinishWorkflowNode(ctx, db.FinishWorkflowNodeParams{Status: next, FailureCode: nullableText(input.FailureCode), ID: node.ID, RunID: input.RunID, WorkspaceID: input.WorkspaceID, AttemptID: attempt.ID, FenceToken: attempt.FenceToken, ExpectedStatus: "verifying"})
		if err != nil {
			return err
		}
		eventType := "verification.rejected"
		if input.Passed {
			eventType = "verification.passed"
		}
		if err := s.appendEvent(ctx, qtx, eventInput{WorkspaceID: input.WorkspaceID, RunID: input.RunID, AggregateType: "verification", AggregateID: verification.ID, EventType: eventType, FromState: "running", ToState: status, AggregateRevision: updated.Revision, Actor: WorkflowActor{Type: input.VerifierKind, ID: input.VerifierID}, AttemptID: attempt.ID, VerificationID: verification.ID, IdempotencyKey: input.IdempotencyKey, Payload: map[string]any{"failure_code": input.FailureCode, "node_state": next}}); err != nil {
			return err
		}
		if next == "succeeded" {
			released, err := qtx.ReleaseReadyWorkflowNodes(ctx, db.ReleaseReadyWorkflowNodesParams{RunID: input.RunID, WorkspaceID: input.WorkspaceID})
			if err != nil {
				return err
			}
			for _, readyNode := range released {
				if err := s.appendEvent(ctx, qtx, eventInput{WorkspaceID: input.WorkspaceID, RunID: input.RunID, AggregateType: "node", AggregateID: readyNode.ID, EventType: "node.ready", FromState: "waiting", ToState: "ready", AggregateRevision: readyNode.Revision, Actor: WorkflowActor{Type: "system"}, IdempotencyKey: input.IdempotencyKey + ":ready:" + readyNode.NodeKey, Payload: map[string]any{"released_by_node_id": util.UUIDToString(node.ID)}}); err != nil {
					return err
				}
			}
			remaining, err := qtx.CountIncompleteWorkflowNodes(ctx, db.CountIncompleteWorkflowNodesParams{RunID: input.RunID, WorkspaceID: input.WorkspaceID})
			if err != nil {
				return err
			}
			if remaining == 0 {
				run, statusErr := qtx.SetWorkflowRunStatus(ctx, db.SetWorkflowRunStatusParams{Status: "succeeded", ID: input.RunID, WorkspaceID: input.WorkspaceID, ExpectedStatus: "running"})
				if statusErr != nil {
					return statusErr
				}
				return s.appendEvent(ctx, qtx, eventInput{WorkspaceID: input.WorkspaceID, RunID: input.RunID, AggregateType: "run", AggregateID: input.RunID, EventType: "workflow.succeeded", FromState: "running", ToState: "succeeded", AggregateRevision: run.Revision, Actor: WorkflowActor{Type: "system"}, IdempotencyKey: input.IdempotencyKey + ":run", Payload: map[string]any{}})
			}
		} else if next == "failed" {
			run, statusErr := qtx.SetWorkflowRunStatus(ctx, db.SetWorkflowRunStatusParams{Status: "failed", ID: input.RunID, WorkspaceID: input.WorkspaceID, ExpectedStatus: "running"})
			if statusErr != nil {
				return statusErr
			}
			return s.appendEvent(ctx, qtx, eventInput{WorkspaceID: input.WorkspaceID, RunID: input.RunID, AggregateType: "run", AggregateID: input.RunID, EventType: "workflow.failed", FromState: "running", ToState: "failed", AggregateRevision: run.Revision, Actor: WorkflowActor{Type: "system"}, IdempotencyKey: input.IdempotencyKey + ":run", Payload: map[string]any{"failure_code": input.FailureCode}})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Snapshot(ctx, input.WorkspaceID, input.RunID)
}

func (s *WorkflowRuntimeService) IsWorkflowTask(ctx context.Context, taskID pgtype.UUID) bool {
	_, err := s.Queries.GetWorkflowAttemptByTask(ctx, taskID)
	return err == nil
}

type eventInput struct {
	WorkspaceID, RunID, AggregateID              pgtype.UUID
	AggregateType, EventType, FromState, ToState string
	AggregateRevision                            int64
	Actor                                        WorkflowActor
	AttemptID, VerificationID                    pgtype.UUID
	IdempotencyKey                               string
	Payload                                      map[string]any
}

func (s *WorkflowRuntimeService) appendEvent(ctx context.Context, q *db.Queries, input eventInput) error {
	sequence, err := q.IncrementWorkflowRunRevision(ctx, db.IncrementWorkflowRunRevisionParams{ID: input.RunID, WorkspaceID: input.WorkspaceID})
	if err != nil {
		return err
	}
	payload, _ := marshalObject(input.Payload)
	eventID := newPGUUID()
	event, err := q.AppendWorkflowEvent(ctx, db.AppendWorkflowEventParams{ID: eventID, WorkspaceID: input.WorkspaceID, RunID: input.RunID, Sequence: sequence, AggregateType: input.AggregateType, AggregateID: input.AggregateID, EventType: input.EventType, FromState: nullableText(input.FromState), ToState: nullableText(input.ToState), AggregateRevision: input.AggregateRevision, ActorType: defaultActorType(input.Actor.Type), ActorID: input.Actor.ID, AttemptID: input.AttemptID, VerificationID: input.VerificationID, IdempotencyKey: textValue(input.IdempotencyKey), Payload: payload})
	if err != nil {
		return fmt.Errorf("append workflow event: %w", err)
	}
	outboxPayload, _ := json.Marshal(map[string]any{"event_id": util.UUIDToString(event.ID), "run_id": util.UUIDToString(event.RunID), "sequence": event.Sequence, "event_type": event.EventType, "aggregate_type": event.AggregateType, "aggregate_id": util.UUIDToString(event.AggregateID), "data": json.RawMessage(event.Payload)})
	_, err = q.EnqueueWorkflowOutbox(ctx, db.EnqueueWorkflowOutboxParams{ID: newPGUUID(), WorkspaceID: input.WorkspaceID, RunID: input.RunID, EventID: event.ID, Topic: "workflow.events", EventKey: util.UUIDToString(input.RunID), Payload: outboxPayload})
	return err
}

func (s *WorkflowRuntimeService) begin(ctx context.Context) (pgx.Tx, error) {
	if s.TxStarter == nil {
		return nil, fmt.Errorf("workflow runtime requires transactional storage")
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin workflow transaction: %w", err)
	}
	return tx, nil
}

func (s *WorkflowRuntimeService) inTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateWorkflowPlan(plan WorkflowPlan) error {
	if strings.TrimSpace(plan.DefinitionKey) == "" || strings.TrimSpace(plan.DefinitionVersion) == "" {
		return fmt.Errorf("definition_key and definition_version are required")
	}
	if len(plan.Nodes) == 0 {
		return fmt.Errorf("workflow requires at least one node")
	}
	known := make(map[string]bool, len(plan.Nodes))
	for _, node := range plan.Nodes {
		if strings.TrimSpace(node.Key) == "" || known[node.Key] {
			return fmt.Errorf("node keys must be non-empty and unique")
		}
		known[node.Key] = true
	}
	indegree := make(map[string]int, len(plan.Nodes))
	children := make(map[string][]string)
	for _, node := range plan.Nodes {
		for _, dep := range node.DependsOn {
			if !known[dep] || dep == node.Key {
				return fmt.Errorf("invalid dependency %q -> %q", dep, node.Key)
			}
			indegree[node.Key]++
			children[dep] = append(children[dep], node.Key)
		}
	}
	queue := make([]string, 0)
	for key := range known {
		if indegree[key] == 0 {
			queue = append(queue, key)
		}
	}
	seen := 0
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		seen++
		for _, child := range children[key] {
			indegree[child]--
			if indegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	if seen != len(known) {
		return fmt.Errorf("workflow graph contains a cycle")
	}
	return nil
}

func newPGUUID() pgtype.UUID                { return util.MustParseUUID(uuid.NewString()) }
func textValue(value string) pgtype.Text    { return pgtype.Text{String: value, Valid: true} }
func nullableText(value string) pgtype.Text { return pgtype.Text{String: value, Valid: value != ""} }
func sameUUID(a, b pgtype.UUID) bool        { return a.Valid && b.Valid && a.Bytes == b.Bytes }
func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func normalizeResultPayload(value []byte) []byte {
	var object map[string]any
	if json.Unmarshal(value, &object) == nil {
		normalized, _ := json.Marshal(object)
		return normalized
	}
	normalized, _ := json.Marshal(map[string]any{"output": string(value)})
	return normalized
}
func marshalObject(value map[string]any) ([]byte, error) {
	if value == nil {
		value = map[string]any{}
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow object: %w", err)
	}
	return payload, nil
}
func defaultNodeKind(kind string) string {
	if kind == "" {
		return "agent_task"
	}
	return kind
}
func defaultVerifierKind(kind string) string {
	if kind == "" {
		return "system"
	}
	return kind
}
func defaultActorType(kind string) string {
	if kind == "agent" || kind == "member" {
		return kind
	}
	if kind == "human" {
		return "member"
	}
	return "system"
}
func withDefaultRetryPolicy(policy map[string]any) map[string]any {
	if policy == nil {
		policy = map[string]any{}
	}
	if _, ok := policy["max_attempts"]; !ok {
		policy["max_attempts"] = 1
	}
	return policy
}
func maxAttempts(payload []byte) int32 {
	var policy struct {
		MaxAttempts int32 `json:"max_attempts"`
	}
	if json.Unmarshal(payload, &policy) != nil || policy.MaxAttempts < 1 {
		return 1
	}
	return policy.MaxAttempts
}
func retryable(payload []byte, code string) bool {
	var policy struct {
		Codes []string `json:"retryable_failure_codes"`
	}
	if json.Unmarshal(payload, &policy) != nil {
		return false
	}
	for _, candidate := range policy.Codes {
		if candidate == code {
			return true
		}
	}
	return false
}
func independentVerification(payload []byte) bool {
	var policy struct {
		Independent bool `json:"independent"`
	}
	_ = json.Unmarshal(payload, &policy)
	return policy.Independent
}
func executionIDs(payload []byte) (pgtype.UUID, pgtype.UUID) {
	var spec struct {
		ExecutorID string `json:"agent_id"`
		RuntimeID  string `json:"runtime_id"`
	}
	if json.Unmarshal(payload, &spec) != nil {
		return pgtype.UUID{}, pgtype.UUID{}
	}
	executorID, _ := util.ParseUUID(spec.ExecutorID)
	runtimeID, _ := util.ParseUUID(spec.RuntimeID)
	return executorID, runtimeID
}
func truncateWorkflowDetail(value string) string {
	if len(value) <= 4096 {
		return value
	}
	return value[:4096]
}
func leaseFromAttempt(attempt db.WorkflowAttempt) WorkflowLease {
	return WorkflowLease{RunID: attempt.RunID, NodeID: attempt.NodeID, AttemptID: attempt.ID, AttemptNo: attempt.AttemptNo, FenceToken: attempt.FenceToken}
}
