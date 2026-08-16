package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/multica-ai/multica/server/internal/workflowruntime"
)

type scenario string

const (
	scenarioHappy             scenario = "happy_path"
	scenarioDuplicateCallback scenario = "duplicate_callback"
	scenarioProcessRestart    scenario = "process_restart"
	scenarioVerificationRetry scenario = "verification_retry"
	scenarioStaleCallback     scenario = "stale_callback"
)

var scenarios = []scenario{
	scenarioHappy,
	scenarioDuplicateCallback,
	scenarioProcessRestart,
	scenarioVerificationRetry,
	scenarioStaleCallback,
}

type engineResult struct {
	Succeeded int     `json:"succeeded"`
	Total     int     `json:"total"`
	Rate      float64 `json:"rate"`
}

type scenarioResult struct {
	Scenario      scenario `json:"scenario"`
	Runs          int      `json:"runs"`
	LegacySuccess int      `json:"legacy_success"`
	CoreSuccess   int      `json:"core_success"`
}

type report struct {
	Benchmark string           `json:"benchmark"`
	Legacy    engineResult     `json:"legacy_issue_driven"`
	Core      engineResult     `json:"authoritative_core"`
	Scenarios []scenarioResult `json:"scenarios"`
}

func main() {
	runs := flag.Int("runs", 100, "total deterministic fault-injected runs")
	jsonOutput := flag.Bool("json", false, "print the report as JSON")
	trace := flag.Bool("trace", false, "print one verification-retry event trace")
	flag.Parse()

	if *runs < len(scenarios) {
		fmt.Fprintf(os.Stderr, "runs must be at least %d\n", len(scenarios))
		os.Exit(2)
	}

	report, err := runBenchmark(*runs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchmark failed: %v\n", err)
		os.Exit(1)
	}
	if *jsonOutput {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "encode report: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(encoded))
	} else {
		printReport(report)
	}

	if *trace {
		if err := printRetryTrace(); err != nil {
			fmt.Fprintf(os.Stderr, "trace failed: %v\n", err)
			os.Exit(1)
		}
	}
	if report.Core.Succeeded != report.Core.Total {
		os.Exit(1)
	}
}

func runBenchmark(runs int) (report, error) {
	counts := make(map[scenario]*scenarioResult, len(scenarios))
	result := report{Benchmark: "deterministic_fault_injected_reference"}
	for _, name := range scenarios {
		counts[name] = &scenarioResult{Scenario: name}
	}

	for i := 0; i < runs; i++ {
		name := scenarios[i%len(scenarios)]
		count := counts[name]
		count.Runs++
		if runLegacy(name) {
			count.LegacySuccess++
			result.Legacy.Succeeded++
		}
		ok, err := runAuthoritative(fmt.Sprintf("benchmark-%03d", i+1), name, nil)
		if err != nil {
			return report{}, fmt.Errorf("%s run %d: %w", name, i+1, err)
		}
		if ok {
			count.CoreSuccess++
			result.Core.Succeeded++
		}
	}

	result.Legacy.Total = runs
	result.Core.Total = runs
	result.Legacy.Rate = percent(result.Legacy.Succeeded, runs)
	result.Core.Rate = percent(result.Core.Succeeded, runs)
	for _, name := range scenarios {
		result.Scenarios = append(result.Scenarios, *counts[name])
	}
	return result, nil
}

// runLegacy models the old issue-driven loop: one mutable status, agent
// completion treated as truth, and no durable attempt identity.
func runLegacy(name scenario) bool {
	status := "running"
	sideEffects := 0

	switch name {
	case scenarioHappy:
		status = "succeeded"
		return status == "succeeded"
	case scenarioDuplicateCallback:
		for range 2 {
			status = "succeeded"
			sideEffects++
		}
		return status == "succeeded" && sideEffects == 1
	case scenarioProcessRestart:
		status = "result_received"
		status = ""
		return status == "succeeded"
	case scenarioVerificationRetry:
		status = "succeeded"
		independentVerificationPassed := false
		return status == "succeeded" && independentVerificationPassed
	case scenarioStaleCallback:
		status = "succeeded"
		status = "failed"
		return status == "succeeded"
	default:
		return false
	}
}

