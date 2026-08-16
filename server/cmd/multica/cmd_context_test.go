package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestContextMCPListsReadOnlyTools(t *testing.T) {
	response := handleContextMCPRequest(context.Background(), nil, contextMCPRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"})
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	result := response.Result.(map[string]any)
	tools := result["tools"].([]map[string]any)
	want := map[string]bool{"context_catalog": true, "context_search": true, "context_get": true, "context_dependency": true}
	for _, tool := range tools {
		delete(want, tool["name"].(string))
	}
	if len(want) != 0 {
		t.Fatalf("missing Context MCP tools: %v", want)
	}
}

func TestContextMCPSearchUsesTaskScopedAPIClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workflow-context/search" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer mat_context_test" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Task-ID"); got != "11111111-1111-4111-8111-111111111111" {
			t.Fatalf("X-Task-ID = %q", got)
		}
		if got := r.Header.Get("X-Workspace-ID"); got != "22222222-2222-4222-8222-222222222222" {
			t.Fatalf("X-Workspace-ID = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"reference_key":"dependency/review","kind":"dependency"}]}`))
	}))
	defer server.Close()
	client := cli.NewAPIClient(server.URL, "22222222-2222-4222-8222-222222222222", "mat_context_test")
	client.TaskID = "11111111-1111-4111-8111-111111111111"
	params := json.RawMessage(`{"name":"context_search","arguments":{"query":"review","kinds":["dependency"],"limit":3}}`)
	response := handleContextMCPRequest(context.Background(), client, contextMCPRequest{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/call", Params: params})
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	result := response.Result.(map[string]any)
	if result["isError"] != false || result["structuredContent"] == nil {
		t.Fatalf("tool result = %#v", result)
	}
}
