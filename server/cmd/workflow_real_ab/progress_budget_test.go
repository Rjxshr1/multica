package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/workflowruntime"
)

func TestRunWithProgressBudgetStopsLiveButIdleAgent(t *testing.T) {
	started := time.Now()
	cmd := exec.Command("sh", "-c", "sleep 5")
	err := runWithProgressBudget(context.Background(), cmd, progressBudget{
		HardTimeout:          time.Second,
		FirstProgressTimeout: 60 * time.Millisecond,
		IdleTimeout:          60 * time.Millisecond,
		PollInterval:         5 * time.Millisecond,
	}, func() bool { return false })
	if !errors.Is(err, errNoProgress) {
		t.Fatalf("error = %v, want errNoProgress", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("idle agent stopped after %s, want under 500ms", elapsed)
	}
}

func TestRunWithProgressBudgetAllowsContinuedProgress(t *testing.T) {
	started := time.Now()
	cmd := exec.Command("sh", "-c", "sleep 0.18")
	err := runWithProgressBudget(context.Background(), cmd, progressBudget{
		HardTimeout:          time.Second,
		FirstProgressTimeout: 80 * time.Millisecond,
		IdleTimeout:          80 * time.Millisecond,
		PollInterval:         5 * time.Millisecond,
	}, func() bool {
		elapsed := time.Since(started)
		return elapsed >= 30*time.Millisecond && elapsed < 170*time.Millisecond
	})
	if err != nil {
		t.Fatalf("progressing agent failed: %v", err)
	}
}

func TestRunWithProgressBudgetDoesNotRequireEarlyArtifact(t *testing.T) {
	started := time.Now()
	cmd := exec.Command("sh", "-c", "sleep 0.22")
	err := runWithProgressBudget(context.Background(), cmd, progressBudget{
		HardTimeout:          time.Second,
		FirstProgressTimeout: 70 * time.Millisecond,
		IdleTimeout:          70 * time.Millisecond,
		PollInterval:         5 * time.Millisecond,
	}, func() bool {
		// Simulate fresh read/query checkpoints without a file artifact.
		elapsed := time.Since(started)
		phase := int(elapsed / (30 * time.Millisecond))
		return phase > 0 && phase < 7 && elapsed%(30*time.Millisecond) < 6*time.Millisecond
	})
	if err != nil {
		t.Fatalf("complex discovery task with fresh checkpoints failed: %v", err)
	}
}

func TestSessionSemanticProbeIgnoresThinkingAndDeduplicatesToolEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	appendLine := func(line string) {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	probe := newSessionSemanticProgressProbe(path)
	appendLine(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":12,"uuid":"a"}`)
	if probe() {
		t.Fatal("thinking tokens must not renew the progress lease")
	}
	appendLine(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"core/a.py"}}]},"uuid":"first"}`)
	if !probe() {
		t.Fatal("a new directed tool query should count as progress")
	}
	appendLine(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-2","name":"Read","input":{"file_path":"core/a.py"}}]},"uuid":"repeat"}`)
	if probe() {
		t.Fatal("an identical repeated query must not keep renewing the lease")
	}
	appendLine(`{"type":"assistant","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"def build(): pass"}]}}`)
	if !probe() {
		t.Fatal("a new tool result should count as progress")
	}
}

func TestProgressBudgetCanBeConfiguredPerNode(t *testing.T) {
	budget := progressBudgetForNode(workflowruntime.NodeSpec{
		ExecutionBudget: &workflowruntime.ExecutionBudgetSpec{
			FirstProgressTimeoutSeconds: 600,
			IdleTimeoutSeconds:          420,
			HardTimeoutSeconds:          3600,
		},
	})
	if budget.FirstProgressTimeout != 10*time.Minute ||
		budget.IdleTimeout != 7*time.Minute ||
		budget.HardTimeout != time.Hour {
		t.Fatalf("unexpected node budget: %#v", budget)
	}
}

func TestSandboxExposesFixtureButNotSiblingHistory(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "fixture")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "previous-run-answer.json")
	if err := os.WriteFile(secret, []byte(`{"hidden":"answer"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := sandboxedCommand(workspace, nil, "/bin/sh", "-c", "test ! -e '"+secret+"' && printf isolated > /workspace/result.txt")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandbox command failed: %v: %s", err, output)
	}
	payload, err := os.ReadFile(filepath.Join(workspace, "result.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "isolated" {
		t.Fatalf("result = %q, want isolated", payload)
	}
}

func TestTailBufferBoundsCapturedModelOutput(t *testing.T) {
	buffer := tailBuffer{max: 8}
	for _, chunk := range []string{"abcd", "efgh", "ijkl"} {
		if _, err := buffer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := buffer.String(); got != "efghijkl" {
		t.Fatalf("tail = %q, want efghijkl", got)
	}
	if !buffer.Truncated() || buffer.total != 12 {
		t.Fatalf("truncated=%v total=%d, want true and 12", buffer.Truncated(), buffer.total)
	}
}
