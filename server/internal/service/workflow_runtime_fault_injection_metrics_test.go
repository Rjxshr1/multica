package service

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestWorkflowKernelFaultInjectionMetrics is an opt-in, PostgreSQL-backed
// measurement of the authoritative Workflow Runtime invariants. It exercises
// the real reducer, SQL CAS/fence predicates, transaction boundaries, event
// store, and durable outbox. Normal test runs skip it because it creates and
// deletes hundreds of isolated workflow runs.
//
// Required environment:
//
//	DATABASE_URL=<migrated PostgreSQL>
//	WORKFLOW_FAULT_METRICS_OUTPUT=/absolute/path/report.json
//	WORKFLOW_FAULT_ISOLATED_DATABASE=1
//
// Optional:
//
//	WORKFLOW_FAULT_METRICS_ITERATIONS=100
func TestWorkflowKernelFaultInjectionMetrics(t *testing.T) {
	outputPath := os.Getenv("WORKFLOW_FAULT_METRICS_OUTPUT")
	if outputPath == "" {
		t.Skip("WORKFLOW_FAULT_METRICS_OUTPUT is required for the opt-in metrics run")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required")
	}
	if os.Getenv("WORKFLOW_FAULT_ISOLATED_DATABASE") != "1" {
		t.Fatal("WORKFLOW_FAULT_ISOLATED_DATABASE=1 is required; this test leases outbox rows and must never target a shared database")
	}
	iterations := 100
	if raw := os.Getenv("WORKFLOW_FAULT_METRICS_ITERATIONS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			t.Fatalf("WORKFLOW_FAULT_METRICS_ITERATIONS must be a positive integer, got %q", raw)
		}
		iterations = parsed
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var databaseVersion string
	if err := pool.QueryRow(ctx, `SELECT version()`).Scan(&databaseVersion); err != nil {
		t.Fatal(err)
	}
	var tablesReady bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('workflow_run') IS NOT NULL`).Scan(&tablesReady); err != nil || !tablesReady {
		t.Fatal("workflow migrations are not applied")
	}
	var existingRuns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_run`).Scan(&existingRuns); err != nil {
		t.Fatal(err)
	}
	if existingRuns != 0 {
		t.Fatalf("fault-injection database must start empty, found %d workflow runs", existingRuns)
	}

	queries := db.New(pool)
	workflow := NewWorkflowRuntimeService(queries, pool)
	metrics := make([]workflowFaultMetric, 0, 7)
	metrics = append(metrics,
		measureCASUniqueWinner(t, ctx, workflow, queries, iterations),
		measureStaleAttemptFence(t, ctx, workflow, queries, iterations),
		measureTransactionRollback(t, ctx, pool, workflow, queries, iterations),
		measureOutboxRecovery(t, ctx, pool, workflow, queries, iterations),
		measureDependencyRelease(t, ctx, pool, workflow, queries, iterations),
		measureCancelCompleteRace(t, ctx, pool, workflow, queries, iterations),
	)

	totalSamples := 0
	for _, metric := range metrics {
		totalSamples += metric.SampleSize
		if metric.Passed != metric.SampleSize {
			t.Errorf("%s passed %d/%d", metric.ID, metric.Passed, metric.SampleSize)
		}
	}
	report := workflowFaultReport{
		SchemaVersion: "workflow-kernel-fault-injection.v1",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		Scope:         "local_postgresql_fault_injection",
		Source:        "real WorkflowRuntimeService + generated SQL queries + PostgreSQL transactions",
		SourceCommit:  os.Getenv("WORKFLOW_FAULT_SOURCE_COMMIT"),
		Iterations:    iterations,
		TotalSamples:  totalSamples,
		Environment: workflowFaultEnvironment{
			OS: runtime.GOOS, Architecture: runtime.GOARCH,
			GoVersion: runtime.Version(), DatabaseVersion: databaseVersion,
		},
		Semantics: workflowFaultSemantics{
			StateTransition: "exactly once by transaction, revision CAS, fence and unique idempotency keys",
			EventDelivery:   "at least once; consumers deduplicate by event_id",
		},
		Comparison: workflowFaultComparison{
			ExternalRepair: "detects and repairs known inconsistent states after multiple API writes",
			KernelRuntime:  "keeps authoritative node, attempt, event and outbox writes in one transaction; concurrency losers are rejected before a contradictory state commits",
		},
		Metrics: metrics,
	}
	if err := writeWorkflowFaultReport(outputPath, report); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote workflow fault-injection evidence to %s (%d samples)", outputPath, totalSamples)
}

