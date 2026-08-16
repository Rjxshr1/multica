package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	maxWorkflowContextItemBytes = 16 << 10
	maxWorkflowContextResults   = 12
)

var workflowContextKinds = map[string]bool{
	"task": true, "workflow": true, "dependency": true, "attempt": true,
	"verification": true, "event": true, "artifact": true,
}

var workflowContextLikeEscaper = strings.NewReplacer(
	`\`, `\\`,
	`%`, `\%`,
	`_`, `\_`,
)

type WorkflowContextCatalog struct {
	SnapshotID      string                       `json:"snapshot_id"`
	RunID           string                       `json:"run_id"`
	NodeID          string                       `json:"node_id"`
	AttemptID       string                       `json:"attempt_id"`
	ContextRevision int64                        `json:"context_revision"`
	SourceDigest    string                       `json:"source_digest"`
	Items           []WorkflowContextItemSummary `json:"items"`
}

type WorkflowContextBootstrap struct {
	SnapshotID      string          `json:"snapshot_id"`
	ContextRevision int64           `json:"context_revision"`
	SourceDigest    string          `json:"source_digest"`
	Task            json.RawMessage `json:"task"`
	Workflow        json.RawMessage `json:"workflow"`
}

type WorkflowContextItemSummary struct {
	ReferenceKey string `json:"reference_key"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	SourceType   string `json:"source_type"`
	SourceID     string `json:"source_id,omitempty"`
	SourceDigest string `json:"source_digest"`
}

type WorkflowContextItemView struct {
	WorkflowContextItemSummary
	Content json.RawMessage `json:"content"`
}

type WorkflowContextSearchInput struct {
	Query string   `json:"query"`
	Kinds []string `json:"kinds,omitempty"`
	Limit int32    `json:"limit,omitempty"`
}

type workflowContextDraft struct {
	ReferenceKey string
	Kind         string
	Title        string
	Content      []byte
	SourceType   string
	SourceID     pgtype.UUID
	SourceDigest string
}

