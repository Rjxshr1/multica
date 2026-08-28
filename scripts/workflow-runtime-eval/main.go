package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	workflowruntime "multica-expanded-eval/runtime"
)

const defaultModel = "deepseek-v4-pro[1m]"

type verifier struct {
	Command       []string            `json:"command,omitempty"`
	RequiredFiles map[string][]string `json:"required_files,omitempty"`
}

type stage struct {
	ID       string   `json:"id"`
	Prompt   string   `json:"prompt"`
	Verifier verifier `json:"verifier"`
}

type taskSpec struct {
	ID            string            `json:"id"`
	Category      string            `json:"category"`
	Difficulty    string            `json:"difficulty"`
	Mode          string            `json:"mode"`
	Files         map[string]string `json:"files"`
	Stages        []stage           `json:"stages"`
	RetryFeedback string            `json:"retry_feedback,omitempty"`
}

type usage struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Total      int64   `json:"total_tokens"`
	Cost       float64 `json:"reported_cost"`
}

type result struct {
	Arm                     string          `json:"arm"`
	Repetition              int             `json:"repetition"`
	TaskID                  string          `json:"task_id"`
	Category                string          `json:"category"`
	Difficulty              string          `json:"difficulty"`
	OrderIndex              int             `json:"order_index"`
	Seed                    int64           `json:"seed"`
	FirstPassSuccess        bool            `json:"first_pass_success"`
	FinalPassed             bool            `json:"final_passed"`
	Recovered               bool            `json:"recovered"`
	HumanInterventionActual int             `json:"human_intervention_actual"`
	WouldRequireHuman       bool            `json:"would_require_human_to_finish"`
	Reason                  string          `json:"reason"`
	StartedAt               string          `json:"started_at"`
	FinishedAt              string          `json:"finished_at"`
	DurationMS              int64           `json:"duration_ms"`
	EnvironmentWaitMS       int64           `json:"environment_wait_ms"`
	ModelCalls              int             `json:"model_calls"`
	GuardRecoveries         int             `json:"guard_recoveries"`
	Usage                   usage           `json:"usage"`
	Events                  []string        `json:"events,omitempty"`
	Requests                []requestMetric `json:"requests,omitempty"`
	BudgetSeconds           int             `json:"per_attempt_hard_budget_seconds"`
}

type job struct {
	Index      int
	Profile    armProfile
	Repetition int
	Task       taskSpec
}

type evaluator struct {
	runRoot                    string
	piPath                     string
	piConfig                   string
	provider                   string
	model                      string
	extensions                 []string
	seed                       int64
	hardTimeout                time.Duration
	firstProgressTimeout       time.Duration
	idleTimeout                time.Duration
	profile                    armProfile
	repetition                 int
	requestMu                  sync.Mutex
	requestsByWorkspace        map[string][]requestMetric
	environmentWaitByWorkspace map[string]time.Duration
	driver                     string
	isolation                  string
	faultTasks                 map[string]bool
	faultStall                 time.Duration
	faultInjected              map[string]bool
}

