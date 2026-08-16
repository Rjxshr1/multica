package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/workflowruntime"
)

const model = "deepseek-v4-pro[1m]"
const maxCapturedModelOutput = 1024 * 1024

type tailBuffer struct {
	data  []byte
	max   int
	total int64
}

func (b *tailBuffer) Write(payload []byte) (int, error) {
	written := len(payload)
	b.total += int64(written)
	if b.max <= 0 {
		return written, nil
	}
	if len(payload) >= b.max {
		b.data = append(b.data[:0], payload[len(payload)-b.max:]...)
		return written, nil
	}
	overflow := len(b.data) + len(payload) - b.max
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, payload...)
	return written, nil
}

func (b *tailBuffer) String() string  { return string(b.data) }
func (b *tailBuffer) Truncated() bool { return b.total > int64(len(b.data)) }

type result struct {
	Arm        string   `json:"arm"`
	Scenario   string   `json:"scenario"`
	Repetition int      `json:"repetition"`
	Passed     bool     `json:"passed"`
	Reason     string   `json:"reason,omitempty"`
	DurationMS int64    `json:"duration_ms"`
	ModelCalls int      `json:"model_calls"`
	Events     []string `json:"events,omitempty"`
}

type evaluator struct {
	root        string
	piPath      string
	piConfig    string
	arm         string
	results     []result
	callCount   int
	repetition  int
	repeatCount int
}

type readOnlyMount struct {
	Host  string
	Guest string
}

