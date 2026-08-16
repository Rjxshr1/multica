package handler

import (
	"net/http/httptest"
	"testing"
)

func TestWorkflowContextScopeRequiresServerStampedTaskToken(t *testing.T) {
	h := &Handler{}
	tests := []struct {
		name        string
		actorSource string
		taskID      string
		workspaceID string
		wantOK      bool
		wantStatus  int
	}{
		{name: "member cannot forge task id", taskID: "11111111-1111-4111-8111-111111111111", workspaceID: "22222222-2222-4222-8222-222222222222", wantStatus: 403},
		{name: "task token is accepted", actorSource: "task_token", taskID: "11111111-1111-4111-8111-111111111111", workspaceID: "22222222-2222-4222-8222-222222222222", wantOK: true},
		{name: "task token without task scope fails", actorSource: "task_token", workspaceID: "22222222-2222-4222-8222-222222222222", wantStatus: 400},
		{name: "task token without workspace scope fails", actorSource: "task_token", taskID: "11111111-1111-4111-8111-111111111111", wantStatus: 400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/workflow-context/catalog", nil)
			req.Header.Set("X-Actor-Source", test.actorSource)
			req.Header.Set("X-Task-ID", test.taskID)
			req.Header.Set("X-Workspace-ID", test.workspaceID)
			w := httptest.NewRecorder()
			_, _, ok := h.workflowContextScope(w, req)
			if ok != test.wantOK {
				t.Fatalf("ok = %v, want %v; response=%s", ok, test.wantOK, w.Body.String())
			}
			if !test.wantOK && w.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, test.wantStatus)
			}
		})
	}
}

func TestWorkflowContextMCPProviderGate(t *testing.T) {
	for _, provider := range []string{"codebuddy", "qoder", "qoderclicn", "qwen", "qwenpaw"} {
		if !workflowContextMCPSupported(provider) {
			t.Errorf("%s should receive the Context MCP", provider)
		}
	}
	for _, provider := range []string{"", "workflow_e2e", "workbuddy"} {
		if workflowContextMCPSupported(provider) {
			t.Errorf("unsupported provider %q received the Context MCP", provider)
		}
	}
}