func main() {
	root := flag.String("output", "/home/ai/codex-work/multica-expanded-eval/workflow/runs", "run output root")
	runID := flag.String("run-id", "", "stable run id (reuses completed task results)")
	piPath := flag.String("pi", "/home/ai/codex-work/multica-workflow-eval/tooling/node_modules/.bin/pi", "Pi executable")
	piConfig := flag.String("pi-config", "/home/ai/codex-work/multica-workflow-eval/pi-config", "Pi config")
	provider := flag.String("provider", "deepseek-anthropic", "Pi provider ID")
	modelName := flag.String("model", defaultModel, "Pi model ID")
	extensionsFlag := flag.String("extensions", "", "comma-separated explicit Pi extension paths")
	seed := flag.Int64("seed", 20260816, "randomization seed")
	workers := flag.Int("workers", 2, "parallel randomized workers")
	hardMinutes := flag.Int("hard-minutes", 12, "hard budget per model attempt")
	firstProgressSeconds := flag.Int("first-progress-seconds", 180, "guard budget before the first progress signal")
	idleSeconds := flag.Int("idle-seconds", 420, "guard budget between progress signals")
	repetitions := flag.Int("repetitions", 1, "paired repetitions of every arm")
	armsFlag := flag.String("arms", strings.Join(defaultArmIDs, ","), "comma-separated experiment arms")
	driver := flag.String("driver", "real", "execution driver: real or deterministic")
	isolation := flag.String("isolation", "bwrap", "real-driver isolation: bwrap or macos-sandbox")
	schedule := flag.String("schedule", "paired", "execution schedule: paired (adjacent task arms) or grouped (throughput-oriented)")
	injectFirstStallTasks := flag.String("inject-first-stall-tasks", "", "comma-separated tasks whose first request simulates a stalled worker")
	faultStallSeconds := flag.Int("fault-stall-seconds", 12, "duration of each injected stalled worker")
	limit := flag.Int("limit", 0, "run only first N selected tasks; zero means all")
	selectedTasks := flag.String("tasks", "", "optional comma-separated task IDs")
	environmentID := flag.String("environment-id", "", "stable environment/cohort identifier; required for real runs")
	flag.Parse()
	if *workers < 1 || *hardMinutes < 1 || *repetitions < 1 || *firstProgressSeconds < 1 || *idleSeconds < 1 || *faultStallSeconds < 1 {
		fatalf("workers, repetitions, and timeout values must be positive")
	}
	if strings.TrimSpace(*provider) == "" || strings.TrimSpace(*modelName) == "" {
		fatalf("provider and model must be non-empty")
	}
	var extensions []string
	for _, extension := range strings.Split(*extensionsFlag, ",") {
		if extension = strings.TrimSpace(extension); extension != "" {
			if !fileExists(extension) {
				fatalf("extension does not exist: %s", extension)
			}
			extensions = append(extensions, extension)
		}
	}
	if *driver != "real" && *driver != "deterministic" {
		fatalf("driver must be real or deterministic")
	}
	if *isolation != "bwrap" && *isolation != "macos-sandbox" {
		fatalf("isolation must be bwrap or macos-sandbox")
	}
	if *schedule != "paired" && *schedule != "grouped" {
		fatalf("schedule must be paired or grouped")
	}
	if *driver == "real" && strings.TrimSpace(*environmentID) == "" {
		fatalf("--environment-id is required for real runs so cross-environment samples cannot be pooled silently")
	}
	profiles, err := parseArms(*armsFlag)
	if err != nil {
		fatalf("%v", err)
	}
	faultTasks := map[string]bool{}
	for _, id := range strings.Split(*injectFirstStallTasks, ",") {
		if id = strings.TrimSpace(id); id != "" {
			faultTasks[id] = true
		}
	}
	if *runID == "" {
		*runID = time.Now().UTC().Format("20060102T150405Z")
	}
	runRoot := filepath.Join(*root, *runID)
	must(os.MkdirAll(runRoot, 0o755))

	tasks := allTasks()
	if len(tasks) != 30 {
		fatalf("catalog must contain exactly 30 distinct tasks; got %d", len(tasks))
	}
	must(writeJSON(filepath.Join(runRoot, "task_catalog.json"), tasks))
	if *selectedTasks != "" {
		wanted := map[string]bool{}
		for _, id := range strings.Split(*selectedTasks, ",") {
			wanted[strings.TrimSpace(id)] = true
		}
		filtered := tasks[:0]
		for _, task := range tasks {
			if wanted[task.ID] {
				filtered = append(filtered, task)
			}
		}
		tasks = filtered
		if len(tasks) == 0 {
			fatalf("no selected task IDs matched")
		}
	}
	if *limit > 0 && *limit < len(tasks) {
		tasks = tasks[:*limit]
	}
	must(writeJSON(filepath.Join(runRoot, "selected_task_catalog.json"), tasks))
	must(writeJSON(filepath.Join(runRoot, "arm_catalog.json"), profiles))
	must(writeJSON(filepath.Join(runRoot, "run_metadata.json"), map[string]any{
		"schema_version": 2, "run_id": *runID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"driver": *driver, "isolation": *isolation, "provider": *provider, "model": *modelName, "extensions": extensions, "seed": *seed, "workers_per_cell": *workers, "schedule": *schedule,
		"environment_id": strings.TrimSpace(*environmentID), "git_revision": gitRevision(),
		"selected_task_catalog_sha256": jsonSHA256(tasks), "arm_catalog_sha256": jsonSHA256(profiles),
		"selected_task_catalog_file_sha256": fileSHA256(filepath.Join(runRoot, "selected_task_catalog.json")),
		"arm_catalog_file_sha256":           fileSHA256(filepath.Join(runRoot, "arm_catalog.json")),
		"repetitions":                       *repetitions, "hard_timeout_seconds": *hardMinutes * 60,
		"first_progress_timeout_seconds": *firstProgressSeconds, "idle_timeout_seconds": *idleSeconds,
		"metric_hierarchy":           []string{"request", "attempt", "node", "issue", "suite"},
		"injected_first_stall_tasks": *injectFirstStallTasks, "fault_stall_seconds": *faultStallSeconds,
	}))
	var cells []experimentCell
	if *schedule == "paired" {
		cells = buildPairedCells(profiles, *repetitions, tasks, *seed)
	} else {
		cells = buildCells(profiles, *repetitions, tasks)
		rng := rand.New(rand.NewSource(*seed))
		rng.Shuffle(len(cells), func(i, j int) { cells[i], cells[j] = cells[j], cells[i] })
	}
	for i := range cells {
		cells[i].OrderIndex = i + 1
	}
	must(writeJSON(filepath.Join(runRoot, "randomized_schedule.json"), cells))

	var suites []suiteMetric
	for _, cell := range cells {
		fmt.Printf("CELL %02d/%02d arm=%s repetition=%d issues=%d\n", cell.OrderIndex, len(cells), cell.Arm.ID, cell.Repetition, len(cell.Tasks))
		e := &evaluator{
			runRoot: runRoot, piPath: *piPath, piConfig: *piConfig, provider: *provider, model: *modelName, extensions: extensions, seed: *seed,
			hardTimeout:          time.Duration(*hardMinutes) * time.Minute,
			firstProgressTimeout: time.Duration(*firstProgressSeconds) * time.Second,
			idleTimeout:          time.Duration(*idleSeconds) * time.Second,
			profile:              cell.Arm, repetition: cell.Repetition,
			requestsByWorkspace:        make(map[string][]requestMetric),
			environmentWaitByWorkspace: make(map[string]time.Duration),
			driver:                     *driver,
			isolation:                  *isolation,
			faultTasks:                 faultTasks,
			faultStall:                 time.Duration(*faultStallSeconds) * time.Second,
			faultInjected:              make(map[string]bool),
		}
		cellStarted := time.Now()
		queue := make(chan job)
		var wg sync.WaitGroup
		for i := 0; i < *workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range queue {
					resultPath := filepath.Join(runRoot, "results", fmt.Sprintf("rep-%02d", item.Repetition), item.Profile.ID, item.Task.ID+".json")
					if fileExists(resultPath) {
						continue
					}
					r := e.run(item)
					must(os.MkdirAll(filepath.Dir(resultPath), 0o755))
					must(writeJSON(resultPath, r))
					fmt.Printf("  %03d %-8s %-28s first=%-5v final=%-5v calls=%d ms=%d\n", item.Index, item.Profile.ID, item.Task.ID, r.FirstPassSuccess, r.FinalPassed, r.ModelCalls, r.DurationMS)
				}
			}()
		}
		for index, task := range cell.Tasks {
			queue <- job{Index: index + 1, Profile: cell.Arm, Repetition: cell.Repetition, Task: task}
		}
		close(queue)
		wg.Wait()
		cellFinished := time.Now()
		cellResults := loadResults(filepath.Join(runRoot, "results", fmt.Sprintf("rep-%02d", cell.Repetition), cell.Arm.ID))
		if *schedule == "grouped" {
			suite := summarizeSuite(cell.Arm.ID, cell.Repetition, cellStarted, cellFinished, cellResults)
			suites = append(suites, suite)
			must(writeJSON(filepath.Join(runRoot, "suites", fmt.Sprintf("rep-%02d-%s.json", cell.Repetition, cell.Arm.ID)), suite))
		}
	}

	results := loadResults(filepath.Join(runRoot, "results"))
	if *schedule == "paired" {
		suites = summarizeSerialEquivalentSuites(results)
	}
	must(writeJSON(filepath.Join(runRoot, "summary.json"), results))
	must(writePerJobCSV(filepath.Join(runRoot, "per_job.csv"), results))
	must(writeJSON(filepath.Join(runRoot, "suite_summary.json"), suites))
	printSummary(runRoot, results)
}