type workflowFaultReport struct {
	SchemaVersion string                   `json:"schema_version"`
	GeneratedAt   string                   `json:"generated_at"`
	Scope         string                   `json:"scope"`
	Source        string                   `json:"source"`
	SourceCommit  string                   `json:"source_commit"`
	Iterations    int                      `json:"iterations_per_schedule"`
	TotalSamples  int                      `json:"total_injected_schedules"`
	Environment   workflowFaultEnvironment `json:"environment"`
	Semantics     workflowFaultSemantics   `json:"semantics"`
	Comparison    workflowFaultComparison  `json:"external_repair_vs_kernel"`
	Metrics       []workflowFaultMetric    `json:"metrics"`
}

type workflowFaultEnvironment struct {
	OS              string `json:"os"`
	Architecture    string `json:"architecture"`
	GoVersion       string `json:"go_version"`
	DatabaseVersion string `json:"database_version"`
}

type workflowFaultSemantics struct {
	StateTransition string `json:"state_transition"`
	EventDelivery   string `json:"event_delivery"`
}

type workflowFaultComparison struct {
	ExternalRepair string `json:"external_repair"`
	KernelRuntime  string `json:"kernel_runtime"`
}

type workflowFaultMetric struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Injected    string         `json:"injected_fault"`
	Denominator string         `json:"denominator"`
	SampleSize  int            `json:"sample_size"`
	Passed      int            `json:"passed"`
	RatePct     float64        `json:"rate_pct"`
	DurationMS  int64          `json:"duration_ms"`
	Details     map[string]int `json:"details,omitempty"`
}

func measureCASUniqueWinner(t *testing.T, ctx context.Context, workflow *WorkflowRuntimeService, queries *db.Queries, iterations int) workflowFaultMetric {
	t.Helper()
	started := time.Now()
	passed := 0
	for i := 0; i < iterations; i++ {
		workspaceID, runID := createMetricRun(t, ctx, workflow, queries, fmt.Sprintf("cas-%d", i), []WorkflowNodeSpec{{Key: "execute"}})
		start := make(chan struct{})
		errs := make(chan error, 2)
		for contender := 0; contender < 2; contender++ {
			go func(contender int) {
				<-start
				_, err := workflow.ClaimNode(ctx, workspaceID, runID, "execute", fmt.Sprintf("claim-%d-%d", i, contender), WorkflowActor{Type: "system"})
				errs <- err
			}(contender)
		}
		close(start)
		successes := 0
		for contender := 0; contender < 2; contender++ {
			if err := <-errs; err == nil {
				successes++
			}
		}
		snapshot, err := workflow.Snapshot(ctx, workspaceID, runID)
		if err != nil {
			t.Fatal(err)
		}
		claimedEvents := countEvent(snapshot.Events, "node.claimed")
		if successes == 1 && len(snapshot.Attempts) == 1 && snapshot.Nodes[0].AttemptCount == 1 && claimedEvents == 1 {
			passed++
		}
		cleanupMetricWorkspace(t, ctx, queries, workspaceID)
	}
	return newWorkflowFaultMetric("cas_unique_winner", "并发 CAS 唯一提交率", "two controllers claim the same ready node concurrently", "exactly one committed attempt and one node.claimed event", iterations, passed, started, nil)
}

