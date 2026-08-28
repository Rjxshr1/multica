package main

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildPairedCellsKeepsCompleteArmsAdjacent(t *testing.T) {
	profiles, err := parseArms("guard,reuse")
	if err != nil {
		t.Fatal(err)
	}
	tasks := allTasks()[:3]
	cells := buildPairedCells(profiles, 2, tasks, 20260825)
	if len(cells) != 12 {
		t.Fatalf("got %d cells, want 12", len(cells))
	}
	seen := map[string]bool{}
	for index := 0; index < len(cells); index += 2 {
		left, right := cells[index], cells[index+1]
		if left.PairKey == "" || left.PairKey != right.PairKey {
			t.Fatalf("cells %d/%d are not one pair: %q %q", index, index+1, left.PairKey, right.PairKey)
		}
		if left.Arm.ID == right.Arm.ID || (left.Arm.ID != "guard" && right.Arm.ID != "guard") || (left.Arm.ID != "reuse" && right.Arm.ID != "reuse") {
			t.Fatalf("pair %s does not contain guard and reuse: %s %s", left.PairKey, left.Arm.ID, right.Arm.ID)
		}
		if len(left.Tasks) != 1 || len(right.Tasks) != 1 || left.Tasks[0].ID != right.Tasks[0].ID {
			t.Fatalf("pair %s does not contain the same single task", left.PairKey)
		}
		if seen[left.PairKey] {
			t.Fatalf("duplicate pair key %s", left.PairKey)
		}
		seen[left.PairKey] = true
	}
}

func TestWritePerJobCSVPreservesPairAndReuseEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "per_job.csv")
	items := []result{{
		Arm: "reuse", Seed: 7, Repetition: 2, TaskID: "task-a", Category: "fixture", Difficulty: "medium",
		FinalPassed: true, FirstPassSuccess: false, Recovered: true, DurationMS: 1234, EnvironmentWaitMS: 5000, ModelCalls: 2,
		Usage: usage{Total: 900, CacheRead: 400}, StartedAt: "2026-08-25T00:00:00Z", FinishedAt: "2026-08-25T00:00:01Z",
		Requests: []requestMetric{{Termination: "completed"}, {Termination: "completed", SessionReused: true}},
	}}
	if err := writePerJobCSV(path, items); err != nil {
		t.Fatal(err)
	}
	handle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	rows, err := csv.NewReader(handle).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1][0] != "7/2/task-a" || rows[1][11] != "5000" || rows[1][16] != "1" || rows[1][17] != "2" {
		t.Fatalf("unexpected CSV rows: %#v", rows)
	}
}

func TestProviderQuotaResponseRequiresMarkerAndZeroUsage(t *testing.T) {
	session := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(session, []byte(`{"responseId":"cost-quota-123","text":"当前小时请求过于频繁，请下个整点重试"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isProviderQuotaResponse(nil, session, usage{}) {
		t.Fatal("zero-usage quota response was not detected")
	}
	if isProviderQuotaResponse(nil, session, usage{Total: 1}) {
		t.Fatal("non-zero usage must not be classified as an environment quota response")
	}
}