func main() {
	arm := flag.String("arm", "all", "original, new, or all")
	root := flag.String("output", "/home/ai/codex-work/multica-workflow-eval/runs", "evaluation output directory")
	piPath := flag.String("pi", "/home/ai/codex-work/multica-workflow-eval/tooling/node_modules/.bin/pi", "Pi executable")
	piConfig := flag.String("pi-config", "/home/ai/codex-work/multica-workflow-eval/pi-config", "Pi config directory")
	selectedScenarios := flag.String("scenarios", "all", "comma-separated scenario names, or all")
	repetitions := flag.Int("repetitions", 1, "paired repetitions per arm")
	flag.Parse()

	if *arm != "all" && *arm != "original" && *arm != "new" {
		fatalf("invalid arm %q", *arm)
	}
	if *repetitions < 1 {
		fatalf("repetitions must be at least 1")
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	runRoot := filepath.Join(*root, stamp)
	if err := os.MkdirAll(runRoot, 0o755); err != nil {
		fatalf("create output: %v", err)
	}

	arms := []string{*arm}
	if *arm == "all" {
		arms = []string{"original", "new"}
	}
	var all []result
	for repetition := 1; repetition <= *repetitions; repetition++ {
		orderedArms := append([]string(nil), arms...)
		if repetition%2 == 0 && len(orderedArms) == 2 {
			orderedArms[0], orderedArms[1] = orderedArms[1], orderedArms[0]
		}
		for _, name := range orderedArms {
			e := &evaluator{root: runRoot, piPath: *piPath, piConfig: *piConfig, arm: name, repetition: repetition, repeatCount: *repetitions}
			e.runAll(*selectedScenarios)
			all = append(all, e.results...)
		}
	}
	if err := writeJSON(filepath.Join(runRoot, "summary.json"), all); err != nil {
		fatalf("write summary: %v", err)
	}
	printSummary(runRoot, all)
}

func (e *evaluator) runAll(selected string) {
	cases := []struct {
		name string
		run  func(string) (bool, string, []string)
	}{
		{"read_code_review", e.readCodeReview},
		{"simple_fix", e.simpleFix},
		{"behavior_preserving_refactor", e.behaviorPreservingRefactor},
		{"technical_design", e.technicalDesign},
		{"chained_integration", e.chainedIntegration},
		{"verification_retry", e.verificationRetry},
		{"cross_workflow_review_insertion", e.crossWorkflow},
	}
	wanted := make(map[string]bool)
	if selected != "all" {
		for _, name := range strings.Split(selected, ",") {
			wanted[strings.TrimSpace(name)] = true
		}
	}
	matched := 0
	for _, item := range cases {
		if selected == "all" || wanted[item.name] {
			e.run(item.name, item.run)
			matched++
		}
	}
	if matched == 0 {
		fatalf("no scenarios matched %q", selected)
	}
}

func (e *evaluator) run(name string, fn func(string) (bool, string, []string)) {
	started := time.Now()
	before := e.callCount
	dir := filepath.Join(e.root, e.arm, name)
	if e.repeatCount > 1 {
		dir = filepath.Join(e.root, fmt.Sprintf("repeat-%02d", e.repetition), e.arm, name)
	}
	if err := os.RemoveAll(dir); err != nil {
		fatalf("reset fixture: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatalf("create fixture: %v", err)
	}
	passed, reason, events := fn(dir)
	r := result{Arm: e.arm, Scenario: name, Repetition: e.repetition, Passed: passed, Reason: reason, DurationMS: time.Since(started).Milliseconds(), ModelCalls: e.callCount - before, Events: events}
	e.results = append(e.results, r)
	_ = writeJSON(filepath.Join(dir, "result.json"), r)
}

func (e *evaluator) simpleFix(dir string) (bool, string, []string) {
	mustWrite(filepath.Join(dir, "calc.js"), "export function add(a, b) { return a - b; }\n")
	mustWrite(filepath.Join(dir, "calc.test.js"), "import test from 'node:test'; import assert from 'node:assert/strict'; import {add} from './calc.js'; test('adds',()=>assert.equal(add(7,5),12));\n")
	mustWrite(filepath.Join(dir, "package.json"), `{"type":"module","scripts":{"test":"node --test"}}`)
	prompt := "Find the bug in this small repository, fix it, and run npm test. Do not only describe the fix; edit the files."
	if e.arm == "original" {
		if err := e.pi(dir, "fix", prompt); err != nil {
			return false, err.Error(), nil
		}
		ok, why := verify(dir, "npm", "test")
		return ok, why, nil
	}
	return e.runRuntimeNode(dir, "simple", "fix", 1, prompt, func() (bool, string) { return verify(dir, "npm", "test") })
}

func (e *evaluator) readCodeReview(dir string) (bool, string, []string) {
	mustWrite(filepath.Join(dir, "inventory.js"), "export function available(stock, reserved) {\n  return stock + reserved;\n}\n")
	mustWrite(filepath.Join(dir, "checkout.js"), "import {available} from './inventory.js';\nexport function canCheckout(stock, reserved, requested) {\n  return available(stock, reserved) >= requested;\n}\n")
	mustWrite(filepath.Join(dir, "README.md"), "Available inventory equals physical stock minus reserved units. Do not edit source code during this review task.\n")
	prompt := "Read README.md, inventory.js, and checkout.js. Do not edit source code. Write CODE_REVIEW.md identifying the concrete correctness defect, its user-visible risk, and the smallest safe fix."
	check := func() (bool, string) {
		return verifyDocument(dir, "CODE_REVIEW.md", []string{"reserved", "subtract", "checkout"})
	}
	if e.arm == "original" {
		if err := e.pi(dir, "review", prompt); err != nil {
			return false, err.Error(), nil
		}
		ok, why := check()
		return ok, why, nil
	}
	return e.runRuntimeNode(dir, "read-review", "review", 1, prompt, check)
}

func (e *evaluator) behaviorPreservingRefactor(dir string) (bool, string, []string) {
	mustWrite(filepath.Join(dir, "parser.js"), `export function parseUser(line) {
  const parts = line.split('|');
  return {id: parts[0].trim(), name: parts[1].trim(), role: parts[2].trim()};
}
export function parseTeam(line) {
  const parts = line.split('|');
  return {id: parts[0].trim(), name: parts[1].trim(), role: parts[2].trim()};
}
`)
	mustWrite(filepath.Join(dir, "parser.test.js"), "import test from 'node:test'; import assert from 'node:assert/strict'; import {parseUser,parseTeam} from './parser.js'; const expected={id:'7',name:'Ada',role:'admin'}; test('user',()=>assert.deepEqual(parseUser(' 7 | Ada | admin '),expected)); test('team',()=>assert.deepEqual(parseTeam(' 7 | Ada | admin '),expected));\n")
	mustWrite(filepath.Join(dir, "package.json"), `{"type":"module","scripts":{"test":"node --test"}}`)
	prompt := "Refactor parser.js to remove the duplicated split/trim/mapping logic while preserving both exported APIs and behavior. Run npm test and write REFACTOR_NOTES.md explaining the boundary you extracted."
	check := func() (bool, string) {
		if ok, why := verifyFilesAndCommand(dir, []string{"REFACTOR_NOTES.md"}, "npm", "test"); !ok {
			return false, why
		}
		payload, err := os.ReadFile(filepath.Join(dir, "parser.js"))
		if err != nil {
			return false, err.Error()
		}
		if strings.Count(string(payload), ".split('|')") > 1 {
			return false, "duplicated split logic remains"
		}
		return true, "tests pass and duplication removed"
	}
	if e.arm == "original" {
		if err := e.pi(dir, "refactor", prompt); err != nil {
			return false, err.Error(), nil
		}
		ok, why := check()
		return ok, why, nil
	}
	return e.runRuntimeNode(dir, "refactor", "refactor", 1, prompt, check)
}

func (e *evaluator) technicalDesign(dir string) (bool, string, []string) {
	mustWrite(filepath.Join(dir, "SYSTEM.md"), `# Existing system

Workers poll a task table. A worker may crash after completing remote work but before recording success. Duplicate delivery is possible. Product requires multi-step workflows, bounded retries, live progress, and no double execution after lease expiry. PostgreSQL is the source of truth; no new message broker may be introduced.
`)
	prompt := "Read SYSTEM.md and write TECHNICAL_PLAN.md in no more than 1200 words. Propose an implementable PostgreSQL-backed workflow design covering the state machine, atomic claiming/concurrency control, idempotency, crash recovery, bounded retries, reliable event delivery, observability, rollout, rollback, and failure tests. Keep it concise and specific enough for engineers to implement."
	check := func() (bool, string) {
		return verifyDocument(dir, "TECHNICAL_PLAN.md", []string{"state", "idempot", "retry", "outbox", "observ", "crash", "rollback"})
	}
	if e.arm == "original" {
		if err := e.pi(dir, "design", prompt); err != nil {
			return false, err.Error(), nil
		}
		ok, why := check()
		return ok, why, nil
	}
	return e.runRuntimeNode(dir, "technical-design", "design", 1, prompt, check)
}

func (e *evaluator) chainedIntegration(dir string) (bool, string, []string) {
	mustWrite(filepath.Join(dir, "stats.js"), "export function median(values) { throw new Error('TODO'); }\n")
	mustWrite(filepath.Join(dir, "stats.test.js"), "import test from 'node:test'; import assert from 'node:assert/strict'; import {median} from './stats.js'; test('odd',()=>assert.equal(median([9,1,5]),5)); test('even',()=>assert.equal(median([1,9,3,7]),5));\n")
	mustWrite(filepath.Join(dir, "package.json"), `{"type":"module","scripts":{"test":"node --test"}}`)
	implement := "Implement median in stats.js for odd and even arrays without mutating the input. Run npm test."
	integration := "Act as the integration-test node. Run npm test and inspect the implementation. If it is correct, create INTEGRATION_PASSED.md with a one-line summary."
	if e.arm == "original" {
		if err := e.pi(dir, "implement", implement); err != nil {
			return false, err.Error(), nil
		}
		if ok, why := verify(dir, "npm", "test"); !ok {
			return false, why, nil
		}
		if err := e.pi(dir, "integration", integration); err != nil {
			return false, err.Error(), nil
		}
		ok, why := verifyFilesAndCommand(dir, []string{"INTEGRATION_PASSED.md"}, "npm", "test")
		return ok, why, nil
	}
	store := workflowruntime.NewMemoryEventStore()
	engine := workflowruntime.NewEngine(store)
	_ = engine.Create(workflowruntime.WorkflowSpec{ID: "chain", Nodes: []workflowruntime.NodeSpec{{ID: "implement", MaxAttempts: 1}, {ID: "integration", DependsOn: []string{"implement"}, MaxAttempts: 1}}}, "create")
	if ok, why := e.execute(engine, "chain", "implement", dir, implement, func() (bool, string) { return verify(dir, "npm", "test") }); !ok {
		return false, why, eventNames(engine, "chain")
	}
	if ok, why := e.execute(engine, "chain", "integration", dir, integration, func() (bool, string) {
		return verifyFilesAndCommand(dir, []string{"INTEGRATION_PASSED.md"}, "npm", "test")
	}); !ok {
		return false, why, eventNames(engine, "chain")
	}
	run, _ := engine.Snapshot("chain")
	return run.State == workflowruntime.WorkflowSucceeded, string(run.State), eventNames(engine, "chain")
}

func (e *evaluator) verificationRetry(dir string) (bool, string, []string) {
	mustWrite(filepath.Join(dir, "slug.js"), "export function slug(value) { return value.toLowerCase().replaceAll(' ', '-'); }\n")
	mustWrite(filepath.Join(dir, "slug.test.js"), "import test from 'node:test'; import assert from 'node:assert/strict'; import {slug} from './slug.js'; test('words',()=>assert.equal(slug('Hello World'),'hello-world'));\n")
	mustWrite(filepath.Join(dir, "package.json"), `{"type":"module","scripts":{"test":"node --test"}}`)
	first := "Run the visible tests and make only the changes they require. Do not invent undocumented behavior."
	hidden := func() (bool, string) {
		return verify(dir, "node", "--input-type=module", "-e", "import {slug} from './slug.js'; let ok=false; try { slug('   ') } catch { ok=true } if(!ok) throw new Error('empty slug must throw');")
	}
	if e.arm == "original" {
		if err := e.pi(dir, "attempt-1", first); err != nil {
			return false, err.Error(), nil
		}
		if ok, why := hidden(); !ok {
			return false, "verification rejected and original task loop stopped: " + why, nil
		}
		return true, "passed first verification", nil
	}
	store := workflowruntime.NewMemoryEventStore()
	engine := workflowruntime.NewEngine(store)
	_ = engine.Create(workflowruntime.WorkflowSpec{ID: "retry", Nodes: []workflowruntime.NodeSpec{{ID: "fix", MaxAttempts: 2}}}, "create")
	if ok, _ := e.execute(engine, "retry", "fix", dir, first, hidden); ok {
		return true, "passed first verification", eventNames(engine, "retry")
	}
	feedback := "Verification rejected the previous result: slug must throw an Error when the normalized slug is empty or whitespace-only. Fix that contract and run all visible tests."
	if ok, why := e.execute(engine, "retry", "fix", dir, feedback, hidden); !ok {
		return false, why, eventNames(engine, "retry")
	}
	run, _ := engine.Snapshot("retry")
	return run.State == workflowruntime.WorkflowSucceeded, string(run.State), eventNames(engine, "retry")
}

func (e *evaluator) crossWorkflow(dir string) (bool, string, []string) {
	mustWrite(filepath.Join(dir, "dependency.js"), "export function format(value) { return {status: 'READY', value}; }\n")
	mustWrite(filepath.Join(dir, "app.js"), "import {format} from './dependency.js'; export function render(value) { return format(value); }\n")
	mustWrite(filepath.Join(dir, "integration.test.js"), "import test from 'node:test'; import assert from 'node:assert/strict'; import {render} from './app.js'; test('contract',()=>assert.equal(render(42),'READY:42'));\n")
	mustWrite(filepath.Join(dir, "package.json"), `{"type":"module","scripts":{"test":"node --test"}}`)
	diagnose := "You are Workflow 1's integration-test node. Run npm test, diagnose the dependency contract failure, and write BUG_REPORT.md for Workflow 2. Do not fix dependency.js."
	fix := "You are Workflow 2. Read BUG_REPORT.md, fix the dependency contract bug, and run npm test."
	review := "You are a newly inserted independent review node. Review BUG_REPORT.md and the current code, run npm test, and create REVIEW_APPROVED.md only if the fix matches the required string contract."
	integrate := "You are Workflow 1's resumed integration-test node. Run npm test. If it passes, create INTEGRATION_PASSED.md."
	if e.arm == "original" {
		if err := e.pi(dir, "w1-diagnose", diagnose); err != nil {
			return false, err.Error(), nil
		}
		if _, err := os.Stat(filepath.Join(dir, "BUG_REPORT.md")); err != nil {
			return false, "Workflow 1 did not create BUG_REPORT.md", nil
		}
		if err := e.pi(dir, "w2-fix", fix); err != nil {
			return false, err.Error(), nil
		}
		if ok, why := verify(dir, "npm", "test"); !ok {
			return false, why, nil
		}
		// Original orchestration can resume the existing integration task, but has
		// no safe live-plan amendment command to insert the requested review gate.
		if err := e.pi(dir, "w1-integration", integrate); err != nil {
			return false, err.Error(), nil
		}
		ok, why := verifyFilesAndCommand(dir, []string{"REVIEW_APPROVED.md", "INTEGRATION_PASSED.md"}, "npm", "test")
		return ok, why, nil
	}
	w1Store := workflowruntime.NewMemoryEventStore()
	w1 := workflowruntime.NewEngine(w1Store)
	_ = w1.Create(workflowruntime.WorkflowSpec{ID: "workflow-1", Nodes: []workflowruntime.NodeSpec{{ID: "integration", MaxAttempts: 2}}}, "create-w1")
	lease, err := w1.Claim("workflow-1", "integration", "claim-diagnose")
	if err != nil {
		return false, err.Error(), eventNames(w1, "workflow-1")
	}
	if err := e.pi(dir, "w1-diagnose", diagnose); err != nil {
		return false, err.Error(), eventNames(w1, "workflow-1")
	}
	_ = w1.ReportTaskSucceeded(lease, "report-diagnose")
	bugExists := fileExists(filepath.Join(dir, "BUG_REPORT.md"))
	_ = w1.Verify(lease, false, workflowruntime.FailureDependencyGap, "verify-diagnose")
	if !bugExists {
		return false, "Workflow 1 did not create BUG_REPORT.md", eventNames(w1, "workflow-1")
	}

	w2Store := workflowruntime.NewMemoryEventStore()
	w2 := workflowruntime.NewEngine(w2Store)
	_ = w2.Create(workflowruntime.WorkflowSpec{ID: "workflow-2", Nodes: []workflowruntime.NodeSpec{{ID: "fix", MaxAttempts: 1}}}, "create-w2")
	if ok, why := e.execute(w2, "workflow-2", "fix", dir, fix, func() (bool, string) { return verify(dir, "npm", "test") }); !ok {
		return false, why, append(eventNames(w1, "workflow-1"), eventNames(w2, "workflow-2")...)
	}
	if err := w1.AddNodeBefore("workflow-1", "integration", workflowruntime.NodeSpec{ID: "review", MaxAttempts: 1}, "insert-review-after-w2"); err != nil {
		return false, err.Error(), eventNames(w1, "workflow-1")
	}
	if ok, why := e.execute(w1, "workflow-1", "review", dir, review, func() (bool, string) {
		return verifyFilesAndCommand(dir, []string{"REVIEW_APPROVED.md"}, "npm", "test")
	}); !ok {
		return false, why, eventNames(w1, "workflow-1")
	}
	if ok, why := e.execute(w1, "workflow-1", "integration", dir, integrate, func() (bool, string) {
		return verifyFilesAndCommand(dir, []string{"REVIEW_APPROVED.md", "INTEGRATION_PASSED.md"}, "npm", "test")
	}); !ok {
		return false, why, eventNames(w1, "workflow-1")
	}
	run, _ := w1.Snapshot("workflow-1")
	events := append(eventNames(w1, "workflow-1"), eventNames(w2, "workflow-2")...)
	return run.State == workflowruntime.WorkflowSucceeded, string(run.State), events
}

func (e *evaluator) runRuntimeNode(dir, runID, nodeID string, attempts int, prompt string, verifier func() (bool, string)) (bool, string, []string) {
	store := workflowruntime.NewMemoryEventStore()
	engine := workflowruntime.NewEngine(store)
	_ = engine.Create(workflowruntime.WorkflowSpec{ID: runID, Nodes: []workflowruntime.NodeSpec{{ID: nodeID, MaxAttempts: attempts}}}, "create")
	ok, why := e.execute(engine, runID, nodeID, dir, prompt, verifier)
	return ok, why, eventNames(engine, runID)
}

func (e *evaluator) execute(engine *workflowruntime.Engine, runID, nodeID, dir, prompt string, verifier func() (bool, string)) (bool, string) {
	lease, err := engine.Claim(runID, nodeID, fmt.Sprintf("claim-%s-%d", nodeID, time.Now().UnixNano()))
	if err != nil {
		return false, err.Error()
	}
	progressSequence := 0
	budget := defaultEvaluationBudget()
	if run, snapshotErr := engine.Snapshot(runID); snapshotErr == nil {
		if node, ok := run.Nodes[nodeID]; ok {
			budget = progressBudgetForNode(node.Spec)
		}
	}
	if err := e.piWithProgress(dir, fmt.Sprintf("%s-a%d", nodeID, lease.Attempt), prompt, func() {
		progressSequence++
		_ = engine.ReportProgress(lease, fmt.Sprintf("progress-%s-%d", lease.AttemptID, progressSequence))
	}, budget); err != nil {
		failure := workflowruntime.FailureEnvironmentGap
		if errors.Is(err, errNoProgress) {
			failure = workflowruntime.FailureNoProgress
		} else if errors.Is(err, errHardDeadline) {
			failure = workflowruntime.FailureDeadlineExceeded
		}
		_ = engine.FailAttempt(lease, failure, fmt.Sprintf("fail-%s", lease.AttemptID))
		return false, err.Error()
	}
	if err := engine.ReportTaskSucceeded(lease, fmt.Sprintf("report-%s", lease.AttemptID)); err != nil {
		return false, err.Error()
	}
	passed, why := verifier()
	failure := workflowruntime.FailureCodeDefect
	if err := engine.Verify(lease, passed, failure, fmt.Sprintf("verify-%s", lease.AttemptID)); err != nil {
		return false, err.Error()
	}
	return passed, why
}

func defaultEvaluationBudget() progressBudget {
	// Conservative evaluation guardrails, not a production-wide definition of
	// task failure. Nodes can override each dimension based on task complexity.
	return progressBudget{
		HardTimeout:          30 * time.Minute,
		FirstProgressTimeout: 8 * time.Minute,
		IdleTimeout:          5 * time.Minute,
	}
}

func progressBudgetForNode(spec workflowruntime.NodeSpec) progressBudget {
	budget := defaultEvaluationBudget()
	if spec.ExecutionBudget == nil {
		return budget
	}
	configured := spec.ExecutionBudget
	if configured.FirstProgressTimeoutSeconds > 0 {
		budget.FirstProgressTimeout = time.Duration(configured.FirstProgressTimeoutSeconds) * time.Second
	}
	if configured.IdleTimeoutSeconds > 0 {
		budget.IdleTimeout = time.Duration(configured.IdleTimeoutSeconds) * time.Second
	}
	if configured.HardTimeoutSeconds > 0 {
		budget.HardTimeout = time.Duration(configured.HardTimeoutSeconds) * time.Second
	}
	return budget
}

func (e *evaluator) pi(dir, label, prompt string) error {
	return e.piWithProgress(dir, label, prompt, nil, defaultEvaluationBudget())
}

func (e *evaluator) piWithProgress(dir, label, prompt string, onProgress func(), budget progressBudget) error {
	e.callCount++
	logDir := filepath.Join(dir, "model-logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	session := filepath.Join(logDir, label+".session.jsonl")
	toolRoot := filepath.Clean(filepath.Join(filepath.Dir(e.piPath), "..", ".."))
	guestPiPath := filepath.Join("/tooling", "node_modules", ".bin", filepath.Base(e.piPath))
	guestSession := filepath.Join("/workspace", "model-logs", label+".session.jsonl")
	cmd := sandboxedCommand(dir, []readOnlyMount{
		{Host: toolRoot, Guest: "/tooling"},
		{Host: e.piConfig, Guest: "/pi-config"},
	}, guestPiPath, "-p", "--mode", "json", "--session", guestSession, "--provider", "deepseek-anthropic", "--model", model)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = cleanProxyEnv(os.Environ())
	cmd.Env = append(cmd.Env, "PI_CODING_AGENT_DIR=/pi-config", "PI_TELEMETRY=0", "HOME=/home/agent")
	output := tailBuffer{max: maxCapturedModelOutput}
	cmd.Stdout = &output
	cmd.Stderr = &output
	started := time.Now()
	// Only semantic session events (tool/query/result) or durable repository
	// artifacts renew the lease. Plain thinking/session growth is not progress.
	probe := newCompositeProgressProbe(
		newFilesystemProgressProbe(dir, filepath.Join(dir, ".git"), logDir),
		newSessionSemanticProgressProbe(session),
	)
	budget.PollInterval = time.Second
	budget.OnProgress = onProgress
	err := runWithProgressBudget(context.Background(), cmd, budget, probe)
	log := map[string]any{
		"label": label, "prompt": prompt, "duration_ms": time.Since(started).Milliseconds(),
		"exit_error": errorString(err), "output": output.String(),
		"output_bytes": output.total, "output_truncated": output.Truncated(),
	}
	_ = writeJSON(filepath.Join(logDir, label+".json"), log)
	if errors.Is(err, errNoProgress) {
		return fmt.Errorf("Pi/DeepSeek %s: %w", label, err)
	}
	if errors.Is(err, errHardDeadline) {
		return fmt.Errorf("Pi/DeepSeek %s: %w", label, err)
	}
	if err != nil {
		return fmt.Errorf("Pi/DeepSeek %s failed: %w", label, err)
	}
	return nil
}

// sandboxedCommand gives the model tools a deliberately narrow filesystem:
// the current fixture is writable at /workspace, runtime/config mounts are
// read-only, and previous runs plus verifier source are absent. Network stays
// available for the model provider.
func sandboxedCommand(workspace string, mounts []readOnlyMount, command string, args ...string) *exec.Cmd {
	bwrapArgs := []string{
		"--die-with-parent", "--new-session", "--unshare-all", "--share-net",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind", "/etc", "/etc",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--dir", "/home", "--dir", "/home/agent",
		"--bind", workspace, "/workspace",
	}
	// WSL's /etc/resolv.conf points outside /etc, so expose only that
	// generated resolver file instead of the rest of /mnt.
	if _, err := os.Stat("/mnt/wsl/resolv.conf"); err == nil {
		bwrapArgs = append(bwrapArgs,
			"--dir", "/mnt", "--dir", "/mnt/wsl",
			"--ro-bind", "/mnt/wsl/resolv.conf", "/mnt/wsl/resolv.conf",
		)
	}
	for _, mount := range mounts {
		bwrapArgs = append(bwrapArgs, "--ro-bind", mount.Host, mount.Guest)
	}
	bwrapArgs = append(bwrapArgs, "--chdir", "/workspace", command)
	bwrapArgs = append(bwrapArgs, args...)
	return exec.Command("bwrap", bwrapArgs...)
}

func cleanProxyEnv(env []string) []string {
	blocked := map[string]bool{"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "http_proxy": true, "https_proxy": true, "all_proxy": true}
	result := env[:0]
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if !blocked[key] {
			result = append(result, item)
		}
	}
	return result
}

func verify(dir, name string, args ...string) (bool, string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return false, strings.TrimSpace(output.String())
	}
	return true, strings.TrimSpace(output.String())
}

func verifyFilesAndCommand(dir string, files []string, name string, args ...string) (bool, string) {
	for _, file := range files {
		if !fileExists(filepath.Join(dir, file)) {
			return false, "missing required artifact " + file
		}
	}
	return verify(dir, name, args...)
}

func verifyDocument(dir, name string, required []string) (bool, string) {
	payload, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return false, "missing required artifact " + name
	}
	text := strings.ToLower(string(payload))
	for _, term := range required {
		if !strings.Contains(text, strings.ToLower(term)) {
			return false, fmt.Sprintf("%s missing required concept %q", name, term)
		}
	}
	return true, name + " passed content checks"
}

func eventNames(engine *workflowruntime.Engine, runID string) []string {
	events := engine.Events(runID)
	result := make([]string, 0, len(events))
	for _, event := range events {
		result = append(result, string(event.Type))
	}
	return result
}

func mustWrite(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fatalf("write fixture: %v", err)
	}
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func writeJSON(path string, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(payload, '\n'), 0o644)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func printSummary(root string, results []result) {
	fmt.Printf("evidence: %s\n", root)
	byArm := map[string][]result{}
	for _, item := range results {
		byArm[item.Arm] = append(byArm[item.Arm], item)
	}
	keys := make([]string, 0, len(byArm))
	for key := range byArm {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, arm := range keys {
		passed := 0
		for _, item := range byArm[arm] {
			if item.Passed {
				passed++
			}
			fmt.Printf("%s %-31s passed=%-5v calls=%d reason=%s\n", arm, item.Scenario, item.Passed, item.ModelCalls, compact(item.Reason))
		}
		fmt.Printf("%s total: %d/%d (%.1f%%)\n", arm, passed, len(byArm[arm]), 100*float64(passed)/float64(len(byArm[arm])))
	}
}

func compact(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 160 {
		return value[:160] + "..."
	}
	return value
}

func fatalf(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...); os.Exit(1) }