func (e *evaluator) run(item job) result {
	started := time.Now()
	dir := filepath.Join(e.runRoot, "workspaces", fmt.Sprintf("rep-%02d", item.Repetition), item.Profile.ID, item.Task.ID)
	_ = os.RemoveAll(dir)
	must(os.MkdirAll(dir, 0o755))
	for name, content := range item.Task.Files {
		path := filepath.Join(dir, name)
		must(os.MkdirAll(filepath.Dir(path), 0o755))
		must(os.WriteFile(path, []byte(content), 0o644))
	}
	r := result{Arm: item.Profile.ID, Repetition: item.Repetition, TaskID: item.Task.ID, Category: item.Task.Category, Difficulty: item.Task.Difficulty, OrderIndex: item.Index, Seed: e.seed, BudgetSeconds: int(e.hardTimeout.Seconds()), StartedAt: started.UTC().Format(time.RFC3339Nano)}
	e.requestMu.Lock()
	e.requestsByWorkspace[dir] = nil
	e.environmentWaitByWorkspace[dir] = 0
	e.requestMu.Unlock()
	var calls int
	var tokens usage
	var events []string
	var first, final bool
	var why string
	switch item.Task.Mode {
	case "single":
		first, final, why, calls, tokens, events = e.runSingle(dir, item.Profile, item.Task)
	case "hidden_retry":
		first, final, why, calls, tokens, events = e.runHiddenRetry(dir, item.Profile, item.Task)
	case "integration":
		first, final, why, calls, tokens, events = e.runIntegration(dir, item.Profile, item.Task)
	case "cross_review":
		first, final, why, calls, tokens, events = e.runCrossReview(dir, item.Profile, item.Task)
	default:
		why = "unknown task mode"
	}
	r.FirstPassSuccess, r.FinalPassed, r.Recovered = first, final, !first && final
	r.WouldRequireHuman = !final
	r.Reason, r.ModelCalls, r.Usage, r.Events = compact(why), calls, tokens, events
	finished := time.Now()
	r.FinishedAt = finished.UTC().Format(time.RFC3339Nano)
	e.requestMu.Lock()
	environmentWait := e.environmentWaitByWorkspace[dir]
	delete(e.environmentWaitByWorkspace, dir)
	r.Requests = append([]requestMetric(nil), e.requestsByWorkspace[dir]...)
	delete(e.requestsByWorkspace, dir)
	e.requestMu.Unlock()
	r.EnvironmentWaitMS = environmentWait.Milliseconds()
	r.DurationMS = (finished.Sub(started) - environmentWait).Milliseconds()
	if r.DurationMS < 1 {
		r.DurationMS = 1
	}
	for _, request := range r.Requests {
		if request.Termination == "no_first_progress" || request.Termination == "idle_no_progress" || request.Termination == "hard_deadline" {
			r.GuardRecoveries++
		}
	}
	return r
}

