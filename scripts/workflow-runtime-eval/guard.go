package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type activityBuffer struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	firstAt time.Time
	lastAt  time.Time
	writes  int
}

func (w *activityBuffer) Write(payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if w.firstAt.IsZero() && len(payload) > 0 {
		w.firstAt = now
	}
	if len(payload) > 0 {
		w.lastAt = now
		w.writes++
	}
	return w.buffer.Write(payload)
}

func (w *activityBuffer) snapshot() (int, time.Time, time.Time, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.Len(), w.firstAt, w.lastAt, w.writes
}

func (w *activityBuffer) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buffer.Bytes()...)
}

type observedCommandOptions struct {
	Guard                bool
	HardTimeout          time.Duration
	FirstProgressTimeout time.Duration
	IdleTimeout          time.Duration
	Workspace            string
	SessionPath          string
}

type observedCommandResult struct {
	Output          []byte
	StartedAt       time.Time
	FinishedAt      time.Time
	FirstProgressAt time.Time
	ProgressSignals int
	Termination     string
	Err             error
}

// runObservedCommand treats stdout, the Pi session journal, and workspace
// mutations as progress evidence.  A hard deadline always applies.  The guard
// arm additionally stops an attempt only after a bounded absence of evidence;
// it never interprets a missing final answer alone as failure.
func runObservedCommand(cmd *exec.Cmd, options observedCommandOptions) observedCommandResult {
	started := time.Now()
	writer := &activityBuffer{}
	cmd.Stdout, cmd.Stderr = writer, writer
	result := observedCommandResult{StartedAt: started, Termination: "completed"}
	if err := cmd.Start(); err != nil {
		result.Err = err
		result.Termination = "start_error"
		result.FinishedAt = time.Now()
		return result
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	baseline := progressSignature(options.Workspace, options.SessionPath)
	lastSignature := baseline
	lastProgress := started
	for {
		select {
		case err := <-done:
			result.Err = err
			result.FinishedAt = time.Now()
			length, first, _, writes := writer.snapshot()
			result.Output = writer.bytes()
			result.ProgressSignals += writes
			if result.FirstProgressAt.IsZero() && !first.IsZero() {
				result.FirstProgressAt = first
			}
			if err != nil && result.Termination == "completed" {
				result.Termination = "process_error"
			}
			_ = length
			return result
		case now := <-ticker.C:
			_, first, lastWrite, _ := writer.snapshot()
			signature := progressSignature(options.Workspace, options.SessionPath)
			if signature != lastSignature || (!lastWrite.IsZero() && lastWrite.After(lastProgress)) {
				lastSignature = signature
				lastProgress = now
				result.ProgressSignals++
				if result.FirstProgressAt.IsZero() {
					if !first.IsZero() {
						result.FirstProgressAt = first
					} else {
						result.FirstProgressAt = now
					}
				}
			}
			termination := ""
			if options.HardTimeout > 0 && now.Sub(started) >= options.HardTimeout {
				termination = "hard_deadline"
			} else if options.Guard && result.FirstProgressAt.IsZero() && options.FirstProgressTimeout > 0 && now.Sub(started) >= options.FirstProgressTimeout {
				termination = "no_first_progress"
			} else if options.Guard && !result.FirstProgressAt.IsZero() && options.IdleTimeout > 0 && now.Sub(lastProgress) >= options.IdleTimeout {
				termination = "idle_no_progress"
			}
			if termination != "" {
				result.Termination = termination
				_ = cmd.Process.Kill()
				result.Err = <-done
				result.FinishedAt = time.Now()
				result.Output = writer.bytes()
				return result
			}
		}
	}
}

func progressSignature(workspace, sessionPath string) string {
	parts := make([]string, 0, 32)
	if info, err := os.Stat(sessionPath); err == nil {
		parts = append(parts, "session:"+strconv.FormatInt(info.Size(), 10)+":"+strconv.FormatInt(info.ModTime().UnixNano(), 10))
	}
	_ = filepath.WalkDir(workspace, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if path != workspace && (entry.Name() == "model-logs" || strings.HasPrefix(entry.Name(), ".git")) {
				return filepath.SkipDir
			}
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr == nil {
			rel, _ := filepath.Rel(workspace, path)
			parts = append(parts, rel+":"+strconv.FormatInt(info.Size(), 10)+":"+strconv.FormatInt(info.ModTime().UnixNano(), 10))
		}
		return nil
	})
	sortStrings(parts)
	return strings.Join(parts, "|")
}

func sortStrings(values []string) {
	if len(values) < 2 {
		return
	}
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

var attemptSuffix = regexp.MustCompile(`(?:^|-)a(\d+)$`)

func metricForObserved(label string, reused bool, observed observedCommandResult) requestMetric {
	attempt := 1
	if match := attemptSuffix.FindStringSubmatch(label); len(match) == 2 {
		attempt, _ = strconv.Atoi(match[1])
	}
	metric := requestMetric{
		ID: label, StageID: strings.TrimSuffix(label, attemptSuffix.FindString(label)), Attempt: attempt,
		StartedAt:       observed.StartedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt:      observed.FinishedAt.UTC().Format(time.RFC3339Nano),
		DurationMS:      observed.FinishedAt.Sub(observed.StartedAt).Milliseconds(),
		ProgressSignals: observed.ProgressSignals, Termination: observed.Termination, SessionReused: reused,
	}
	if !observed.FirstProgressAt.IsZero() {
		metric.TTFBMS = observed.FirstProgressAt.Sub(observed.StartedAt).Milliseconds()
	}
	return metric
}

func observedFailure(observed observedCommandResult) error {
	switch observed.Termination {
	case "hard_deadline":
		return errors.New("hard deadline")
	case "no_first_progress", "idle_no_progress":
		return errors.New(observed.Termination)
	default:
		return observed.Err
	}
}
