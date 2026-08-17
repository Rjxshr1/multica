// Package workflowruntime owns authoritative workflow execution state.
//
// Agents report observations to this package. They never mutate workflow or
// node state directly. State is rebuilt from an append-only event stream, and
// every command is guarded by an idempotency key plus optimistic concurrency.
package workflowruntime