func (e *evaluator) runSingle(dir string, profile armProfile, task taskSpec) (bool, bool, string, int, usage, []string) {
	st := task.Stages[0]
	if !profile.Runtime {
		u, err := e.pi(dir, "attempt-1", st.Prompt)
		if err != nil {
			return false, false, err.Error(), 1, u, nil
		}
		ok, why := runVerifier(dir, st.Verifier)
		return ok, ok, why, 1, u, nil
	}
	store := workflowruntime.NewMemoryEventStore()
	engine := workflowruntime.NewEngine(store)
	must(engine.Create(workflowruntime.WorkflowSpec{ID: task.ID, Nodes: []workflowruntime.NodeSpec{{ID: st.ID, MaxAttempts: profile.MaxStageAttempts}}}, "create"))
	first, final, why, calls, u := e.executeWithPolicy(engine, task.ID, st, dir, task.RetryFeedback)
	return first, final, why, calls, u, eventNames(engine, task.ID)
}

func (e *evaluator) runHiddenRetry(dir string, profile armProfile, task taskSpec) (bool, bool, string, int, usage, []string) {
	st := task.Stages[0]
	if !profile.Runtime {
		u, err := e.pi(dir, "attempt-1", st.Prompt)
		if err != nil {
			return false, false, err.Error(), 1, u, nil
		}
		ok, why := runVerifier(dir, st.Verifier)
		return ok, ok, why, 1, u, nil
	}
	store := workflowruntime.NewMemoryEventStore()
	engine := workflowruntime.NewEngine(store)
	must(engine.Create(workflowruntime.WorkflowSpec{ID: task.ID, Nodes: []workflowruntime.NodeSpec{{ID: st.ID, MaxAttempts: 2}}}, "create"))
	ok1, why, u1 := e.execute(engine, task.ID, st, dir)
	if ok1 {
		return true, true, why, 1, u1, eventNames(engine, task.ID)
	}
	retry := stage{ID: st.ID, Prompt: task.RetryFeedback, Verifier: st.Verifier}
	ok2, why2, u2 := e.execute(engine, task.ID, retry, dir)
	u1.add(u2)
	return false, ok2, why2, 2, u1, eventNames(engine, task.ID)
}

func (e *evaluator) runIntegration(dir string, profile armProfile, task taskSpec) (bool, bool, string, int, usage, []string) {
	var total usage
	first := true
	if !profile.Runtime {
		for i, st := range task.Stages {
			u, err := e.pi(dir, fmt.Sprintf("%s-%d", st.ID, i+1), st.Prompt)
			total.add(u)
			if err != nil {
				return false, false, err.Error(), i + 1, total, nil
			}
			ok, why := runVerifier(dir, st.Verifier)
			if !ok {
				return false, false, why, i + 1, total, nil
			}
		}
		return first, true, "all stages verified", len(task.Stages), total, nil
	}
	store := workflowruntime.NewMemoryEventStore()
	engine := workflowruntime.NewEngine(store)
	nodes := make([]workflowruntime.NodeSpec, len(task.Stages))
	for i, st := range task.Stages {
		nodes[i] = workflowruntime.NodeSpec{ID: st.ID, MaxAttempts: profile.MaxStageAttempts}
		if i > 0 {
			nodes[i].DependsOn = []string{task.Stages[i-1].ID}
		}
	}
	must(engine.Create(workflowruntime.WorkflowSpec{ID: task.ID, Nodes: nodes}, "create"))
	calls := 0
	for i, st := range task.Stages {
		retryFeedback := ""
		if i == 0 {
			retryFeedback = task.RetryFeedback
		}
		stageFirst, ok, why, stageCalls, u := e.executeWithPolicy(engine, task.ID, st, dir, retryFeedback)
		calls += stageCalls
		total.add(u)
		first = first && stageFirst
		if !ok {
			return first, false, why, calls, total, eventNames(engine, task.ID)
		}
		_ = i
	}
	return first, true, "workflow succeeded", calls, total, eventNames(engine, task.ID)
}