func runAuthoritative(runID string, name scenario, trace *[]workflowruntime.Event) (bool, error) {
	var store workflowruntime.EventStore = workflowruntime.NewMemoryEventStore()
	var durablePath string
	if name == scenarioProcessRestart {
		directory, err := os.MkdirTemp("", "multica-workflow-loop-*")
		if err != nil {
			return false, err
		}
		defer os.RemoveAll(directory)
		durablePath = filepath.Join(directory, "events.json")
		store, err = workflowruntime.OpenFileEventStore(durablePath)
		if err != nil {
			return false, err
		}
	}
	engine := workflowruntime.NewEngine(store)
	spec := workflowruntime.WorkflowSpec{ID: runID, Nodes: []workflowruntime.NodeSpec{
		{ID: "implement", MaxAttempts: 2},
		{ID: "deliver", DependsOn: []string{"implement"}, MaxAttempts: 1},
	}}
	if err := engine.Create(spec, "create"); err != nil {
		return false, err
	}

	first, err := engine.Claim(runID, "implement", "claim-implement-1")
	if err != nil {
		return false, err
	}
	if err := engine.ReportTaskSucceeded(first, "result-implement-1"); err != nil {
		return false, err
	}

	switch name {
	case scenarioDuplicateCallback:
		if err := engine.ReportTaskSucceeded(first, "result-implement-1"); err != nil {
			return false, err
		}
		if countEvents(engine.Events(runID), workflowruntime.EventTaskResultAccepted) != 1 {
			return false, nil
		}
	case scenarioProcessRestart:
		reopened, err := workflowruntime.OpenFileEventStore(durablePath)
		if err != nil {
			return false, err
		}
		engine = workflowruntime.NewEngine(reopened)
	case scenarioVerificationRetry, scenarioStaleCallback:
		if err := engine.Verify(first, false, workflowruntime.FailureCodeDefect, "verify-implement-1"); err != nil {
			return false, err
		}
		second, err := engine.Claim(runID, "implement", "claim-implement-2")
		if err != nil {
			return false, err
		}
		if name == scenarioStaleCallback {
			err := engine.ReportTaskSucceeded(first, "late-result-from-attempt-1")
			if !errors.Is(err, workflowruntime.ErrStaleFence) {
				return false, fmt.Errorf("late callback error = %v, want stale fence", err)
			}
		}
		if err := engine.ReportTaskSucceeded(second, "result-implement-2"); err != nil {
			return false, err
		}
		if err := engine.Verify(second, true, "", "verify-implement-2"); err != nil {
			return false, err
		}
	default:
	}

	if name != scenarioVerificationRetry && name != scenarioStaleCallback {
		if err := engine.Verify(first, true, "", "verify-implement-1"); err != nil {
			return false, err
		}
	}

	deliver, err := engine.Claim(runID, "deliver", "claim-deliver")
	if err != nil {
		return false, err
	}
	if err := engine.ReportTaskSucceeded(deliver, "result-deliver"); err != nil {
		return false, err
	}
	if err := engine.Verify(deliver, true, "", "verify-deliver"); err != nil {
		return false, err
	}

	run, err := engine.Snapshot(runID)
	if err != nil {
		return false, err
	}
	if trace != nil {
		*trace = engine.Events(runID)
	}
	return run.State == workflowruntime.WorkflowSucceeded, nil
}

func countEvents(events []workflowruntime.Event, eventType workflowruntime.EventType) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func printReport(result report) {
	fmt.Println("Multica workflow loop comparison")
	fmt.Println("Benchmark: deterministic, fault-injected reference (not production telemetry)")
	fmt.Println()
	fmt.Printf("%-24s %10s %10s\n", "engine", "success", "rate")
	fmt.Printf("%-24s %3d/%-6d %9.1f%%\n", "legacy_issue_driven", result.Legacy.Succeeded, result.Legacy.Total, result.Legacy.Rate)
	fmt.Printf("%-24s %3d/%-6d %9.1f%%\n", "authoritative_core", result.Core.Succeeded, result.Core.Total, result.Core.Rate)
	fmt.Println()
	fmt.Printf("%-24s %10s %10s\n", "scenario", "legacy", "core")
	for _, item := range result.Scenarios {
		fmt.Printf("%-24s %3d/%-6d %3d/%-6d\n", item.Scenario, item.LegacySuccess, item.Runs, item.CoreSuccess, item.Runs)
	}
}

func printRetryTrace() error {
	var events []workflowruntime.Event
	ok, err := runAuthoritative("trace-verification-retry", scenarioVerificationRetry, &events)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("trace workflow did not succeed")
	}

	sort.Slice(events, func(i, j int) bool { return events[i].Version < events[j].Version })
	fmt.Println()
	fmt.Println("Verification-retry loop trace")
	for _, event := range events {
		detail := event.NodeID
		if event.AttemptID != "" {
			detail = fmt.Sprintf("%s attempt=%s fence=%d", event.NodeID, event.AttemptID, event.Fence)
		}
		if event.FailureClass != "" {
			detail += " failure=" + string(event.FailureClass)
		}
		fmt.Printf("v%-2d %-27s %s\n", event.Version, event.Type, detail)
	}
	fmt.Println("final state: succeeded (implement attempts=2, deliver attempts=1)")
	return nil
}

func percent(success, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(success) * 100 / float64(total)
}
