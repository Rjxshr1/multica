package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	errNoProgress   = errors.New("agent made no durable progress")
	errHardDeadline = errors.New("agent exceeded hard deadline")
)

type progressBudget struct {
	HardTimeout          time.Duration
	FirstProgressTimeout time.Duration
	IdleTimeout          time.Duration
	PollInterval         time.Duration
	OnProgress           func()
}

func runWithProgressBudget(ctx context.Context, cmd *exec.Cmd, budget progressBudget, progressed func() bool) error {
	if budget.PollInterval <= 0 {
		budget.PollInterval = time.Second
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	startedAt := time.Now()
	lastProgressAt := startedAt
	hasProgress := false
	ticker := time.NewTicker(budget.PollInterval)
	defer ticker.Stop()

	stop := func(cause error) error {
		_ = cmd.Process.Kill()
		<-done
		return cause
	}
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return stop(ctx.Err())
		case now := <-ticker.C:
			if progressed != nil && progressed() {
				hasProgress = true
				lastProgressAt = now
				if budget.OnProgress != nil {
					budget.OnProgress()
				}
			}
			if budget.HardTimeout > 0 && now.Sub(startedAt) >= budget.HardTimeout {
				return stop(fmt.Errorf("%w after %s", errHardDeadline, budget.HardTimeout))
			}
			if !hasProgress && budget.FirstProgressTimeout > 0 && now.Sub(startedAt) >= budget.FirstProgressTimeout {
				return stop(fmt.Errorf("%w within %s", errNoProgress, budget.FirstProgressTimeout))
			}
			if hasProgress && budget.IdleTimeout > 0 && now.Sub(lastProgressAt) >= budget.IdleTimeout {
				return stop(fmt.Errorf("%w for %s", errNoProgress, budget.IdleTimeout))
			}
		}
	}
}

type fileStamp struct {
	Size    int64
	ModTime int64
}

func newFilesystemProgressProbe(root string, ignored ...string) func() bool {
	ignoredPaths := make([]string, 0, len(ignored))
	for _, path := range ignored {
		absolute, err := filepath.Abs(path)
		if err == nil {
			ignoredPaths = append(ignoredPaths, filepath.Clean(absolute))
		}
	}
	snapshot := scanFileStamps(root, ignoredPaths)
	return func() bool {
		next := scanFileStamps(root, ignoredPaths)
		changed := len(next) != len(snapshot)
		if !changed {
			for path, stamp := range next {
				if snapshot[path] != stamp {
					changed = true
					break
				}
			}
		}
		snapshot = next
		return changed
	}
}

// newCompositeProgressProbe reports progress when any child probe reports a
// new, independently verifiable checkpoint.
func newCompositeProgressProbe(probes ...func() bool) func() bool {
	return func() bool {
		progressed := false
		for _, probe := range probes {
			if probe != nil && probe() {
				progressed = true
			}
		}
		return progressed
	}
}

// newSessionSemanticProgressProbe tails a JSONL session but deliberately
// ignores thinking-token, heartbeat, and plain assistant-text events. Only a
// new tool/query/result event can renew the progress lease. Identical repeated
// events are de-duplicated after volatile transport fields are removed.
func newSessionSemanticProgressProbe(path string) func() bool {
	var offset int64
	var remainder string
	seen := make(map[string]struct{})
	return func() bool {
		file, err := os.Open(path)
		if err != nil {
			return false
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return false
		}
		if info.Size() < offset {
			offset = 0
			remainder = ""
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return false
		}
		chunk, err := io.ReadAll(file)
		if err != nil {
			return false
		}
		offset += int64(len(chunk))
		text := remainder + string(chunk)
		lines := strings.Split(text, "\n")
		remainder = lines[len(lines)-1]
		progressed := false
		for _, line := range lines[:len(lines)-1] {
			var event any
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			semantic, ok := semanticEvent(event)
			if !ok {
				continue
			}
			normalized, err := json.Marshal(stripVolatileFields(semantic))
			if err != nil {
				continue
			}
			digest := sha256.Sum256(normalized)
			signature := hex.EncodeToString(digest[:])
			if _, duplicate := seen[signature]; duplicate {
				continue
			}
			seen[signature] = struct{}{}
			progressed = true
		}
		return progressed
	}
}

func semanticEvent(value any) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"type", "subtype"} {
			kind, _ := typed[key].(string)
			switch strings.ToLower(kind) {
			case "tool_use", "tool_result", "tool_call", "toolcall", "toolresult", "function_call", "command_execution", "command_result":
				return typed, true
			}
		}
		for _, child := range typed {
			if event, ok := semanticEvent(child); ok {
				return event, true
			}
		}
	case []any:
		for _, child := range typed {
			if event, ok := semanticEvent(child); ok {
				return event, true
			}
		}
	}
	return nil, false
}

func stripVolatileFields(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for key, child := range typed {
			switch key {
			case "id", "uuid", "call_id", "tool_use_id", "session_id", "timestamp", "created_at", "updated_at", "estimated_tokens", "estimated_tokens_delta":
				continue
			}
			clean[key] = stripVolatileFields(child)
		}
		return clean
	case []any:
		clean := make([]any, len(typed))
		for index, child := range typed {
			clean[index] = stripVolatileFields(child)
		}
		return clean
	default:
		return value
	}
}

func scanFileStamps(root string, ignored []string) map[string]fileStamp {
	result := make(map[string]fileStamp)
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		clean := filepath.Clean(path)
		for _, prefix := range ignored {
			if clean == prefix || strings.HasPrefix(clean, prefix+string(filepath.Separator)) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if entry.IsDir() {
			return nil
		}
		info, statErr := entry.Info()
		if statErr == nil {
			result[clean] = fileStamp{Size: info.Size(), ModTime: info.ModTime().UnixNano()}
		}
		return nil
	})
	return result
}
