package workflowruntime

import (
	"fmt"
	"sort"
	"time"
)

type Engine struct {
	store EventStore
	now   func() time.Time
}

func NewEngine(store EventStore) *Engine {
	return &Engine{store: store, now: func() time.Time { return time.Now().UTC() }}
}

func (e *Engine) Create(spec WorkflowSpec, commandID string) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	if len(e.store.Events(spec.ID)) > 0 {
		if e.store.CommandApplied(spec.ID, commandID) {
			return nil
		}
		return ErrAlreadyExists
	}

	_, err := e.store.Append(spec.ID, 0, commandID, Event{
		Type:       EventWorkflowCreated,
		OccurredAt: e.now(),
		Spec:       &spec,
	})
	return err
}

func (e *Engine) Snapshot(runID string) (*WorkflowRun, error) {
	events := e.store.Events(runID)
	if len(events) == 0 {
		return nil, ErrNotFound
	}

	var run *WorkflowRun
	for _, event := range events {
		if event.Type == EventWorkflowCreated {
			run = newRun(*event.Spec)
		}
		if run == nil {
			return nil, fmt.Errorf("event stream does not start with workflow.created")
		}
		if err := applyEvent(run, event); err != nil {
			return nil, err
		}
		run.Version = event.Version
	}
	return run, nil
}

func (e *Engine) Claim(runID, nodeID, commandID string) (Lease, error) {
	if e.store.CommandApplied(runID, commandID) {
		return e.activeLease(runID, nodeID)
	}

	run, err := e.Snapshot(runID)
	if err != nil {
		return Lease{}, err
	}
	node, err := run.Node(nodeID)
	if err != nil {
		return Lease{}, err
	}
	if node.State != NodeReady {
		return Lease{}, fmt.Errorf("%w: node %s is %s, want ready", ErrInvalidState, nodeID, node.State)
	}

	attempt := len(node.Attempts) + 1
	if attempt > node.Spec.MaxAttempts {
		return Lease{}, fmt.Errorf("%w: node %s exhausted %d attempts", ErrInvalidState, nodeID, node.Spec.MaxAttempts)
	}
	lease := Lease{
		RunID:     runID,
		NodeID:    nodeID,
		AttemptID: fmt.Sprintf("%s-a%d", nodeID, attempt),
		Attempt:   attempt,
		Fence:     node.Fence + 1,
	}
	_, err = e.store.Append(runID, run.Version, commandID, Event{
		Type:       EventNodeClaimed,
		OccurredAt: e.now(),
		NodeID:     nodeID,
		AttemptID:  lease.AttemptID,
		Attempt:    lease.Attempt,
		Fence:      lease.Fence,
	})
	return lease, err
}

func (e *Engine) ReportTaskSucceeded(lease Lease, commandID string) error {
	if e.store.CommandApplied(lease.RunID, commandID) {
		return nil
	}
	run, node, err := e.nodeForLease(lease)
	if err != nil {
		return err
	}
	if node.State != NodeRunning {
		return fmt.Errorf("%w: node %s is %s, want running", ErrInvalidState, lease.NodeID, node.State)
	}

	_, err = e.store.Append(lease.RunID, run.Version, commandID, Event{
		Type:       EventTaskResultAccepted,
		OccurredAt: e.now(),
		NodeID:     lease.NodeID,
		AttemptID:  lease.AttemptID,
		Attempt:    lease.Attempt,
		Fence:      lease.Fence,
	})
	return err
}

func (e *Engine) Verify(lease Lease, passed bool, failure FailureClass, commandID string) error {
	if e.store.CommandApplied(lease.RunID, commandID) {
		return nil
	}
	run, node, err := e.nodeForLease(lease)
	if err != nil {
		return err
	}
	if node.State != NodeVerifying {
		return fmt.Errorf("%w: node %s is %s, want verifying", ErrInvalidState, lease.NodeID, node.State)
	}

	eventType := EventVerificationRejected
	if passed {
		eventType = EventVerificationPassed
		failure = ""
	} else if failure == "" {
		failure = FailureUnclassified
	}
	_, err = e.store.Append(lease.RunID, run.Version, commandID, Event{
		Type:         eventType,
		OccurredAt:   e.now(),
		NodeID:       lease.NodeID,
		AttemptID:    lease.AttemptID,
		Attempt:      lease.Attempt,
		Fence:        lease.Fence,
		FailureClass: failure,
	})
	return err
}

func (e *Engine) Events(runID string) []Event {
	return e.store.Events(runID)
}

func (e *Engine) activeLease(runID, nodeID string) (Lease, error) {
	run, err := e.Snapshot(runID)
	if err != nil {
		return Lease{}, err
	}
	node, err := run.Node(nodeID)
	if err != nil {
		return Lease{}, err
	}
	if len(node.Attempts) == 0 {
		return Lease{}, fmt.Errorf("%w: node %s has no active attempt", ErrInvalidState, nodeID)
	}
	attempt := node.Attempts[len(node.Attempts)-1]
	return Lease{RunID: runID, NodeID: nodeID, AttemptID: attempt.ID, Attempt: attempt.Number, Fence: attempt.Fence}, nil
}

func (e *Engine) nodeForLease(lease Lease) (*WorkflowRun, *NodeExecution, error) {
	run, err := e.Snapshot(lease.RunID)
	if err != nil {
		return nil, nil, err
	}
	node, err := run.Node(lease.NodeID)
	if err != nil {
		return nil, nil, err
	}
	if node.Fence != lease.Fence || node.ActiveAttemptID != lease.AttemptID {
		return nil, nil, fmt.Errorf("%w: node %s active attempt is %s at fence %d", ErrStaleFence, lease.NodeID, node.ActiveAttemptID, node.Fence)
	}
	return run, node, nil
}