func (e *evaluator) runCrossReview(dir string, profile armProfile, task taskSpec) (bool, bool, string, int, usage, []string) {
	var total usage
	if !profile.Runtime {
		// The legacy loop can diagnose, delegate the fix, and resume integration,
		// but it has no live-plan amendment primitive for the requested Review gate.
		calls := 0
		for _, idx := range []int{0, 1, 3} {
			st := task.Stages[idx]
			u, err := e.pi(dir, fmt.Sprintf("stage-%d-%s", idx+1, st.ID), st.Prompt)
			total.add(u)
			calls++
			if err != nil {
				return false, false, err.Error(), calls, total, nil
			}
			if ok, why := runVerifier(dir, st.Verifier); !ok {
				return false, false, why, calls, total, nil
			}
		}
		ok, why := runVerifier(dir, task.Stages[3].Verifier)
		return ok, ok, why, calls, total, nil
	}
	w1Store, w2Store := workflowruntime.NewMemoryEventStore(), workflowruntime.NewMemoryEventStore()
	w1, w2 := workflowruntime.NewEngine(w1Store), workflowruntime.NewEngine(w2Store)
	integrationAttempts := 2
	if profile.ClassifiedRetry {
		integrationAttempts++
	}
	must(w1.Create(workflowruntime.WorkflowSpec{ID: task.ID + "-w1", Nodes: []workflowruntime.NodeSpec{{ID: "integration", MaxAttempts: integrationAttempts}}}, "create-w1"))
	must(w2.Create(workflowruntime.WorkflowSpec{ID: task.ID + "-w2", Nodes: []workflowruntime.NodeSpec{{ID: "fix", MaxAttempts: profile.MaxStageAttempts}}}, "create-w2"))
	// Workflow 1 diagnoses and is rejected with a dependency gap.
	lease, err := w1.Claim(task.ID+"-w1", "integration", "claim-diagnose")
	if err != nil {
		return false, false, err.Error(), 0, total, eventNames(w1, task.ID+"-w1")
	}
	u, err := e.pi(dir, "diagnose", task.Stages[0].Prompt)
	total.add(u)
	if err != nil {
		return false, false, err.Error(), 1, total, eventNames(w1, task.ID+"-w1")
	}
	must(w1.ReportTaskSucceeded(lease, "report-diagnose"))
	ok, why := runVerifier(dir, task.Stages[0].Verifier)
	must(w1.Verify(lease, false, workflowruntime.FailureDependencyGap, "reject-diagnose"))
	if !ok {
		return false, false, why, 1, total, eventNames(w1, task.ID+"-w1")
	}
	// Workflow 2 fixes the dependency.
	_, ok, why, fixCalls, u := e.executeWithPolicy(w2, task.ID+"-w2", task.Stages[1], dir, "")
	total.add(u)
	if !ok {
		return false, false, why, 1 + fixCalls, total, append(eventNames(w1, task.ID+"-w1"), eventNames(w2, task.ID+"-w2")...)
	}
	// Runtime inserts Review before the waiting integration node.
	must(w1.AddNodeBefore(task.ID+"-w1", "integration", workflowruntime.NodeSpec{ID: "review", MaxAttempts: profile.MaxStageAttempts}, "insert-review"))
	_, ok, why, reviewCalls, u := e.executeWithPolicy(w1, task.ID+"-w1", task.Stages[2], dir, "")
	total.add(u)
	if !ok {
		return false, false, why, 1 + fixCalls + reviewCalls, total, eventNames(w1, task.ID+"-w1")
	}
	_, ok, why, integrationCalls, u := e.executeWithPolicy(w1, task.ID+"-w1", task.Stages[3], dir, "")
	total.add(u)
	events := append(eventNames(w1, task.ID+"-w1"), eventNames(w2, task.ID+"-w2")...)
	return false, ok, why, 1 + fixCalls + reviewCalls + integrationCalls, total, events
}

func (e *evaluator) execute(engine *workflowruntime.Engine, runID string, st stage, dir string) (bool, string, usage) {
	lease, err := engine.Claim(runID, st.ID, fmt.Sprintf("claim-%s-%d", st.ID, time.Now().UnixNano()))
	if err != nil {
		return false, err.Error(), usage{}
	}
	u, piErr := e.pi(dir, fmt.Sprintf("%s-a%d", st.ID, lease.Attempt), st.Prompt)
	if piErr != nil {
		_ = engine.FailAttempt(lease, classifyExecutionFailure(piErr), "fail-"+lease.AttemptID)
		return false, piErr.Error(), u
	}
	must(engine.ReportTaskSucceeded(lease, "report-"+lease.AttemptID))
	ok, why := runVerifier(dir, st.Verifier)
	must(engine.Verify(lease, ok, workflowruntime.FailureCodeDefect, "verify-"+lease.AttemptID))
	return ok, why, u
}

