package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var contextCmd = &cobra.Command{
	Use:   "context",
	Short: "Query task-scoped Workflow context",
}

var contextMCPCmd = &cobra.Command{
	Use:    "mcp",
	Short:  "Run the task-scoped Context Gateway over MCP stdio",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE:   runContextMCP,
}

type contextMCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type contextMCPResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *contextMCPError `json:"error,omitempty"`
}

type contextMCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func init() {
	contextCmd.AddCommand(contextMCPCmd)
}

func runContextMCP(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	reader := bufio.NewScanner(os.Stdin)
	reader.Buffer(make([]byte, 64<<10), 1<<20)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()
	for reader.Scan() {
		line := reader.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var request contextMCPRequest
		if err := json.Unmarshal(line, &request); err != nil || request.JSONRPC != "2.0" || request.Method == "" {
			if writeErr := writeContextMCPResponse(writer, contextMCPResponse{JSONRPC: "2.0", ID: request.ID, Error: &contextMCPError{Code: -32600, Message: "Invalid Request"}}); writeErr != nil {
				return writeErr
			}
			continue
		}
		// Notifications deliberately produce no response.
		if len(request.ID) == 0 {
			continue
		}
		response := handleContextMCPRequest(cmd.Context(), client, request)
		if err := writeContextMCPResponse(writer, response); err != nil {
			return err
		}
	}
	if err := reader.Err(); err != nil {
		return fmt.Errorf("read MCP request: %w", err)
	}
	return nil
}

func handleContextMCPRequest(parent context.Context, client *cli.APIClient, request contextMCPRequest) contextMCPResponse {
	response := contextMCPResponse{JSONRPC: "2.0", ID: request.ID}
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = "2025-03-26"
		}
		response.Result = map[string]any{
			"protocolVersion": params.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "multica-context", "version": "1"},
		}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": contextMCPTools()}
	case "tools/call":
		result, rpcErr := callContextMCPTool(parent, client, request.Params)
		if rpcErr != nil {
			response.Error = rpcErr
		} else {
			response.Result = result
		}
	default:
		response.Error = &contextMCPError{Code: -32601, Message: "Method not found"}
	}
	return response
}

func contextMCPTools() []map[string]any {
	return []map[string]any{
		{
			"name": "context_catalog", "description": "List the immutable context references available to this Workflow Attempt without loading their contents.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		},
		{
			"name": "context_search", "description": "Search this Attempt's immutable context snapshot. Use kinds to narrow to dependency, attempt, verification, event, artifact, task, or workflow.",
			"inputSchema": map[string]any{
				"type": "object", "properties": map[string]any{
					"query": map[string]any{"type": "string"},
					"kinds": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 12},
				}, "required": []string{"query"}, "additionalProperties": false,
			},
		},
		{
			"name": "context_get", "description": "Read one context item by the exact reference_key returned by context_catalog or context_search.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"reference_key": map[string]any{"type": "string"}}, "required": []string{"reference_key"}, "additionalProperties": false},
		},
		{
			"name": "context_dependency", "description": "Read the frozen output and status of one direct dependency node.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"node_key": map[string]any{"type": "string"}}, "required": []string{"node_key"}, "additionalProperties": false},
		},
	}
}

func callContextMCPTool(parent context.Context, client *cli.APIClient, raw json.RawMessage) (any, *contextMCPError) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil || params.Name == "" {
		return nil, &contextMCPError{Code: -32602, Message: "Invalid tool arguments"}
	}
	ctx, cancel := cli.APIContext(parent)
	defer cancel()
	var payload any
	var err error
	switch params.Name {
	case "context_catalog":
		var catalog map[string]any
		err = client.GetJSON(ctx, "/api/workflow-context/catalog", &catalog)
		payload = catalog
	case "context_search":
		var input struct {
			Query string   `json:"query"`
			Kinds []string `json:"kinds,omitempty"`
			Limit int      `json:"limit,omitempty"`
		}
		if json.Unmarshal(params.Arguments, &input) != nil || strings.TrimSpace(input.Query) == "" {
			return nil, &contextMCPError{Code: -32602, Message: "query is required"}
		}
		var result map[string]any
		err = client.PostJSON(ctx, "/api/workflow-context/search", input, &result)
		payload = result
	case "context_get", "context_dependency":
		var input struct {
			ReferenceKey string `json:"reference_key"`
			NodeKey      string `json:"node_key"`
		}
		if json.Unmarshal(params.Arguments, &input) != nil {
			return nil, &contextMCPError{Code: -32602, Message: "Invalid tool arguments"}
		}
		if params.Name == "context_dependency" {
			if strings.TrimSpace(input.NodeKey) == "" {
				return nil, &contextMCPError{Code: -32602, Message: "node_key is required"}
			}
			input.ReferenceKey = "dependency/" + strings.TrimSpace(input.NodeKey)
		}
		if strings.TrimSpace(input.ReferenceKey) == "" {
			return nil, &contextMCPError{Code: -32602, Message: "reference_key is required"}
		}
		var item map[string]any
		err = client.PostJSON(ctx, "/api/workflow-context/item", map[string]string{"reference_key": input.ReferenceKey}, &item)
		payload = item
	default:
		return nil, &contextMCPError{Code: -32602, Message: "Unknown context tool"}
	}
	if err != nil {
		return nil, &contextMCPError{Code: -32000, Message: "Context Gateway request failed: " + err.Error()}
	}
	textPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, &contextMCPError{Code: -32603, Message: "Context result encoding failed"}
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(textPayload)}},
		"structuredContent": payload,
		"isError":           false,
	}, nil
}

func writeContextMCPResponse(writer *bufio.Writer, response contextMCPResponse) error {
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if _, err := writer.Write(append(raw, '\n')); err != nil {
		return err
	}
	return writer.Flush()
}