func measureStaleAttemptFence(t *testing.T, ctx context.Context, workflow *WorkflowRuntimeService, queries *db.Queries, iterations int) workflowFaultMetric {
	t.Helper()
	started := time.Now()
	passed := 0
	for i := 0; i < iterations; i++ {
		workspaceID, runID := createMetricRun(t, ctx, workflow, queries, fmt.Sprintf("fence-%d", i), []WorkflowNodeSpec{{Key: "execute", RetryPolicy: map[string]any{"max_attempts": 2, "retryable_failure_codes": []string{"runtime_lost"}}}})
		first, err := workflow.ClaimNode(ctx, workspaceID, runID, "execute", fmt.Sprintf("first-%d", i), WorkflowActor{Type: "system"})
		if err != nil {
			t.Fatal(err)
		}
		firstTask := newPGUUID()
		bindSyntheticAttempt(t, ctx, queries, workspaceID, first, firstTask)
		if owned, err := workflow.SettleTaskFailure(ctx, firstTask, "runtime_lost", "fault injection"); err != nil || !owned {
			t.Fatalf("settle first failure: owned=%v err=%v", owned, err)
		}
		second, err := workflow.ClaimNode(ctx, workspaceID, runID, "execute", fmt.Sprintf("second-%d", i), WorkflowActor{Type: "system"})
		if err != nil {
			t.Fatal(err)
		}
		secondTask := newPGUUID()
		bindSyntheticAttempt(t, ctx, queries, workspaceID, second, secondTask)
		tx, err := workflow.begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		owned, staleErr := workflow.SettleTaskSuccessTx(ctx, queries.WithTx(tx), firstTask, []byte(`{"late":true}`))
		_ = tx.Rollback(ctx)
		snapshot, err := workflow.Snapshot(ctx, workspaceID, runID)
		if err != nil {
			t.Fatal(err)
		}
		if owned && errors.Is(staleErr, ErrWorkflowStaleFence) && len(snapshot.Attempts) == 2 && snapshot.Nodes[0].Status == "running" && sameUUID(snapshot.Nodes[0].ActiveAttemptID, second.AttemptID) {
			passed++
		}
		cleanupMetricWorkspace(t, ctx, queries, workspaceID)
	}
	return newWorkflowFaultMetric("stale_attempt_rejected", "旧 Attempt 迟到写入阻断率", "attempt-1 fails, attempt-2 starts, then attempt-1 submits a late result", "late attempt returns stale fence and cannot alter the active node", iterations, passed, started, nil)
}