// createContextSnapshotTx freezes the minimum required context plus a
// searchable catalog at task bind time. The source tables may continue to
// evolve, but this Attempt always reads these immutable rows.
func (s *WorkflowRuntimeService) createContextSnapshotTx(ctx context.Context, qtx *db.Queries, attempt db.WorkflowAttempt, taskID pgtype.UUID) error {
	run, err := qtx.GetWorkflowRun(ctx, db.GetWorkflowRunParams{ID: attempt.RunID, WorkspaceID: attempt.WorkspaceID})
	if err != nil {
		return err
	}
	node, err := qtx.GetWorkflowNodeForUpdate(ctx, db.GetWorkflowNodeForUpdateParams{ID: attempt.NodeID, RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID})
	if err != nil {
		return err
	}
	nodes, err := qtx.ListWorkflowNodes(ctx, db.ListWorkflowNodesParams{RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID})
	if err != nil {
		return err
	}
	dependencies, err := qtx.ListWorkflowDependencies(ctx, db.ListWorkflowDependenciesParams{RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID})
	if err != nil {
		return err
	}
	attempts, err := qtx.ListWorkflowAttempts(ctx, db.ListWorkflowAttemptsParams{RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID})
	if err != nil {
		return err
	}
	verifications, err := qtx.ListWorkflowVerifications(ctx, db.ListWorkflowVerificationsParams{RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID})
	if err != nil {
		return err
	}
	events, err := qtx.ListWorkflowEvents(ctx, db.ListWorkflowEventsParams{RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID})
	if err != nil {
		return err
	}
	artifacts, err := qtx.ListWorkflowArtifacts(ctx, db.ListWorkflowArtifactsParams{RunID: attempt.RunID, WorkspaceID: attempt.WorkspaceID})
	if err != nil {
		return err
	}

	drafts := make([]workflowContextDraft, 0, 8)
	add := func(ref, kind, title, sourceType string, sourceID pgtype.UUID, value any) {
		content := marshalWorkflowContextValue(value)
		drafts = append(drafts, workflowContextDraft{
			ReferenceKey: ref, Kind: kind, Title: title, Content: content,
			SourceType: sourceType, SourceID: sourceID, SourceDigest: digestBytes(content),
		})
	}
	add("task/current", "task", "Current task contract", "workflow_node", node.ID, map[string]any{
		"node_key": node.NodeKey, "node_kind": node.NodeKind, "status": node.Status,
		"input": json.RawMessage(node.InputSpec), "executor": json.RawMessage(node.ExecutorSpec),
		"retry_policy": json.RawMessage(node.RetryPolicy), "verification_policy": json.RawMessage(node.VerificationPolicy),
		"attempt_no": attempt.AttemptNo, "fence_token": attempt.FenceToken,
	})
	add("workflow/overview", "workflow", "Workflow snapshot", "workflow_run", run.ID, map[string]any{
		"definition_key": run.DefinitionKey, "definition_version": run.DefinitionVersion,
		"definition_digest": run.DefinitionDigest, "status": run.Status, "revision": run.Revision,
		"policy": json.RawMessage(run.PolicySnapshot),
	})

	nodeByID := make(map[string]db.WorkflowNodeExecution, len(nodes))
	predecessorIDs := make(map[string]bool)
	for _, candidate := range nodes {
		nodeByID[util.UUIDToString(candidate.ID)] = candidate
	}
	for _, edge := range dependencies {
		if sameUUID(edge.SuccessorNodeID, node.ID) {
			predecessorIDs[util.UUIDToString(edge.PredecessorNodeID)] = true
		}
	}
	predecessors := make([]db.WorkflowNodeExecution, 0, len(predecessorIDs))
	for id := range predecessorIDs {
		if predecessor, ok := nodeByID[id]; ok {
			predecessors = append(predecessors, predecessor)
		}
	}
	sort.Slice(predecessors, func(i, j int) bool { return predecessors[i].NodeKey < predecessors[j].NodeKey })
	for _, predecessor := range predecessors {
		var latest *db.WorkflowAttempt
		for i := range attempts {
			if sameUUID(attempts[i].NodeID, predecessor.ID) && (latest == nil || attempts[i].AttemptNo > latest.AttemptNo) {
				candidate := attempts[i]
				latest = &candidate
			}
		}
		content := map[string]any{"node_key": predecessor.NodeKey, "status": predecessor.Status}
		if latest != nil {
			content["attempt_no"] = latest.AttemptNo
			content["result"] = rawJSONObject(latest.ResultPayload)
			content["result_digest"] = latest.ResultDigest.String
			content["failure_code"] = latest.FailureCode.String
		}
		add("dependency/"+predecessor.NodeKey, "dependency", "Dependency: "+predecessor.NodeKey, "workflow_node", predecessor.ID, content)
	}

	for _, prior := range attempts {
		if !sameUUID(prior.NodeID, node.ID) || sameUUID(prior.ID, attempt.ID) {
			continue
		}
		add(fmt.Sprintf("attempt/%s/%d", node.NodeKey, prior.AttemptNo), "attempt", fmt.Sprintf("Prior attempt %d for %s", prior.AttemptNo, node.NodeKey), "workflow_attempt", prior.ID, map[string]any{
			"status": prior.Status, "result": rawJSONObject(prior.ResultPayload), "result_digest": prior.ResultDigest.String,
			"failure_code": prior.FailureCode.String, "failure_detail": prior.FailureDetail.String,
		})
		for _, verification := range verifications {
			if sameUUID(verification.AttemptID, prior.ID) {
				add("verification/"+util.UUIDToString(verification.ID), "verification", fmt.Sprintf("Verification for attempt %d", prior.AttemptNo), "workflow_verification", verification.ID, map[string]any{
					"status": verification.Status, "failure_code": verification.FailureCode.String,
					"verifier_kind": verification.VerifierKind, "result": rawJSONObject(verification.Result),
				})
			}
		}
	}

	relevantNodes := map[string]bool{util.UUIDToString(node.ID): true}
	for id := range predecessorIDs {
		relevantNodes[id] = true
	}
	start := 0
	if len(events) > 100 {
		start = len(events) - 100
	}
	for _, event := range events[start:] {
		if event.AggregateType != "run" && !relevantNodes[util.UUIDToString(event.AggregateID)] && !relevantNodes[contextEventNodeID(event, attempts)] {
			continue
		}
		add(fmt.Sprintf("event/%d", event.Sequence), "event", event.EventType, "workflow_event", event.ID, map[string]any{
			"sequence": event.Sequence, "event_type": event.EventType, "from_state": event.FromState.String,
			"to_state": event.ToState.String, "payload": rawJSONObject(event.Payload),
		})
	}
	for _, artifact := range artifacts {
		if !relevantNodes[util.UUIDToString(artifact.NodeID)] {
			continue
		}
		add("artifact/"+util.UUIDToString(artifact.ID), "artifact", artifact.Kind, "workflow_artifact", artifact.ID, map[string]any{
			"kind": artifact.Kind, "uri": artifact.Uri, "digest": artifact.Digest,
			"manifest": rawJSONObject(artifact.Manifest),
		})
	}

	manifestItems := make([]WorkflowContextItemSummary, 0, len(drafts))
	for _, draft := range drafts {
		manifestItems = append(manifestItems, contextDraftSummary(draft))
	}
	manifest, _ := json.Marshal(map[string]any{
		"schema_version":  1,
		"default_context": []string{"task/current", "workflow/overview"},
		"query_policy":    map[string]any{"max_results": maxWorkflowContextResults, "read_only": true},
		"items":           manifestItems,
	})
	digestInput, _ := json.Marshal(map[string]any{"run_revision": run.Revision, "manifest": json.RawMessage(manifest), "items": drafts})
	snapshotID := newPGUUID()
	if _, err := qtx.CreateWorkflowContextSnapshot(ctx, db.CreateWorkflowContextSnapshotParams{
		ID: snapshotID, WorkspaceID: attempt.WorkspaceID, RunID: attempt.RunID, NodeID: node.ID,
		AttemptID: attempt.ID, TaskID: taskID, RunRevision: run.Revision,
		Digest: digestBytes(digestInput), Manifest: manifest,
	}); err != nil {
		return fmt.Errorf("create workflow context snapshot: %w", err)
	}
	for ordinal, draft := range drafts {
		if _, err := qtx.CreateWorkflowContextItem(ctx, db.CreateWorkflowContextItemParams{
			ID: newPGUUID(), WorkspaceID: attempt.WorkspaceID, SnapshotID: snapshotID, Ordinal: int32(ordinal),
			ReferenceKey: draft.ReferenceKey, Kind: draft.Kind, Title: draft.Title, Content: draft.Content,
			SearchText: strings.Join([]string{draft.ReferenceKey, draft.Kind, draft.Title, string(draft.Content)}, "\n"),
			SourceType: draft.SourceType, SourceID: draft.SourceID, SourceDigest: draft.SourceDigest,
		}); err != nil {
			return fmt.Errorf("create workflow context item %q: %w", draft.ReferenceKey, err)
		}
	}
	return nil
}

