//go:build agentintegration

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/agent"
)

const launchABModel = "deepseek-anthropic/deepseek-v4-pro[1m]"

type launchABResult struct {
	Arm                     string `json:"arm"`
	SkillCount              int    `json:"skill_count"`
	Repetition              int    `json:"repetition"`
	ProcessStartMS          int64  `json:"process_start_ms"`
	FirstSemanticActivityMS int64  `json:"first_semantic_activity_ms"`
	TotalMS                 int64  `json:"total_ms"`
	Status                  string `json:"status"`
	Output                  string `json:"output,omitempty"`
	Error                   string `json:"error,omitempty"`
	RuntimeResolveMS        int64  `json:"runtime_resolve_ms,omitempty"`
	SkillResolveMS          int64  `json:"skill_resolve_ms,omitempty"`
	RemoteMCPMS             int64  `json:"remote_mcp_ms,omitempty"`
	EnvPrepareMS            int64  `json:"env_prepare_ms,omitempty"`
	StartTaskRPCMS          int64  `json:"start_task_rpc_ms,omitempty"`
	BackendSetupMS          int64  `json:"backend_setup_ms,omitempty"`
	ReadyToExecuteMS        int64  `json:"ready_to_execute_ms,omitempty"`
}

type launchABLockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *launchABLockedBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(payload)
}

func (b *launchABLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestPiDeepSeekLaunchLatencyAB compares a pre-prepared local Pi invocation
// with the complete Multica daemon launch path. It is intentionally opt-in:
// the test uses the operator's authenticated Pi/DeepSeek configuration and
// consumes provider quota. Both arms use the same prompt, model, generated
// runtime brief, and skill files.
func TestPiDeepSeekLaunchLatencyAB(t *testing.T) {
	if os.Getenv("MULTICA_RUN_REAL_AGENT_SMOKE") != "1" {
		t.Skip("set MULTICA_RUN_REAL_AGENT_SMOKE=1 to run the authenticated launch A/B")
	}
	piPath := strings.TrimSpace(os.Getenv("MULTICA_REAL_AB_PI_PATH"))
	if piPath == "" {
		t.Fatal("MULTICA_REAL_AB_PI_PATH is required")
	}
	if _, err := os.Stat(piPath); err != nil {
		t.Fatalf("Pi executable: %v", err)
	}
	if strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")) == "" {
		t.Fatal("PI_CODING_AGENT_DIR is required")
	}

	repetitions := 1
	if raw := strings.TrimSpace(os.Getenv("MULTICA_LAUNCH_AB_REPETITIONS")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			t.Fatalf("invalid MULTICA_LAUNCH_AB_REPETITIONS %q", raw)
		}
		repetitions = value
	}

	for repetition := 1; repetition <= repetitions; repetition++ {
		for _, skillCount := range []int{0, 16} {
			skills := launchABSkills(skillCount)
			prompt := "Reply with exactly READY and do not call tools."
			if repetition%2 == 1 {
				emitLaunchABResult(t, runDirectLaunchAB(t, piPath, prompt, skills, repetition))
				emitLaunchABResult(t, runMulticaLaunchAB(t, piPath, prompt, skills, repetition))
			} else {
				emitLaunchABResult(t, runMulticaLaunchAB(t, piPath, prompt, skills, repetition))
				emitLaunchABResult(t, runDirectLaunchAB(t, piPath, prompt, skills, repetition))
			}
		}
	}
}