func classifyExecutionFailure(err error) workflowruntime.FailureClass {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "no_first_progress"), strings.Contains(message, "idle_no_progress"), strings.Contains(message, "injected_worker_disconnect"):
		return workflowruntime.FailureNoProgress
	case strings.Contains(message, "hard deadline"):
		return workflowruntime.FailureDeadlineExceeded
	default:
		return workflowruntime.FailureEnvironmentGap
	}
}

func (e *evaluator) executeWithPolicy(engine *workflowruntime.Engine, runID string, st stage, dir, retryFeedback string) (bool, bool, string, int, usage) {
	ok, why, total := e.execute(engine, runID, st, dir)
	if ok || !e.profile.ClassifiedRetry {
		return ok, ok, why, 1, total
	}
	retry := st
	evidence := "The independent verifier rejected the prior attempt: " + compact(why) + "."
	if strings.TrimSpace(retryFeedback) != "" {
		evidence = strings.TrimSpace(retryFeedback) + "\n\nRaw verifier evidence: " + compact(why) + "."
	}
	retry.Prompt = st.Prompt + "\n\n" + evidence + " Inspect the current workspace, correct only the verified defect, and rerun the required checks."
	ok, why, next := e.execute(engine, runID, retry, dir)
	total.add(next)
	return false, ok, why, 2, total
}

