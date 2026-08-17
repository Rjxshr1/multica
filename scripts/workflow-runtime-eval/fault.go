package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
)

func (e *evaluator) takeInjectedFault(dir string) bool {
	if !e.faultTasks[filepath.Base(dir)] {
		return false
	}
	e.requestMu.Lock()
	defer e.requestMu.Unlock()
	if e.faultInjected[dir] {
		return false
	}
	e.faultInjected[dir] = true
	return true
}

func (e *evaluator) runInjectedStall(dir, label string) (usage, error) {
	seconds := max(1, int(e.faultStall.Seconds()))
	cmd := exec.Command("bash", "-lc", "sleep "+strconv.Itoa(seconds))
	observed := runObservedCommand(cmd, observedCommandOptions{
		Guard:                e.profile.ProgressGuard,
		HardTimeout:          e.hardTimeout,
		FirstProgressTimeout: e.firstProgressTimeout,
		IdleTimeout:          e.idleTimeout,
		Workspace:            dir,
		SessionPath:          filepath.Join(dir, "model-logs", "injected-stall.session.jsonl"),
	})
	if observed.Termination == "completed" {
		observed.Termination = "injected_worker_disconnect"
		observed.Err = errors.New("injected worker disconnected after stall")
	}
	metric := metricForObserved(label+"-injected-stall", false, observed)
	e.requestMu.Lock()
	e.requestsByWorkspace[dir] = append(e.requestsByWorkspace[dir], metric)
	e.requestMu.Unlock()
	return usage{}, fmt.Errorf("injected stall (%s): %w", observed.Termination, observed.Err)
}
