package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type contextPerformanceMetric struct {
	P50Microseconds int64 `json:"p50_us"`
	P95Microseconds int64 `json:"p95_us"`
	PayloadBytes    int   `json:"payload_bytes"`
}

// TestWorkflowContextPerformanceAB is an opt-in, real-PostgreSQL microbenchmark
// for the Context Gateway data plane. It deliberately does not claim to be
// model latency or production telemetry.
func TestWorkflowContextPerformanceAB(t *testing.T) {
	if os.Getenv("WORKFLOW_CONTEXT_PERF") != "1" {
		t.Skip("set WORKFLOW_CONTEXT_PERF=1 to run the context performance A/B")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	queries := db.New(pool)
	workspaceID, runID, nodeID, attemptID, taskID, snapshotID := newPGUUID(), newPGUUID(), newPGUUID(), newPGUUID(), newPGUUID(), newPGUUID()
	if _, err := queries.CreateWorkflowContextSnapshot(ctx, db.CreateWorkflowContextSnapshotParams{
		ID: snapshotID, WorkspaceID: workspaceID, RunID: runID, NodeID: nodeID, AttemptID: attemptID, TaskID: taskID,
		RunRevision: 42, Digest: "performance-snapshot", Manifest: []byte(`{"schema_version":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_context_item WHERE workspace_id=$1`, workspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_context_snapshot WHERE workspace_id=$1`, workspaceID)
	})
	for i := 0; i < 200; i++ {
		kind := "event"
		ref := fmt.Sprintf("event/%03d", i)
		title := fmt.Sprintf("Historical event %03d", i)
		body := strings.Repeat(fmt.Sprintf("irrelevant-%03d ", i), 260)
		if i == 0 {
			kind, ref, title, body = "task", "task/current", "Current task contract", strings.Repeat("required task contract ", 100)
		} else if i == 1 {
			kind, ref, title, body = "workflow", "workflow/overview", "Workflow snapshot", strings.Repeat("workflow policy and state ", 100)
		} else if i == 173 {
			body += " NEEDLE_APPROVAL_RULE review must pass before integration "
		}
		content, _ := json.Marshal(map[string]any{"text": body, "ordinal": i})
		if _, err := queries.CreateWorkflowContextItem(ctx, db.CreateWorkflowContextItemParams{
			ID: newPGUUID(), WorkspaceID: workspaceID, SnapshotID: snapshotID, Ordinal: int32(i), ReferenceKey: ref,
			Kind: kind, Title: title, Content: content, SearchText: ref + "\n" + title + "\n" + body,
			SourceType: kind, SourceDigest: digestBytes(content),
		}); err != nil {
			t.Fatal(err)
		}
	}
	runtime := NewWorkflowRuntimeService(queries, pool)
	const iterations = 120
	metrics := map[string]contextPerformanceMetric{}
	metrics["eager_full_context"] = measureContextOperation(t, iterations, func() any {
		items, err := queries.ListWorkflowContextItems(ctx, db.ListWorkflowContextItemsParams{SnapshotID: snapshotID, WorkspaceID: workspaceID})
		if err != nil {
			t.Fatal(err)
		}
		return items
	})
	metrics["catalog_only"] = measureContextOperation(t, iterations, func() any {
		catalog, err := runtime.ContextCatalog(ctx, workspaceID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return catalog
	})
	metrics["compact_l0"] = measureContextOperation(t, iterations, func() any {
		bootstrap, err := runtime.ContextBootstrap(ctx, workspaceID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return bootstrap
	})
	metrics["on_demand_search"] = measureContextOperation(t, iterations, func() any {
		items, err := runtime.SearchContext(ctx, workspaceID, taskID, WorkflowContextSearchInput{Query: "NEEDLE_APPROVAL_RULE", Kinds: []string{"event"}, Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Fatalf("search returned %d items, want 1", len(items))
		}
		return items
	})
	metrics["exact_get"] = measureContextOperation(t, iterations, func() any {
		item, err := runtime.GetContextItem(ctx, workspaceID, taskID, "event/173")
		if err != nil {
			t.Fatal(err)
		}
		return item
	})
	result := map[string]any{
		"dataset": map[string]any{"items": 200, "iterations": iterations, "database": "PostgreSQL", "classification": "local deterministic benchmark, not production telemetry"},
		"metrics": metrics,
	}
	raw, _ := json.Marshal(result)
	fmt.Printf("CONTEXT_BENCHMARK_JSON=%s\n", raw)
}

func TestContextCatalogBuildsFromSummaryRows(t *testing.T) {
	snapshot := db.WorkflowContextSnapshot{
		ID: newPGUUID(), RunID: newPGUUID(), NodeID: newPGUUID(), AttemptID: newPGUUID(),
		RunRevision: 7, Digest: "snapshot-digest",
	}
	sourceID := newPGUUID()
	catalog := contextCatalog(snapshot, []db.ListWorkflowContextItemSummariesRow{{
		ReferenceKey: "event/42", Kind: "event", Title: "verification passed",
		SourceType: "workflow_event", SourceID: sourceID, SourceDigest: "item-digest",
	}})
	if len(catalog.Items) != 1 {
		t.Fatalf("catalog items = %d, want 1", len(catalog.Items))
	}
	item := catalog.Items[0]
	if item.ReferenceKey != "event/42" || item.SourceID != util.UUIDToString(sourceID) || item.SourceDigest != "item-digest" {
		t.Fatalf("catalog item = %+v", item)
	}
}

func measureContextOperation(t *testing.T, iterations int, operation func() any) contextPerformanceMetric {
	t.Helper()
	durations := make([]time.Duration, 0, iterations)
	var payloadBytes int
	for i := 0; i < iterations; i++ {
		started := time.Now()
		value := operation()
		durations = append(durations, time.Since(started))
		if i == 0 {
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			payloadBytes = len(raw)
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return contextPerformanceMetric{
		P50Microseconds: durations[len(durations)/2].Microseconds(),
		P95Microseconds: durations[(len(durations)*95)/100].Microseconds(),
		PayloadBytes:    payloadBytes,
	}
}