func measureTransactionRollback(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workflow *WorkflowRuntimeService, queries *db.Queries, iterations int) workflowFaultMetric {
	t.Helper()
	started := time.Now()
	passed := 0
	boundaries := []string{"after_node_cas", "after_attempt_insert", "after_event_and_outbox_insert"}
	for i := 0; i < iterations; i++ {
		for _, boundary := range boundaries {
			workspaceID, runID := createMetricRun(t, ctx, workflow, queries, fmt.Sprintf("rollback-%s-%d", boundary, i), []WorkflowNodeSpec{{Key: "execute"}})
			before, err := workflow.Snapshot(ctx, workspaceID, runID)
			if err != nil {
				t.Fatal(err)
			}
			beforeOutbox := countOutbox(t, ctx, pool, runID)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			qtx := queries.WithTx(tx)
			if err := qtx.LockWorkflowRun(ctx, util.UUIDToString(runID)); err != nil {
				t.Fatal(err)
			}
			node, err := qtx.GetWorkflowNodeByKeyForUpdate(ctx, db.GetWorkflowNodeByKeyForUpdateParams{NodeKey: "execute", RunID: runID, WorkspaceID: workspaceID})
			if err != nil {
				t.Fatal(err)
			}
			attemptID := newPGUUID()
			claimed, err := qtx.ClaimWorkflowNode(ctx, db.ClaimWorkflowNodeParams{AttemptID: attemptID, ID: node.ID, RunID: runID, WorkspaceID: workspaceID, ExpectedRevision: node.Revision})
			if err != nil {
				t.Fatal(err)
			}
			if boundary != "after_node_cas" {
				if _, err := qtx.CreateWorkflowAttempt(ctx, db.CreateWorkflowAttemptParams{ID: attemptID, WorkspaceID: workspaceID, RunID: runID, NodeID: node.ID, AttemptNo: claimed.AttemptCount, FenceToken: claimed.FenceToken}); err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "after_event_and_outbox_insert" {
				if err := workflow.appendEvent(ctx, qtx, eventInput{WorkspaceID: workspaceID, RunID: runID, AggregateType: "node", AggregateID: node.ID, EventType: "node.claimed", FromState: "ready", ToState: "running", AggregateRevision: claimed.Revision, Actor: WorkflowActor{Type: "system"}, AttemptID: attemptID, IdempotencyKey: fmt.Sprintf("rollback-%s-%d:fault", boundary, i)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			after, err := workflow.Snapshot(ctx, workspaceID, runID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Nodes[0].Status == "ready" && after.Nodes[0].AttemptCount == 0 && len(after.Attempts) == 0 && len(after.Events) == len(before.Events) && countOutbox(t, ctx, pool, runID) == beforeOutbox {
				passed++
			}
			cleanupMetricWorkspace(t, ctx, queries, workspaceID)
		}
	}
	samples := iterations * len(boundaries)
	return newWorkflowFaultMetric("transaction_rollback_invariant", "提交中断原子一致性保持率", "rollback at three real transaction boundaries: after node CAS, attempt insert, and event/outbox insert", "no partial node, attempt, event or outbox write remains", samples, passed, started, map[string]int{"boundaries": len(boundaries), "iterations_per_boundary": iterations})
}

func measureOutboxRecovery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workflow *WorkflowRuntimeService, queries *db.Queries, iterations int) workflowFaultMetric {
	t.Helper()
	started := time.Now()
	passed := 0
	rawDeliveries := 0
	uniqueDeliveries := 0
	for i := 0; i < iterations; i++ {
		workspaceID, runID := createMetricRun(t, ctx, workflow, queries, fmt.Sprintf("outbox-%d", i), []WorkflowNodeSpec{{Key: "execute"}})
		bus := events.New()
		seen := map[string]int{}
		bus.Subscribe(workflowOutboxEventType, func(event events.Event) {
			payload, _ := event.Payload.(map[string]any)
			if payload["run_id"] == util.UUIDToString(runID) {
				if eventID, ok := payload["event_id"].(string); ok {
					seen[eventID]++
				}
			}
		})
		leaseToken := newPGUUID()
		items, err := queries.LeaseWorkflowOutbox(ctx, db.LeaseWorkflowOutboxParams{LeaseToken: leaseToken, LeaseSeconds: 30, BatchSize: 64})
		if err != nil {
			t.Fatal(err)
		}
		targets := 0
		for _, item := range items {
			if !sameUUID(item.RunID, runID) {
				continue
			}
			targets++
			bus.Publish(events.Event{Type: workflowOutboxEventType, WorkspaceID: util.UUIDToString(item.WorkspaceID), ActorType: "system", Payload: map[string]any{"event_id": util.UUIDToString(item.EventID), "run_id": util.UUIDToString(item.RunID), "topic": item.Topic, "data": json.RawMessage(item.Payload)}})
		}
		if _, err := pool.Exec(ctx, `UPDATE workflow_outbox SET lease_expires_at = now() - interval '1 second' WHERE run_id = $1 AND status = 'leased'`, util.UUIDToString(runID)); err != nil {
			t.Fatal(err)
		}
		worker := NewWorkflowOutboxWorker(queries, bus)
		if _, err := worker.ProcessBatch(ctx, 64); err != nil {
			t.Fatal(err)
		}
		published := countOutboxStatus(t, ctx, pool, runID, "published")
		unique := len(seen)
		raw := 0
		for _, count := range seen {
			raw += count
		}
		rawDeliveries += raw
		uniqueDeliveries += unique
		if targets > 0 && published == targets && unique == targets && raw == targets*2 {
			passed++
		}
		cleanupMetricWorkspace(t, ctx, queries, workspaceID)
	}
	return newWorkflowFaultMetric("outbox_ack_loss_recovery", "Outbox 丢确认恢复率", "publish succeeds but acknowledgement is lost; lease expires and the worker retries", "all durable events become published and event_id dedupe exposes each event once", iterations, passed, started, map[string]int{"raw_at_least_once_deliveries": rawDeliveries, "unique_event_ids": uniqueDeliveries})
}

func measureDependencyRelease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workflow *WorkflowRuntimeService, queries *db.Queries, iterations int) workflowFaultMetric {
	t.Helper()
	started := time.Now()
	passed := 0
	for i := 0; i < iterations; i++ {
		workspaceID, runID := createMetricRun(t, ctx, workflow, queries, fmt.Sprintf("dependency-%d", i), []WorkflowNodeSpec{{Key: "a"}, {Key: "b"}, {Key: "c", DependsOn: []string{"a", "b"}}})
		leases := make([]WorkflowLease, 0, 2)
		for _, nodeKey := range []string{"a", "b"} {
			lease, err := workflow.ClaimNode(ctx, workspaceID, runID, nodeKey, fmt.Sprintf("claim-%s-%d", nodeKey, i), WorkflowActor{Type: "system"})
			if err != nil {
				t.Fatal(err)
			}
			taskID := newPGUUID()
			bindSyntheticAttempt(t, ctx, queries, workspaceID, lease, taskID)
			settleSuccess(t, ctx, pool, workflow, taskID, `{"ok":true}`)
			leases = append(leases, lease)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		for index, lease := range leases {
			go func(index int, lease WorkflowLease) {
				<-start
				_, err := workflow.Verify(ctx, VerificationInput{WorkspaceID: workspaceID, RunID: runID, AttemptID: lease.AttemptID, FenceToken: lease.FenceToken, Passed: true, VerifierKind: "system", IdempotencyKey: fmt.Sprintf("verify-%d-%d", i, index)})
				errs <- err
			}(index, lease)
		}
		close(start)
		bothSucceeded := true
		for index := 0; index < 2; index++ {
			if err := <-errs; err != nil {
				bothSucceeded = false
			}
		}
		snapshot, err := workflow.Snapshot(ctx, workspaceID, runID)
		if err != nil {
			t.Fatal(err)
		}
		cReady := false
		cReadyEvents := 0
		for _, node := range snapshot.Nodes {
			if node.NodeKey == "c" {
				cReady = node.Status == "ready" && node.Revision == 1
				for _, event := range snapshot.Events {
					if event.EventType == "node.ready" && sameUUID(event.AggregateID, node.ID) {
						cReadyEvents++
					}
				}
			}
		}
		if bothSucceeded && cReady && cReadyEvents == 1 {
			passed++
		}
		cleanupMetricWorkspace(t, ctx, queries, workspaceID)
	}
	return newWorkflowFaultMetric("dependency_release_once", "并发前驱依赖唯一释放率", "two predecessor verification passes race to release one successor", "successor enters ready exactly once with one node.ready event", iterations, passed, started, nil)
}

func measureCancelCompleteRace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workflow *WorkflowRuntimeService, queries *db.Queries, iterations int) workflowFaultMetric {
	t.Helper()
	started := time.Now()
	passed := 0
	outcomes := map[string]int{"cancelled": 0, "succeeded": 0}
	for i := 0; i < iterations; i++ {
		workspaceID, runID := createMetricRun(t, ctx, workflow, queries, fmt.Sprintf("terminal-race-%d", i), []WorkflowNodeSpec{{Key: "execute"}})
		lease, err := workflow.ClaimNode(ctx, workspaceID, runID, "execute", fmt.Sprintf("claim-%d", i), WorkflowActor{Type: "system"})
		if err != nil {
			t.Fatal(err)
		}
		taskID := newPGUUID()
		bindSyntheticAttempt(t, ctx, queries, workspaceID, lease, taskID)
		settleSuccess(t, ctx, pool, workflow, taskID, `{"ok":true}`)
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			_, err := workflow.Verify(ctx, VerificationInput{WorkspaceID: workspaceID, RunID: runID, AttemptID: lease.AttemptID, FenceToken: lease.FenceToken, Passed: true, VerifierKind: "system", IdempotencyKey: fmt.Sprintf("complete-%d", i)})
			errs <- err
		}()
		go func() {
			<-start
			errs <- cancelMetricRun(ctx, workflow, workspaceID, runID, fmt.Sprintf("cancel-%d", i))
		}()
		close(start)
		successes := 0
		for contender := 0; contender < 2; contender++ {
			if err := <-errs; err == nil {
				successes++
			}
		}
		snapshot, err := workflow.Snapshot(ctx, workspaceID, runID)
		if err != nil {
			t.Fatal(err)
		}
		terminalEvents := countEvent(snapshot.Events, "workflow.succeeded") + countEvent(snapshot.Events, "workflow.cancelled")
		if successes == 1 && terminalEvents == 1 && (snapshot.Run.Status == "succeeded" || snapshot.Run.Status == "cancelled") {
			passed++
			outcomes[snapshot.Run.Status]++
		}
		cleanupMetricWorkspace(t, ctx, queries, workspaceID)
	}
	return newWorkflowFaultMetric("cancel_complete_single_terminal", "取消与完成竞争唯一终态率", "run cancellation races with verification PASS", "one command wins, one terminal event is stored, and no contradictory terminal is committed", iterations, passed, started, outcomes)
}

func createMetricRun(t *testing.T, ctx context.Context, workflow *WorkflowRuntimeService, queries *db.Queries, key string, nodes []WorkflowNodeSpec) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	workspaceID := newPGUUID()
	snapshot, err := workflow.CreateRun(ctx, CreateWorkflowRunInput{WorkspaceID: workspaceID, IdempotencyKey: key, Plan: WorkflowPlan{DefinitionKey: "fault-injection", DefinitionVersion: "1", Nodes: nodes}, Actor: WorkflowActor{Type: "system"}})
	if err != nil {
		t.Fatal(err)
	}
	return workspaceID, snapshot.Run.ID
}

func cleanupMetricWorkspace(t *testing.T, ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID) {
	t.Helper()
	if err := queries.DeleteWorkspaceWorkflowRuntime(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
}

func cancelMetricRun(ctx context.Context, workflow *WorkflowRuntimeService, workspaceID, runID pgtype.UUID, idempotencyKey string) error {
	return workflow.inTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.LockWorkflowRun(ctx, util.UUIDToString(runID)); err != nil {
			return err
		}
		run, err := qtx.SetWorkflowRunStatus(ctx, db.SetWorkflowRunStatusParams{Status: "cancelled", ID: runID, WorkspaceID: workspaceID, ExpectedStatus: "running"})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkflowInvalidState
		}
		if err != nil {
			return err
		}
		return workflow.appendEvent(ctx, qtx, eventInput{WorkspaceID: workspaceID, RunID: runID, AggregateType: "run", AggregateID: runID, EventType: "workflow.cancelled", FromState: "running", ToState: "cancelled", AggregateRevision: run.Revision, Actor: WorkflowActor{Type: "system"}, IdempotencyKey: idempotencyKey, Payload: map[string]any{}})
	})
}

