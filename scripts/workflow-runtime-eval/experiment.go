package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// armProfile keeps every experiment arm explicit. Arms are cumulative so a
// difference between adjacent arms has one primary explanation.
type armProfile struct {
	ID               string `json:"id"`
	Label            string `json:"label"`
	Runtime          bool   `json:"authoritative_runtime"`
	ClassifiedRetry  bool   `json:"classified_retry"`
	ProgressGuard    bool   `json:"progress_guard"`
	SessionReuse     bool   `json:"session_reuse"`
	MaxStageAttempts int    `json:"max_stage_attempts"`
}

var armCatalog = map[string]armProfile{
	"original": {
		ID: "original", Label: "Pi+DeepSeek original", MaxStageAttempts: 1,
	},
	"runtime": {
		ID: "runtime", Label: "original + authoritative runtime", Runtime: true, MaxStageAttempts: 1,
	},
	"retry": {
		ID: "retry", Label: "runtime + classified retry", Runtime: true, ClassifiedRetry: true, MaxStageAttempts: 2,
	},
	"guard": {
		ID: "guard", Label: "retry + progress-aware outer guard", Runtime: true, ClassifiedRetry: true, ProgressGuard: true, MaxStageAttempts: 2,
	},
	"reuse": {
		ID: "reuse", Label: "guard + issue-scoped session reuse", Runtime: true, ClassifiedRetry: true, ProgressGuard: true, SessionReuse: true, MaxStageAttempts: 2,
	},
}

var defaultArmIDs = []string{"original", "runtime", "retry", "guard", "reuse"}

func parseArms(value string) ([]armProfile, error) {
	if strings.TrimSpace(value) == "" {
		value = strings.Join(defaultArmIDs, ",")
	}
	seen := map[string]bool{}
	var profiles []armProfile
	for _, raw := range strings.Split(value, ",") {
		id := strings.TrimSpace(raw)
		profile, ok := armCatalog[id]
		if !ok {
			return nil, fmt.Errorf("unknown arm %q (valid: %s)", id, strings.Join(defaultArmIDs, ","))
		}
		if !seen[id] {
			seen[id] = true
			profiles = append(profiles, profile)
		}
	}
	if len(profiles) < 2 {
		return nil, fmt.Errorf("at least two experiment arms are required")
	}
	return profiles, nil
}

type requestMetric struct {
	ID              string `json:"id"`
	StageID         string `json:"stage_id"`
	Attempt         int    `json:"attempt"`
	StartedAt       string `json:"started_at"`
	FinishedAt      string `json:"finished_at"`
	TTFBMS          int64  `json:"ttfb_ms,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	ProgressSignals int    `json:"progress_signals"`
	Termination     string `json:"termination"`
	SessionReused   bool   `json:"session_reused"`
}

type suiteMetric struct {
	Arm             string `json:"arm"`
	Repetition      int    `json:"repetition"`
	StartedAt       string `json:"started_at"`
	FinishedAt      string `json:"finished_at"`
	DurationMS      int64  `json:"duration_ms"`
	IssueCount      int    `json:"issue_count"`
	PassedCount     int    `json:"passed_count"`
	AllPassed       bool   `json:"all_passed"`
	GuardRecoveries int    `json:"guard_recoveries"`
}

type experimentCell struct {
	OrderIndex int        `json:"order_index"`
	Arm        armProfile `json:"arm"`
	Repetition int        `json:"repetition"`
	Tasks      []taskSpec `json:"-"`
}

func buildCells(profiles []armProfile, repetitions int, tasks []taskSpec) []experimentCell {
	var cells []experimentCell
	for repetition := 1; repetition <= repetitions; repetition++ {
		for _, profile := range profiles {
			cells = append(cells, experimentCell{Arm: profile, Repetition: repetition, Tasks: tasks})
		}
	}
	return cells
}

func summarizeSuite(arm string, repetition int, started, finished time.Time, results []result) suiteMetric {
	metric := suiteMetric{
		Arm: arm, Repetition: repetition,
		StartedAt:  started.UTC().Format(time.RFC3339Nano),
		FinishedAt: finished.UTC().Format(time.RFC3339Nano),
		DurationMS: finished.Sub(started).Milliseconds(),
		IssueCount: len(results),
	}
	for _, item := range results {
		if item.FinalPassed {
			metric.PassedCount++
		}
		metric.GuardRecoveries += item.GuardRecoveries
	}
	metric.AllPassed = metric.IssueCount > 0 && metric.PassedCount == metric.IssueCount
	return metric
}

func sortedProfiles(profiles []armProfile) []armProfile {
	out := append([]armProfile(nil), profiles...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
