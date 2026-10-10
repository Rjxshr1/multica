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

func TestValidateClientIdempotencyKeyRejectsRuntimeKeys(t *testing.T) {
	for _, key := range []string{"", "  ", "task-complete:abc", "task-fail:abc", "workflow-fail:abc", "verify:ready:node", "verify:run"} {
		if validateClientIdempotencyKey(key) == nil {
			t.Errorf("key %q was accepted", key)
		}
	}
	for _, key := range []string{"claim-implement-1", "issue:42", "run-7"} {
		if err := validateClientIdempotencyKey(key); err != nil {
			t.Errorf("key %q rejected: %v", key, err)
		}
	}
}
