package workflowruntime

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrAlreadyExists   = errors.New("workflow run already exists")
	ErrNotFound        = errors.New("workflow run not found")
	ErrVersionConflict = errors.New("workflow run version conflict")
	ErrInvalidState    = errors.New("invalid workflow state transition")
	ErrStaleFence      = errors.New("stale execution fence")
)

type WorkflowState string

const (
	WorkflowRunning     WorkflowState = "running"
	WorkflowSucceeded   WorkflowState = "succeeded"
	WorkflowHardBlocked WorkflowState = "hard_blocked"
)

type NodeState string

const (
	NodeBlocked     NodeState = "blocked"
	NodeReady       NodeState = "ready"
	NodeRunning     NodeState = "running"
	NodeVerifying   NodeState = "verifying"
	NodeWaiting     NodeState = "waiting"
	NodeHardBlocked NodeState = "hard_blocked"
	NodeSucceeded   NodeState = "succeeded"
)

type AttemptState string

const (
	AttemptRunning   AttemptState = "running"
	AttemptVerifying AttemptState = "verifying"
	AttemptRejected  AttemptState = "rejected"
	AttemptSucceeded AttemptState = "succeeded"
)

type FailureClass string

const (
	FailureCodeDefect        FailureClass = "code_defect"
	FailureTestDefect        FailureClass = "test_defect"
	FailureEnvironmentGap    FailureClass = "environment_gap"
	FailureDependencyGap     FailureClass = "dependency_gap"
	FailureContractAmbiguity FailureClass = "contract_ambiguity"
	FailureSecurityRisk      FailureClass = "security_risk"
	FailureUnclassified      FailureClass = "unclassified"
)

type NodeSpec struct {
	ID          string   `json:"id"`
	DependsOn   []string `json:"depends_on,omitempty"`
	MaxAttempts int      `json:"max_attempts"`
}

type WorkflowSpec struct {
	ID    string     `json:"id"`
	Nodes []NodeSpec `json:"nodes"`
}

type Attempt struct {
	ID           string       `json:"id"`
	Number       int          `json:"number"`
	Fence        uint64       `json:"fence"`
	State        AttemptState `json:"state"`
	FailureClass FailureClass `json:"failure_class,omitempty"`
	StartedAt    time.Time    `json:"started_at"`
	FinishedAt   *time.Time   `json:"finished_at,omitempty"`
}

type NodeExecution struct {
	Spec            NodeSpec  `json:"spec"`
	State           NodeState `json:"state"`
	Fence           uint64    `json:"fence"`
	ActiveAttemptID string    `json:"active_attempt_id,omitempty"`
	Attempts        []Attempt `json:"attempts"`
}

type WorkflowRun struct {
	ID      string                    `json:"id"`
	State   WorkflowState             `json:"state"`
	Version uint64                    `json:"version"`
	Nodes   map[string]*NodeExecution `json:"nodes"`
}

type Lease struct {
	RunID     string `json:"run_id"`
	NodeID    string `json:"node_id"`
	AttemptID string `json:"attempt_id"`
	Attempt   int    `json:"attempt"`
	Fence     uint64 `json:"fence"`
}

type EventType string

const (
	EventWorkflowCreated      EventType = "workflow.created"
	EventNodeClaimed          EventType = "node.claimed"
	EventTaskResultAccepted   EventType = "task.result_accepted"
	EventVerificationPassed   EventType = "verification.passed"
	EventVerificationRejected EventType = "verification.rejected"
)

type Event struct {
	Version      uint64        `json:"version"`
	Type         EventType     `json:"type"`
	OccurredAt   time.Time     `json:"occurred_at"`
	Spec         *WorkflowSpec `json:"spec,omitempty"`
	NodeID       string        `json:"node_id,omitempty"`
	AttemptID    string        `json:"attempt_id,omitempty"`
	Attempt      int           `json:"attempt,omitempty"`
	Fence        uint64        `json:"fence,omitempty"`
	FailureClass FailureClass  `json:"failure_class,omitempty"`
}

func (r *WorkflowRun) Node(id string) (*NodeExecution, error) {
	node, ok := r.Nodes[id]
	if !ok {
		return nil, fmt.Errorf("%w: node %q", ErrNotFound, id)
	}
	return node, nil
}
