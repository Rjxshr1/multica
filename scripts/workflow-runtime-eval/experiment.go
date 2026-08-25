package main

import (
	"fmt"
	"math/rand"
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
	DurationKind    string `json:"duration_kind,omitempty"`
}

type experimentCell struct {
	OrderIndex int        `json:"order_index"`
	PairKey    string     `json:"pair_key,omitempty"`
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

// buildPairedCells keeps both arms for the same task adjacent while
// randomizing which arm runs first. This costs throughput, but prevents an
// entire arm from being measured in a different load window.
func buildPairedCells(profiles []armProfile, repetitions int, tasks []taskSpec, seed int64) []experimentCell {
	rng := rand.New(rand.NewSource(seed))
	type block struct {
		repetition int
		task       taskSpec
	}
	blocks := make([]block, 0, repetitions*len(tasks))
	for repetition := 1; repetition <= repetitions; repetition++ {
		for _, task := range tasks {
			blocks = append(blocks, block{repetition: repetition, task: task})
		}
	}
	rng.Shuffle(len(blocks), func(i, j int) { blocks[i], blocks[j] = blocks[j], blocks[i] })
	cells := make([]experimentCell, 0, len(blocks)*len(profiles))
	for _, item := range blocks {
		ordered := append([]armProfile(nil), profiles...)
		rng.Shuffle(len(ordered), func(i, j int) { ordered[i], ordered[j] = ordered[j], ordered[i] })
		pairKey := fmt.Sprintf("%d/%d/%s", seed, item.repetition, item.task.ID)
		for _, profile := range ordered {
			cells = append(cells, experimentCell{PairKey: pairKey, Arm: profile, Repetition: item.repetition, Tasks: []taskSpec{item.task}})
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

func summarizeSerialEquivalentSuites(results []result) []suiteMetric {
	grouped := map[string][]result{}
	for _, item := range results {
		key := fmt.Sprintf("%s/%d", item.Arm, item.Repetition)
		grouped[key] = append(grouped[key], item)
	}
	metrics := make([]suiteMetric, 0, len(grouped))
	for _, items := range grouped {
		if len(items) == 0 {
			continue
		}
		metric := suiteMetric{Arm: items[0].Arm, Repetition: items[0].Repetition, IssueCount: len(items), DurationKind: "serial_equivalent_sum_issue"}
		var first, last time.Time
		for _, item := range items {
			metric.DurationMS += item.DurationMS
			if item.FinalPassed {
				metric.PassedCount++
			}
			metric.GuardRecoveries += item.GuardRecoveries
			started, _ := time.Parse(time.RFC3339Nano, item.StartedAt)
			finished, _ := time.Parse(time.RFC3339Nano, item.FinishedAt)
			if first.IsZero() || (!started.IsZero() && started.Before(first)) {
				first = started
			}
			if finished.After(last) {
				last = finished
			}
		}
		metric.AllPassed = metric.PassedCount == metric.IssueCount
		metric.StartedAt = first.UTC().Format(time.RFC3339Nano)
		metric.FinishedAt = last.UTC().Format(time.RFC3339Nano)
		metrics = append(metrics, metric)
	}
	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].Repetition != metrics[j].Repetition {
			return metrics[i].Repetition < metrics[j].Repetition
		}
		return metrics[i].Arm < metrics[j].Arm
	})
	return metrics
}

func sortedProfiles(profiles []armProfile) []armProfile {
	out := append([]armProfile(nil), profiles...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