func (s *WorkflowRuntimeService) ContextCatalog(ctx context.Context, workspaceID, taskID pgtype.UUID) (*WorkflowContextCatalog, error) {
	snapshot, err := s.Queries.GetWorkflowContextSnapshotByTask(ctx, db.GetWorkflowContextSnapshotByTaskParams{TaskID: taskID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	items, err := s.Queries.ListWorkflowContextItemSummaries(ctx, db.ListWorkflowContextItemSummariesParams{SnapshotID: snapshot.ID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	return contextCatalog(snapshot, items), nil
}

func (s *WorkflowRuntimeService) ContextBootstrap(ctx context.Context, workspaceID, taskID pgtype.UUID) (*WorkflowContextBootstrap, error) {
	snapshot, err := s.Queries.GetWorkflowContextSnapshotByTask(ctx, db.GetWorkflowContextSnapshotByTaskParams{TaskID: taskID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	items, err := s.Queries.GetWorkflowContextBootstrapItems(ctx, db.GetWorkflowContextBootstrapItemsParams{SnapshotID: snapshot.ID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	bootstrap := &WorkflowContextBootstrap{
		SnapshotID: util.UUIDToString(snapshot.ID), ContextRevision: snapshot.RunRevision, SourceDigest: snapshot.Digest,
		Task: json.RawMessage(`{}`), Workflow: json.RawMessage(`{}`),
	}
	for _, item := range items {
		switch item.ReferenceKey {
		case "task/current":
			bootstrap.Task = json.RawMessage(item.Content)
		case "workflow/overview":
			bootstrap.Workflow = json.RawMessage(item.Content)
		}
	}
	return bootstrap, nil
}

func (s *WorkflowRuntimeService) SearchContext(ctx context.Context, workspaceID, taskID pgtype.UUID, input WorkflowContextSearchInput) ([]WorkflowContextItemView, error) {
	query := strings.TrimSpace(input.Query)
	snapshot, err := s.Queries.GetWorkflowContextSnapshotByTask(ctx, db.GetWorkflowContextSnapshotByTaskParams{TaskID: taskID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > maxWorkflowContextResults {
		limit = maxWorkflowContextResults
	}
	kinds := make([]string, 0, len(input.Kinds))
	for _, kind := range input.Kinds {
		kind = strings.TrimSpace(kind)
		if !workflowContextKinds[kind] {
			return nil, fmt.Errorf("unsupported context kind %q", kind)
		}
		kinds = append(kinds, kind)
	}
	var items []db.WorkflowContextItem
	if query == "" {
		items, err = s.Queries.ListWorkflowContextItemsForEmptySearch(ctx, db.ListWorkflowContextItemsForEmptySearchParams{
			SnapshotID: snapshot.ID, WorkspaceID: workspaceID, Kinds: kinds, ResultLimit: limit,
		})
	} else {
		items, err = s.Queries.SearchWorkflowContextItems(ctx, db.SearchWorkflowContextItemsParams{
			SnapshotID: snapshot.ID, WorkspaceID: workspaceID, Query: query,
			QueryPattern: workflowContextLikePattern(query), Kinds: kinds, ResultLimit: limit,
		})
	}
	if err != nil {
		return nil, err
	}
	return contextItemViews(items), nil
}

func workflowContextLikePattern(query string) string {
	return "%" + workflowContextLikeEscaper.Replace(query) + "%"
}

func (s *WorkflowRuntimeService) GetContextItem(ctx context.Context, workspaceID, taskID pgtype.UUID, referenceKey string) (*WorkflowContextItemView, error) {
	snapshot, err := s.Queries.GetWorkflowContextSnapshotByTask(ctx, db.GetWorkflowContextSnapshotByTaskParams{TaskID: taskID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	item, err := s.Queries.GetWorkflowContextItemByReference(ctx, db.GetWorkflowContextItemByReferenceParams{
		SnapshotID: snapshot.ID, WorkspaceID: workspaceID, ReferenceKey: strings.TrimSpace(referenceKey),
	})
	if err != nil {
		return nil, err
	}
	view := contextItemView(item)
	return &view, nil
}

func contextCatalog(snapshot db.WorkflowContextSnapshot, items []db.ListWorkflowContextItemSummariesRow) *WorkflowContextCatalog {
	catalog := &WorkflowContextCatalog{
		SnapshotID: util.UUIDToString(snapshot.ID), RunID: util.UUIDToString(snapshot.RunID), NodeID: util.UUIDToString(snapshot.NodeID),
		AttemptID: util.UUIDToString(snapshot.AttemptID), ContextRevision: snapshot.RunRevision, SourceDigest: snapshot.Digest,
		Items: make([]WorkflowContextItemSummary, 0, len(items)),
	}
	for _, item := range items {
		catalog.Items = append(catalog.Items, WorkflowContextItemSummary{
			ReferenceKey: item.ReferenceKey, Kind: item.Kind, Title: item.Title,
			SourceType: item.SourceType, SourceID: util.UUIDToString(item.SourceID), SourceDigest: item.SourceDigest,
		})
	}
	return catalog
}

func contextItemViews(items []db.WorkflowContextItem) []WorkflowContextItemView {
	views := make([]WorkflowContextItemView, 0, len(items))
	for _, item := range items {
		views = append(views, contextItemView(item))
	}
	return views
}

func contextItemView(item db.WorkflowContextItem) WorkflowContextItemView {
	return WorkflowContextItemView{WorkflowContextItemSummary: contextItemSummary(item), Content: json.RawMessage(item.Content)}
}

func contextItemSummary(item db.WorkflowContextItem) WorkflowContextItemSummary {
	return WorkflowContextItemSummary{ReferenceKey: item.ReferenceKey, Kind: item.Kind, Title: item.Title, SourceType: item.SourceType, SourceID: util.UUIDToString(item.SourceID), SourceDigest: item.SourceDigest}
}

func contextDraftSummary(item workflowContextDraft) WorkflowContextItemSummary {
	return WorkflowContextItemSummary{ReferenceKey: item.ReferenceKey, Kind: item.Kind, Title: item.Title, SourceType: item.SourceType, SourceID: util.UUIDToString(item.SourceID), SourceDigest: item.SourceDigest}
}

func marshalWorkflowContextValue(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		raw = []byte(`{"error":"context source could not be encoded"}`)
	}
	if len(raw) <= maxWorkflowContextItemBytes {
		return raw
	}
	digest := digestBytes(raw)
	preview := string(raw[:maxWorkflowContextItemBytes/2])
	truncated, _ := json.Marshal(map[string]any{"truncated": true, "original_digest": digest, "preview": preview})
	return truncated
}

func rawJSONObject(raw []byte) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(raw)
}

func contextEventNodeID(event db.WorkflowEvent, attempts []db.WorkflowAttempt) string {
	if !event.AttemptID.Valid {
		return ""
	}
	for _, attempt := range attempts {
		if sameUUID(attempt.ID, event.AttemptID) {
			return util.UUIDToString(attempt.NodeID)
		}
	}
	return ""
}