func newRun(spec WorkflowSpec) *WorkflowRun {
	run := &WorkflowRun{
		ID:    spec.ID,
		State: WorkflowRunning,
		Nodes: make(map[string]*NodeExecution, len(spec.Nodes)),
	}
	for _, nodeSpec := range spec.Nodes {
		if nodeSpec.MaxAttempts == 0 {
			nodeSpec.MaxAttempts = 1
		}
		state := NodeBlocked
		if len(nodeSpec.DependsOn) == 0 {
			state = NodeReady
		}
		run.Nodes[nodeSpec.ID] = &NodeExecution{Spec: nodeSpec, State: state}
	}
	return run
}

func applyEvent(run *WorkflowRun, event Event) error {
	switch event.Type {
	case EventWorkflowCreated:
		return nil
	case EventNodeClaimed:
		node, err := run.Node(event.NodeID)
		if err != nil {
			return err
		}
		node.State = NodeRunning
		node.Fence = event.Fence
		node.ActiveAttemptID = event.AttemptID
		node.Attempts = append(node.Attempts, Attempt{
			ID:        event.AttemptID,
			Number:    event.Attempt,
			Fence:     event.Fence,
			State:     AttemptRunning,
			StartedAt: event.OccurredAt,
		})
	case EventTaskResultAccepted:
		node, attempt, err := activeAttempt(run, event)
		if err != nil {
			return err
		}
		node.State = NodeVerifying
		attempt.State = AttemptVerifying
	case EventVerificationPassed:
		node, attempt, err := activeAttempt(run, event)
		if err != nil {
			return err
		}
		finishedAt := event.OccurredAt
		attempt.State = AttemptSucceeded
		attempt.FinishedAt = &finishedAt
		node.State = NodeSucceeded
		node.ActiveAttemptID = ""
		releaseDependencies(run)
	case EventVerificationRejected:
		node, attempt, err := activeAttempt(run, event)
		if err != nil {
			return err
		}
		finishedAt := event.OccurredAt
		attempt.State = AttemptRejected
		attempt.FailureClass = event.FailureClass
		attempt.FinishedAt = &finishedAt
		node.ActiveAttemptID = ""
		routeFailure(run, node, event.FailureClass)
	default:
		return fmt.Errorf("unknown workflow event type %q", event.Type)
	}
	return nil
}

func activeAttempt(run *WorkflowRun, event Event) (*NodeExecution, *Attempt, error) {
	node, err := run.Node(event.NodeID)
	if err != nil {
		return nil, nil, err
	}
	if node.Fence != event.Fence || node.ActiveAttemptID != event.AttemptID || len(node.Attempts) == 0 {
		return nil, nil, ErrStaleFence
	}
	return node, &node.Attempts[len(node.Attempts)-1], nil
}

func routeFailure(run *WorkflowRun, node *NodeExecution, failure FailureClass) {
	switch failure {
	case FailureEnvironmentGap, FailureDependencyGap:
		node.State = NodeWaiting
	case FailureContractAmbiguity, FailureSecurityRisk:
		node.State = NodeHardBlocked
		run.State = WorkflowHardBlocked
	default:
		if len(node.Attempts) < node.Spec.MaxAttempts {
			node.State = NodeReady
		} else {
			node.State = NodeHardBlocked
			run.State = WorkflowHardBlocked
		}
	}
}

func releaseDependencies(run *WorkflowRun) {
	for _, node := range run.Nodes {
		if node.State != NodeBlocked {
			continue
		}
		ready := true
		for _, dependencyID := range node.Spec.DependsOn {
			if run.Nodes[dependencyID].State != NodeSucceeded {
				ready = false
				break
			}
		}
		if ready {
			node.State = NodeReady
		}
	}

	for _, node := range run.Nodes {
		if node.State != NodeSucceeded {
			return
		}
	}
	run.State = WorkflowSucceeded
}

func validateSpec(spec WorkflowSpec) error {
	if spec.ID == "" {
		return fmt.Errorf("workflow id is required")
	}
	if len(spec.Nodes) == 0 {
		return fmt.Errorf("workflow must contain at least one node")
	}

	nodes := make(map[string]NodeSpec, len(spec.Nodes))
	for _, node := range spec.Nodes {
		if node.ID == "" {
			return fmt.Errorf("node id is required")
		}
		if _, exists := nodes[node.ID]; exists {
			return fmt.Errorf("duplicate node id %q", node.ID)
		}
		if node.MaxAttempts < 0 {
			return fmt.Errorf("node %q max_attempts cannot be negative", node.ID)
		}
		nodes[node.ID] = node
	}

	indegree := make(map[string]int, len(nodes))
	children := make(map[string][]string, len(nodes))
	for _, node := range spec.Nodes {
		for _, dependencyID := range node.DependsOn {
			if dependencyID == node.ID {
				return fmt.Errorf("node %q cannot depend on itself", node.ID)
			}
			if _, exists := nodes[dependencyID]; !exists {
				return fmt.Errorf("node %q depends on unknown node %q", node.ID, dependencyID)
			}
			indegree[node.ID]++
			children[dependencyID] = append(children[dependencyID], node.ID)
		}
	}

	queue := make([]string, 0, len(nodes))
	for id := range nodes {
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	sort.Strings(queue)
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, child := range children[id] {
			indegree[child]--
			if indegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	if visited != len(nodes) {
		return fmt.Errorf("workflow graph contains a cycle")
	}
	return nil
}
