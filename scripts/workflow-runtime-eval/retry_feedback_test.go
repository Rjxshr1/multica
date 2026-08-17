package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression for the two opaque verifier failures that kept the fully
// optimized arm below 98%. A retry cannot reliably repair a hidden contract
// when the only evidence is a one-word exception such as "escape" or "base".
func TestHardHiddenContractsProvideActionableRetryEvidence(t *testing.T) {
	want := map[string][]string{
		"integrate-csv-record": {"escaped", "double-quote", "literal quote"},
		"fix-retry-backoff":    {"positive", "base"},
	}

	for _, task := range allTasks() {
		terms, ok := want[task.ID]
		if !ok {
			continue
		}
		delete(want, task.ID)
		feedback := strings.ToLower(task.RetryFeedback)
		if feedback == "" {
			t.Errorf("%s: retry evidence is empty", task.ID)
			continue
		}
		for _, term := range terms {
			if !strings.Contains(feedback, term) {
				t.Errorf("%s: retry evidence %q missing %q", task.ID, task.RetryFeedback, term)
			}
		}
	}
	for id := range want {
		t.Errorf("task %s not found", id)
	}
}

func TestIntegrateCSVVerifierAcceptsCorrectEscapedQuoteImplementation(t *testing.T) {
	var target taskSpec
	for _, task := range allTasks() {
		if task.ID == "integrate-csv-record" {
			target = task
			break
		}
	}
	if target.ID == "" {
		t.Fatal("integrate-csv-record task not found")
	}

	dir := t.TempDir()
	for name, content := range target.Files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	implementation := `export function parseRecord(line) {
  const fields = [];
  let field = '';
  let quoted = false;
  for (let i = 0; i < line.length; i++) {
    if (line[i] === '"' && quoted && line[i + 1] === '"') {
      field += '"';
      i++;
    } else if (line[i] === '"') {
      quoted = !quoted;
    } else if (line[i] === ',' && !quoted) {
      fields.push(field);
      field = '';
    } else {
      field += line[i];
    }
  }
  fields.push(field);
  return fields;
}
`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(implementation), 0o644); err != nil {
		t.Fatal(err)
	}

	if ok, why := runVerifier(dir, target.Stages[0].Verifier); !ok {
		t.Fatalf("correct escaped-quote implementation was rejected: %s", why)
	}
}
