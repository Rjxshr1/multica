package service

import "testing"

func TestWorkflowContextLikePatternTreatsQueryAsLiteralText(t *testing.T) {
	tests := map[string]string{
		"review":  `%review%`,
		"node_1":  `%node\_1%`,
		"100%":    `%100\%%`,
		`path\id`: `%path\\id%`,
	}
	for query, want := range tests {
		if got := workflowContextLikePattern(query); got != want {
			t.Errorf("workflowContextLikePattern(%q) = %q, want %q", query, got, want)
		}
	}
}
