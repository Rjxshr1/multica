package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type createWorkflowRunRequest struct {
	IdempotencyKey string               `json:"idempotency_key"`
	Plan           service.WorkflowPlan `json:"plan"`
	Policy         map[string]any       `json:"policy,omitempty"`
}

func (h *Handler) CreateWorkflowRun(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := util.ParseUUID(workspaceIDFromURL(r, "id"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_workspace", "invalid workspace id")
		return
	}
	member, ok := ctxMember(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "workspace membership required")
		return
	}
	var request createWorkflowRunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_contract", "invalid workflow request")
		return
	}
	if header := strings.TrimSpace(r.Header.Get("Idempotency-Key")); header != "" {
		request.IdempotencyKey = header
	}
	snapshot, err := h.WorkflowRuntime.CreateRun(r.Context(), service.CreateWorkflowRunInput{WorkspaceID: workspaceID, IdempotencyKey: request.IdempotencyKey, Plan: request.Plan, Policy: request.Policy, Actor: service.WorkflowActor{Type: "member", ID: member.UserID}})
	if err != nil {
		writeWorkflowError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, snapshot)
}

func (h *Handler) GetWorkflowRun(w http.ResponseWriter, r *http.Request) {
	workspaceID, runID, ok := workflowPathIDs(w, r)
	if !ok {
		return
	}
	snapshot, err := h.WorkflowRuntime.Snapshot(r.Context(), workspaceID, runID)
	if err != nil {
		writeWorkflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

type claimWorkflowNodeRequest struct {
	NodeKey        string `json:"node_key"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (h *Handler) ClaimWorkflowNode(w http.ResponseWriter, r *http.Request) {
	workspaceID, runID, ok := workflowPathIDs(w, r)
	if !ok {
		return
	}
	member, exists := ctxMember(r.Context())
	if !exists {
		writeError(w, http.StatusUnauthorized, "workspace membership required")
		return
	}
	var request claimWorkflowNodeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_contract", "invalid claim request")
		return
	}
	if header := strings.TrimSpace(r.Header.Get("Idempotency-Key")); header != "" {
		request.IdempotencyKey = header
	}
	if request.IdempotencyKey == "" {
		writeErrorCode(w, http.StatusBadRequest, "invalid_contract", "idempotency_key is required")
		return
	}
	lease, err := h.WorkflowRuntime.ClaimNode(r.Context(), workspaceID, runID, request.NodeKey, request.IdempotencyKey, service.WorkflowActor{Type: "member", ID: member.UserID})
	if err != nil {
		writeWorkflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

type bindWorkflowTaskRequest struct {
	TaskID string `json:"task_id"`
}

func (h *Handler) BindWorkflowTask(w http.ResponseWriter, r *http.Request) {
	workspaceID, runID, ok := workflowPathIDs(w, r)
	if !ok {
		return
	}
	attemptID, err := util.ParseUUID(chi.URLParam(r, "attemptId"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_contract", "invalid attempt id")
		return
	}
	var request bindWorkflowTaskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_contract", "invalid task binding")
		return
	}
	taskID, err := util.ParseUUID(request.TaskID)
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_contract", "invalid task id")
		return
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskID)
	if err != nil {
		writeWorkflowError(w, err)
		return
	}
	if h.TaskService.ResolveTaskWorkspaceID(r.Context(), task) != util.UUIDToString(workspaceID) {
		writeErrorCode(w, http.StatusForbidden, "workspace_mismatch", "task does not belong to this workspace")
		return
	}
	attempt, err := h.Queries.GetWorkflowAttempt(r.Context(), db.GetWorkflowAttemptParams{ID: attemptID, RunID: runID, WorkspaceID: workspaceID})
	if err != nil {
		writeWorkflowError(w, err)
		return
	}
	err = h.WorkflowRuntime.BindTask(r.Context(), workspaceID, service.WorkflowLease{RunID: runID, NodeID: attempt.NodeID, AttemptID: attempt.ID, AttemptNo: attempt.AttemptNo, FenceToken: attempt.FenceToken}, taskID)
	if err != nil {
		writeWorkflowError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type verifyWorkflowAttemptRequest struct {
	FenceToken     int64          `json:"fence_token"`
	Passed         bool           `json:"passed"`
	FailureCode    string         `json:"failure_code,omitempty"`
	Result         map[string]any `json:"result,omitempty"`
	IdempotencyKey string         `json:"idempotency_key"`
}

func (h *Handler) VerifyWorkflowAttempt(w http.ResponseWriter, r *http.Request) {
	workspaceID, runID, ok := workflowPathIDs(w, r)
	if !ok {
		return
	}
	attemptID, err := util.ParseUUID(chi.URLParam(r, "attemptId"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_contract", "invalid attempt id")
		return
	}
	member, exists := ctxMember(r.Context())
	if !exists {
		writeError(w, http.StatusUnauthorized, "workspace membership required")
		return
	}
	var request verifyWorkflowAttemptRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_contract", "invalid verification result")
		return
	}
	if header := strings.TrimSpace(r.Header.Get("Idempotency-Key")); header != "" {
		request.IdempotencyKey = header
	}
	snapshot, err := h.WorkflowRuntime.Verify(r.Context(), service.VerificationInput{WorkspaceID: workspaceID, RunID: runID, AttemptID: attemptID, FenceToken: request.FenceToken, Passed: request.Passed, FailureCode: request.FailureCode, Result: request.Result, VerifierKind: "human", VerifierID: member.UserID, IdempotencyKey: request.IdempotencyKey})
	if err != nil {
		writeWorkflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func workflowPathIDs(w http.ResponseWriter, r *http.Request) (workspaceID, runID pgtype.UUID, ok bool) {
	workspaceID, err := util.ParseUUID(workspaceIDFromURL(r, "id"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_workspace", "invalid workspace id")
		return workspaceID, runID, false
	}
	runID, err = util.ParseUUID(chi.URLParam(r, "runId"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_contract", "invalid run id")
		return workspaceID, runID, false
	}
	return workspaceID, runID, true
}

func writeWorkflowError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrWorkflowStaleRevision):
		writeErrorCode(w, http.StatusConflict, "stale_revision", err.Error())
	case errors.Is(err, service.ErrWorkflowStaleFence):
		writeErrorCode(w, http.StatusConflict, "stale_fence", err.Error())
	case errors.Is(err, service.ErrWorkflowInvalidState):
		writeErrorCode(w, http.StatusConflict, "invalid_state", err.Error())
	case errors.Is(err, pgx.ErrNoRows):
		writeErrorCode(w, http.StatusNotFound, "not_found", "workflow resource not found")
	default:
		writeErrorCode(w, http.StatusBadRequest, "invalid_contract", err.Error())
	}
}
