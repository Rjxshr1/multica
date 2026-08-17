package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// deterministicPi is a seconds-scale harness driver.  It validates scheduling,
// retry, dynamic Review, verification, and metric aggregation without making a
// model call.  Its numbers are never mixed with or presented as Agent results.
func (e *evaluator) deterministicPi(dir, label, prompt string) (usage, error) {
	started := time.Now()
	taskID := filepath.Base(dir)
	lower := strings.ToLower(prompt)
	write := func(name, value string) {
		must(os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644))
	}

	switch taskID {
	case "read-inventory-reservation":
		write("CODE_REVIEW.md", "available must subtract reserved inventory; otherwise checkout accepts unavailable stock.\n")
	case "fix-clamp-bounds":
		if strings.HasSuffix(label, "a2") {
			write("index.js", "export function clamp(v,min,max){ return Math.min(max,Math.max(min,v)); }\n")
		} else {
			write("index.js", "export function clamp(v,min,max){ return Math.max(min,v); }\n")
		}
	case "design-webhook-delivery":
		write("TECHNICAL_PLAN.md", "Durable event log; signature verification; idempotency key; bounded retry; dead-letter queue; authorized replay; ordering tradeoff; observability and rollout.\n")
	case "recover-email-contract":
		if strings.Contains(lower, "hidden verification") || strings.HasSuffix(label, "a2") {
			write("index.js", "export function normalizeEmail(v){const x=v.trim().toLowerCase();const p=x.split('@');if(p.length!==2||!p[0]||!p[1])throw Error('invalid email');return x}\n")
		}
	case "integrate-median":
		if strings.Contains(lower, "independent integration") {
			write("INTEGRATION_PASSED.md", "pass\n")
		} else {
			write("index.js", "export function median(v){if(!v.length)throw Error('empty');const a=[...v].sort((x,y)=>x-y),m=Math.floor(a.length/2);return a.length%2?a[m]:(a[m-1]+a[m])/2}\n")
		}
	case "cross-format-contract":
		switch {
		case strings.Contains(lower, "resumed integration"):
			write("INTEGRATION_PASSED.md", "pass\n")
		case strings.Contains(lower, "review node"):
			write("REVIEW_APPROVED.md", "approved\n")
		case strings.Contains(lower, "you are workflow 2"):
			write("dependency.js", "export function format(v){return 'READY:'+v}\n")
		case strings.Contains(lower, "workflow 1 integration"):
			write("BUG_REPORT.md", "contract requires READY:<value> string\n")
		}
	default:
		return usage{}, fmt.Errorf("deterministic driver has no fixture for %s", taskID)
	}

	time.Sleep(2 * time.Millisecond)
	finished := time.Now()
	e.requestMu.Lock()
	reused := e.profile.SessionReuse && len(e.requestsByWorkspace[dir]) > 0
	e.requestMu.Unlock()
	metric := requestMetric{
		ID: label, StageID: label, Attempt: 1,
		StartedAt: started.UTC().Format(time.RFC3339Nano), FinishedAt: finished.UTC().Format(time.RFC3339Nano),
		TTFBMS: 1, DurationMS: finished.Sub(started).Milliseconds(), ProgressSignals: 1,
		Termination: "completed", SessionReused: reused,
	}
	e.requestMu.Lock()
	e.requestsByWorkspace[dir] = append(e.requestsByWorkspace[dir], metric)
	e.requestMu.Unlock()
	return usage{Input: 80, Output: 20, Total: 100}, nil
}