func runDirectLaunchAB(t *testing.T, piPath, prompt string, skills []SkillData, repetition int) launchABResult {
	t.Helper()
	prompt = BuildPrompt(Task{WorkflowContext: launchABWorkflowContext(prompt)}, "pi")
	taskContext := launchABTaskContext(skills)
	env, err := execenv.Prepare(execenv.PrepareParams{
		WorkspacesRoot: t.TempDir(),
		WorkspaceID:    "ws-direct",
		TaskID:         fmt.Sprintf("direct-%d-%d", repetition, len(skills)),
		AgentName:      "Launch A/B",
		Provider:       "pi",
		Task:           taskContext,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("prepare direct fixture: %v", err)
	}
	t.Cleanup(func() { _ = env.Cleanup(true) })
	if _, err := execenv.InjectRuntimeConfig(env.WorkDir, "pi", taskContext); err != nil {
		t.Fatalf("inject direct runtime config: %v", err)
	}

	backend, err := agent.ResolveBackend("pi", agent.Config{
		ExecutablePath: piPath,
		Env:            launchABAgentEnv(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("create direct backend: %v", err)
	}

	started := time.Now()
	session, err := backend.Execute(context.Background(), prompt, agent.ExecOptions{
		Cwd: env.WorkDir, Model: launchABModel, Timeout: 3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("start direct Pi: %v", err)
	}
	processStartedAt := time.Now()
	firstSemanticAt := time.Time{}
	for message := range session.Messages {
		if message.Type != agent.MessageStatus && firstSemanticAt.IsZero() {
			firstSemanticAt = time.Now()
		}
	}
	result := <-session.Result
	finishedAt := time.Now()
	if firstSemanticAt.IsZero() {
		firstSemanticAt = finishedAt
	}
	return launchABResult{
		Arm: "local-direct", SkillCount: len(skills), Repetition: repetition,
		ProcessStartMS:          processStartedAt.Sub(started).Milliseconds(),
		FirstSemanticActivityMS: firstSemanticAt.Sub(started).Milliseconds(),
		TotalMS:                 finishedAt.Sub(started).Milliseconds(), Status: result.Status,
		Output: strings.TrimSpace(result.Output), Error: result.Error,
	}
}

func runMulticaLaunchAB(t *testing.T, piPath, prompt string, skills []SkillData, repetition int) launchABResult {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)

	var logs launchABLockedBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	d := &Daemon{
		client: NewClient(srv.URL), logger: logger,
		workspaces:     make(map[string]*workspaceState),
		runtimeIndex:   map[string]Runtime{"rt-pi": {ID: "rt-pi", Provider: "pi"}},
		activeEnvRoots: make(map[string]int),
		cfg: Config{
			WorkspacesRoot: t.TempDir(), AgentTimeout: 3 * time.Minute, ServerBaseURL: srv.URL,
			Agents: map[string]AgentEntry{"pi": {Path: piPath, Model: launchABModel}},
		},
	}
	task := Task{
		ID:          fmt.Sprintf("multica-%d-%d", repetition, len(skills)),
		WorkspaceID: "ws-multica", RuntimeID: "rt-pi", IssueID: "issue-launch-ab",
		AgentID: "agent-launch-ab", AuthToken: "mat_launch_ab",
		WorkflowContext: launchABWorkflowContext(prompt),
		Agent: &AgentData{
			ID: "agent-launch-ab", Name: "Launch A/B", Model: launchABModel, Skills: skills,
			CustomEnv: launchABAgentEnv(),
		},
	}

	started := time.Now()
	result, err := d.runTask(context.Background(), task, "pi", 0, logger)
	finishedAt := time.Now()
	if err != nil {
		t.Fatalf("run Multica task: %v\nlogs:\n%s", err, logs.String())
	}
	metrics := launchABMetrics(logs.String())
	resultError := ""
	if result.Status != "completed" {
		resultError = result.Comment
	}
	return launchABResult{
		Arm: "multica-cold", SkillCount: len(skills), Repetition: repetition,
		ProcessStartMS:          metrics["backend_start_ms"],
		FirstSemanticActivityMS: metrics["ready_to_execute_ms"] + metrics["after_execute_call_ms"],
		TotalMS:                 finishedAt.Sub(started).Milliseconds(), Status: result.Status,
		Output: strings.TrimSpace(result.Comment), Error: resultError,
		RuntimeResolveMS: metrics["runtime_resolve_ms"], SkillResolveMS: metrics["skill_resolve_ms"],
		RemoteMCPMS: metrics["remote_mcp_ms"], EnvPrepareMS: metrics["env_prepare_ms"],
		StartTaskRPCMS: metrics["start_task_rpc_ms"], BackendSetupMS: metrics["backend_setup_ms"],
		ReadyToExecuteMS: metrics["ready_to_execute_ms"],
	}
}

func launchABWorkflowContext(prompt string) *WorkflowContextBootstrapData {
	return &WorkflowContextBootstrapData{
		SnapshotID: "snapshot-launch-ab", ContextRevision: 1, SourceDigest: "launch-ab",
		Task:     json.RawMessage(fmt.Sprintf(`{"instruction":%q}`, prompt)),
		Workflow: json.RawMessage(`{"nodes":["launch-ab"]}`),
	}
}

func launchABTaskContext(skills []SkillData) execenv.TaskContextForEnv {
	converted := make([]execenv.SkillContextForEnv, 0, len(skills))
	for _, skill := range skills {
		converted = append(converted, execenv.SkillContextForEnv{
			Name: skill.Name, Description: skill.Description, Content: skill.Content,
		})
	}
	return execenv.TaskContextForEnv{
		IssueID: "issue-launch-ab", AgentID: "agent-launch-ab", AgentName: "Launch A/B",
		AgentSkills: converted,
	}
}

func launchABSkills(count int) []SkillData {
	skills := make([]SkillData, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("launch-ab-%02d", i)
		skills = append(skills, SkillData{
			ID: name, Source: "workspace", Name: name,
			Description: "Synthetic launch A/B skill; use only when explicitly requested.",
			Content:     "This skill is not needed for the launch latency probe.\n",
		})
	}
	return skills
}

func launchABAgentEnv() map[string]string {
	return map[string]string{
		"PI_CODING_AGENT_DIR": os.Getenv("PI_CODING_AGENT_DIR"),
		"PI_TELEMETRY":        "0",
		"HTTP_PROXY":          "",
		"HTTPS_PROXY":         "",
		"ALL_PROXY":           "",
		"http_proxy":          "",
		"https_proxy":         "",
		"all_proxy":           "",
	}
}

func launchABMetrics(raw string) map[string]int64 {
	metrics := make(map[string]int64)
	for _, line := range strings.Split(raw, "\n") {
		var record map[string]any
		if strings.TrimSpace(line) == "" || json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		for key, value := range record {
			if number, ok := value.(float64); ok {
				metrics[key] = int64(number)
			}
		}
	}
	return metrics
}

func emitLaunchABResult(t *testing.T, result launchABResult) {
	t.Helper()
	if result.Status != "completed" {
		t.Fatalf("launch A/B arm %s with %d skills failed: %s", result.Arm, result.SkillCount, result.Error)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("LAUNCH_AB %s", payload)
}