func (e *evaluator) pi(dir, label, prompt string) (usage, error) {
	if e.driver == "deterministic" {
		return e.deterministicPi(dir, label, prompt)
	}
	if e.takeInjectedFault(dir) {
		return e.runInjectedStall(dir, label)
	}
	logDir := filepath.Join(dir, "model-logs")
	must(os.MkdirAll(logDir, 0o755))
	sessionName := label + ".session.jsonl"
	if e.profile.SessionReuse {
		sessionName = "issue.session.jsonl"
	}
	sessionHost := filepath.Join(logDir, sessionName)
	guestSession := filepath.Join("/workspace", "model-logs", sessionName)
	reused := e.profile.SessionReuse && fileExists(sessionHost)
	sessionExisted := fileExists(sessionHost)
	var sessionSnapshot []byte
	if sessionExisted {
		sessionSnapshot, _ = os.ReadFile(sessionHost)
	}
	usageBefore := parseUsage(sessionHost)
	piArgs := []string{"-p", "--mode", "json", "--provider", e.provider, "--model", e.model, "--no-context-files", "--no-skills", "--no-extensions", "--no-prompt-templates", "--no-themes", "--approve"}
	for _, extension := range e.extensions {
		piArgs = append(piArgs, "--extension", extension)
	}
	newCommand := func() *exec.Cmd {
		argsForPi := append([]string(nil), piArgs...)
		var cmd *exec.Cmd
		if e.isolation == "macos-sandbox" {
			argsForPi = append(argsForPi, "--session", sessionHost)
			profile := fmt.Sprintf("(version 1)(allow default)(deny file-write* (subpath %q))(allow file-write* (subpath %q))(allow file-write* (subpath %q))", filepath.Dir(e.runRoot), dir, os.TempDir())
			cmd = exec.Command("/usr/bin/sandbox-exec", append([]string{"-p", profile, e.piPath}, argsForPi...)...)
			cmd.Dir = dir
			cmd.Env = append(cleanProxyEnv(os.Environ()), "PI_CODING_AGENT_DIR="+e.piConfig, "PI_TELEMETRY=0")
		} else {
			toolRoot := filepath.Clean(filepath.Join(filepath.Dir(e.piPath), "..", ".."))
			guestPi := "/tooling/node_modules/.bin/pi"
			args := []string{"--die-with-parent", "--new-session", "--unshare-all", "--share-net", "--ro-bind", "/usr", "/usr", "--ro-bind", "/bin", "/bin", "--ro-bind", "/lib", "/lib", "--ro-bind", "/lib64", "/lib64", "--ro-bind", "/etc", "/etc", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/home", "--dir", "/home/agent", "--bind", dir, "/workspace"}
			if fileExists("/mnt/wsl/resolv.conf") {
				args = append(args, "--dir", "/mnt", "--dir", "/mnt/wsl", "--ro-bind", "/mnt/wsl/resolv.conf", "/mnt/wsl/resolv.conf")
			}
			argsForPi = append(argsForPi, "--session", guestSession)
			args = append(args, "--ro-bind", toolRoot, "/tooling", "--ro-bind", e.piConfig, "/pi-config", "--chdir", "/workspace", guestPi)
			args = append(args, argsForPi...)
			cmd = exec.Command("bwrap", args...)
			cmd.Dir = "/"
			cmd.Env = append(cleanProxyEnv(os.Environ()), "PI_CODING_AGENT_DIR=/pi-config", "PI_TELEMETRY=0", "HOME=/home/agent")
		}
		cmd.Stdin = strings.NewReader(prompt)
		return cmd
	}
	quotaResponses := 0
	var observed observedCommandResult
	for {
		observed = runObservedCommand(newCommand(), observedCommandOptions{
			Guard:                e.profile.ProgressGuard,
			HardTimeout:          e.hardTimeout,
			FirstProgressTimeout: e.firstProgressTimeout,
			IdleTimeout:          e.idleTimeout,
			Workspace:            dir,
			SessionPath:          sessionHost,
		})
		attemptUsage := parseUsage(sessionHost)
		attemptUsage.subtract(usageBefore)
		if !isProviderQuotaResponse(observed.Output, sessionHost, attemptUsage) {
			break
		}
		quotaResponses++
		must(os.WriteFile(filepath.Join(logDir, fmt.Sprintf("%s.quota-%02d.log", label, quotaResponses)), observed.Output, 0o644))
		if sessionExisted {
			must(os.WriteFile(sessionHost, sessionSnapshot, 0o644))
		} else if err := os.Remove(sessionHost); err != nil && !os.IsNotExist(err) {
			must(err)
		}
		waitUntil := time.Now().Truncate(time.Hour).Add(time.Hour).Add(time.Duration(5+(e.seed%5)*3) * time.Second)
		wait := time.Until(waitUntil)
		if wait < time.Second {
			wait = time.Second
		}
		fmt.Printf("  provider quota arm=%s task=%s; pausing %s until %s\n", e.profile.ID, filepath.Base(dir), wait.Round(time.Second), waitUntil.Format(time.RFC3339))
		waitStarted := time.Now()
		time.Sleep(wait)
		e.requestMu.Lock()
		e.environmentWaitByWorkspace[dir] += time.Since(waitStarted)
		e.requestMu.Unlock()
	}
	must(os.WriteFile(filepath.Join(logDir, label+".output.log"), observed.Output, 0o644))
	u := parseUsage(sessionHost)
	if reused {
		u.subtract(usageBefore)
	}
	metric := metricForObserved(label, reused, observed)
	e.requestMu.Lock()
	e.requestsByWorkspace[dir] = append(e.requestsByWorkspace[dir], metric)
	e.requestMu.Unlock()
	if err := observedFailure(observed); err != nil {
		if observed.Termination == "hard_deadline" {
			return u, fmt.Errorf("hard deadline after %s", e.hardTimeout)
		}
		return u, fmt.Errorf("Pi/DeepSeek failed (%s): %v", observed.Termination, err)
	}
	return u, nil
}

func isProviderQuotaResponse(output []byte, sessionPath string, attemptUsage usage) bool {
	if attemptUsage.Total != 0 || attemptUsage.Input != 0 || attemptUsage.Output != 0 || attemptUsage.CacheRead != 0 || attemptUsage.CacheWrite != 0 {
		return false
	}
	payload := append([]byte(nil), output...)
	if session, err := os.ReadFile(sessionPath); err == nil {
		payload = append(payload, session...)
	}
	text := strings.ToLower(string(payload))
	return strings.Contains(text, "cost-quota-") || strings.Contains(text, "当前小时请求过于频繁，请下个整点重试")
}

func runVerifier(dir string, v verifier) (bool, string) {
	for name, terms := range v.RequiredFiles {
		payload, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return false, "missing artifact " + name
		}
		lower := strings.ToLower(string(payload))
		for _, term := range terms {
			if !strings.Contains(lower, strings.ToLower(term)) {
				return false, fmt.Sprintf("%s missing concept %q", name, term)
			}
		}
	}
	if len(v.Command) > 0 {
		cmd := exec.Command(v.Command[0], v.Command[1:]...)
		cmd.Dir = dir
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			return false, compact(out.String())
		}
		return true, compact(out.String())
	}
	return true, "artifact checks passed"
}