func countEvent(events []db.WorkflowEvent, eventType string) int {
	count := 0
	for _, event := range events {
		if event.EventType == eventType {
			count++
		}
	}
	return count
}

func countOutbox(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_outbox WHERE run_id = $1`, util.UUIDToString(runID)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countOutboxStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID pgtype.UUID, status string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_outbox WHERE run_id = $1 AND status = $2`, util.UUIDToString(runID), status).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func newWorkflowFaultMetric(id, name, injected, denominator string, samples, passed int, started time.Time, details map[string]int) workflowFaultMetric {
	rate := 0.0
	if samples > 0 {
		rate = float64(passed) * 100 / float64(samples)
	}
	return workflowFaultMetric{ID: id, Name: name, Injected: injected, Denominator: denominator, SampleSize: samples, Passed: passed, RatePct: rate, DurationMS: time.Since(started).Milliseconds(), Details: details}
}

func writeWorkflowFaultReport(path string, report workflowFaultReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return err
	}
	csvPath := path[:len(path)-len(filepath.Ext(path))] + ".csv"
	file, err := os.Create(csvPath)
	if err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	if err := writer.Write([]string{"metric_id", "metric_name", "injected_fault", "denominator", "sample_size", "passed", "rate_pct", "duration_ms"}); err != nil {
		_ = file.Close()
		return err
	}
	for _, metric := range report.Metrics {
		if err := writer.Write([]string{metric.ID, metric.Name, metric.Injected, metric.Denominator, strconv.Itoa(metric.SampleSize), strconv.Itoa(metric.Passed), strconv.FormatFloat(metric.RatePct, 'f', 1, 64), strconv.FormatInt(metric.DurationMS, 10)}); err != nil {
			_ = file.Close()
			return err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
