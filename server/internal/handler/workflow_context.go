package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// workflowContextScope accepts only a task token and returns the immutable
// task/workspace scope stamped by auth middleware. Member credentials and
// forged X-Task-ID headers fail closed.
func (h *Handler) workflowContextScope(w http.ResponseWriter, r *http.Request) (pgtype.UUID, pgtype.UUID, bool) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "workflow context is only available from within its agent task")
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	taskID, err := util.ParseUUID(strings.TrimSpace(r.Header.Get("X-Task-ID")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task context")
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	workspaceID, err := util.ParseUUID(strings.TrimSpace(r.Header.Get("X-Workspace-ID")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workspace context")
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	return workspaceID, taskID, true
}

func (h *Handler) GetWorkflowContextCatalog(w http.ResponseWriter, r *http.Request) {
	workspaceID, taskID, ok := h.workflowContextScope(w, r)
	if !ok {
		return
	}
	catalog, err := h.WorkflowRuntime.ContextCatalog(r.Context(), workspaceID, taskID)
	if err != nil {
		h.writeWorkflowContextError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, catalog)
}

func (h *Handler) SearchWorkflowContext(w http.ResponseWriter, r *http.Request) {
	workspaceID, taskID, ok := h.workflowContextScope(w, r)
	if !ok {
		return
	}
	var input service.WorkflowContextSearchInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid context query")
		return
	}
	items, err := h.WorkflowRuntime.SearchContext(r.Context(), workspaceID, taskID, input)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported context kind") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.writeWorkflowContextError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) GetWorkflowContextItem(w http.ResponseWriter, r *http.Request) {
	workspaceID, taskID, ok := h.workflowContextScope(w, r)
	if !ok {
		return
	}
	var input struct {
		ReferenceKey string `json:"reference_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&input); err != nil || strings.TrimSpace(input.ReferenceKey) == "" {
		writeError(w, http.StatusBadRequest, "reference_key is required")
		return
	}
	item, err := h.WorkflowRuntime.GetContextItem(r.Context(), workspaceID, taskID, input.ReferenceKey)
	if err != nil {
		h.writeWorkflowContextError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *Handler) writeWorkflowContextError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "workflow context not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "failed to read workflow context")
}