func parseUsage(path string) usage {
	file, err := os.Open(path)
	if err != nil {
		return usage{}
	}
	defer file.Close()
	var total usage
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event struct {
			Message struct {
				Usage struct {
					Input, Output, CacheRead, CacheWrite, TotalTokens int64
					Cost                                              struct {
						Total float64 `json:"total"`
					} `json:"cost"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		total.Input += event.Message.Usage.Input
		total.Output += event.Message.Usage.Output
		total.CacheRead += event.Message.Usage.CacheRead
		total.CacheWrite += event.Message.Usage.CacheWrite
		total.Total += event.Message.Usage.TotalTokens
		total.Cost += event.Message.Usage.Cost.Total
	}
	return total
}

func (u *usage) add(other usage) {
	u.Input += other.Input
	u.Output += other.Output
	u.CacheRead += other.CacheRead
	u.CacheWrite += other.CacheWrite
	u.Total += other.Total
	u.Cost += other.Cost
}

func (u *usage) subtract(previous usage) {
	u.Input -= previous.Input
	u.Output -= previous.Output
	u.CacheRead -= previous.CacheRead
	u.CacheWrite -= previous.CacheWrite
	u.Total -= previous.Total
	u.Cost -= previous.Cost
	if u.Input < 0 {
		u.Input = 0
	}
	if u.Output < 0 {
		u.Output = 0
	}
	if u.CacheRead < 0 {
		u.CacheRead = 0
	}
	if u.CacheWrite < 0 {
		u.CacheWrite = 0
	}
	if u.Total < 0 {
		u.Total = 0
	}
	if u.Cost < 0 {
		u.Cost = 0
	}
}

func eventNames(engine *workflowruntime.Engine, runID string) []string {
	events := engine.Events(runID)
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, string(event.Type))
	}
	return names
}

func loadResults(root string) []result {
	var results []result
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		var r result
		payload, readErr := os.ReadFile(path)
		if readErr == nil && json.Unmarshal(payload, &r) == nil {
			results = append(results, r)
		}
		return nil
	})
	sort.Slice(results, func(i, j int) bool {
		if results[i].Repetition != results[j].Repetition {
			return results[i].Repetition < results[j].Repetition
		}
		if results[i].Arm != results[j].Arm {
			return results[i].Arm < results[j].Arm
		}
		return results[i].OrderIndex < results[j].OrderIndex
	})
	return results
}

func printSummary(root string, results []result) {
	fmt.Println("evidence:", root)
	armSet := map[string]bool{}
	for _, item := range results {
		armSet[item.Arm] = true
	}
	var arms []string
	for arm := range armSet {
		arms = append(arms, arm)
	}
	sort.Strings(arms)
	for _, arm := range arms {
		var n, first, final, recovered, calls int
		var ms []int64
		var tokens int64
		for _, r := range results {
			if r.Arm == arm {
				n++
				if r.FirstPassSuccess {
					first++
				}
				if r.FinalPassed {
					final++
				}
				if r.Recovered {
					recovered++
				}
				calls += r.ModelCalls
				ms = append(ms, r.DurationMS)
				tokens += r.Usage.Total
			}
		}
		if n == 0 {
			continue
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
		p95 := ms[(95*n+99)/100-1]
		fmt.Printf("%s n=%d first=%d/%d final=%d/%d recovered=%d calls=%d tokens=%d wall_p95_ms=%d\n", arm, n, first, n, final, n, recovered, calls, tokens, p95)
	}
}

func cleanProxyEnv(env []string) []string {
	blocked := map[string]bool{"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "http_proxy": true, "https_proxy": true, "all_proxy": true}
	out := make([]string, 0, len(env))
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		if !blocked[key] {
			out = append(out, value)
		}
	}
	return out
}
func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }
func compact(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 500 {
		return value[:500] + "..."
	}
	return value
}
func writeJSON(path string, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(payload, '\n'), 0o644)
}

func jsonSHA256(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		fatalf("hash JSON: %v", err)
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum[:])
}

func fileSHA256(path string) string {
	payload, err := os.ReadFile(path)
	if err != nil {
		fatalf("hash file %s: %v", path, err)
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum[:])
}

func gitRevision() string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	payload, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(payload))
}

func writePerJobCSV(path string, results []result) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	handle, err := os.Create(path)
	if err != nil {
		return err
	}
	defer handle.Close()
	w := csv.NewWriter(handle)
	defer w.Flush()
	if err := w.Write([]string{"pair_key", "seed", "repetition", "task_id", "arm", "category", "difficulty", "final_passed", "first_pass_success", "recovered", "duration_ms", "environment_wait_ms", "model_calls", "total_tokens", "cache_read_tokens", "guard_recoveries", "reused_requests", "request_count", "started_at", "finished_at", "termination_reasons"}); err != nil {
		return err
	}
	for _, item := range results {
		reused := 0
		terminations := make([]string, 0, len(item.Requests))
		for _, request := range item.Requests {
			if request.SessionReused {
				reused++
			}
			terminations = append(terminations, request.Termination)
		}
		row := []string{
			fmt.Sprintf("%d/%d/%s", item.Seed, item.Repetition, item.TaskID), strconv.FormatInt(item.Seed, 10), strconv.Itoa(item.Repetition), item.TaskID, item.Arm, item.Category, item.Difficulty,
			strconv.FormatBool(item.FinalPassed), strconv.FormatBool(item.FirstPassSuccess), strconv.FormatBool(item.Recovered), strconv.FormatInt(item.DurationMS, 10), strconv.FormatInt(item.EnvironmentWaitMS, 10), strconv.Itoa(item.ModelCalls), strconv.FormatInt(item.Usage.Total, 10), strconv.FormatInt(item.Usage.CacheRead, 10), strconv.Itoa(item.GuardRecoveries), strconv.Itoa(reused), strconv.Itoa(len(item.Requests)), item.StartedAt, item.FinishedAt, strings.Join(terminations, ";"),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return w.Error()
}
func must(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}
func fatalf(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...); os.Exit(1) }
