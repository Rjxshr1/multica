package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const workflowOutboxEventType = "workflow:event"

type WorkflowOutboxWorker struct {
	Queries *db.Queries
	Bus     *events.Bus
}

func NewWorkflowOutboxWorker(queries *db.Queries, bus *events.Bus) *WorkflowOutboxWorker {
	return &WorkflowOutboxWorker{Queries: queries, Bus: bus}
}

func (w *WorkflowOutboxWorker) Run(ctx context.Context) {
	if w == nil || w.Queries == nil || w.Bus == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		processed, err := w.ProcessBatch(ctx, 64)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("workflow outbox worker", "error", err)
		}
		if processed > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *WorkflowOutboxWorker) ProcessBatch(ctx context.Context, batchSize int32) (int, error) {
	if w == nil || w.Queries == nil || w.Bus == nil {
		return 0, nil
	}
	leaseToken := newPGUUID()
	items, err := w.Queries.LeaseWorkflowOutbox(ctx, db.LeaseWorkflowOutboxParams{LeaseToken: leaseToken, LeaseSeconds: 30, BatchSize: batchSize})
	if err != nil {
		return 0, fmt.Errorf("lease workflow outbox: %w", err)
	}
	for _, item := range items {
		payload := map[string]any{"event_id": util.UUIDToString(item.EventID), "run_id": util.UUIDToString(item.RunID), "topic": item.Topic, "data": json.RawMessage(item.Payload)}
		w.Bus.Publish(events.Event{Type: workflowOutboxEventType, WorkspaceID: util.UUIDToString(item.WorkspaceID), ActorType: "system", Payload: payload})
		rows, markErr := w.Queries.MarkWorkflowOutboxPublished(ctx, db.MarkWorkflowOutboxPublishedParams{ID: item.ID, LeaseToken: leaseToken})
		if markErr == nil && rows == 1 {
			continue
		}
		message := "outbox publish acknowledgement lost"
		if markErr != nil {
			message = markErr.Error()
		}
		_, retryErr := w.Queries.RetryWorkflowOutbox(ctx, db.RetryWorkflowOutboxParams{MaxAttempts: 10, AvailableAt: pgtype.Timestamptz{Time: time.Now().UTC().Add(time.Second), Valid: true}, LastError: textValue(message), ID: item.ID, LeaseToken: leaseToken})
		if retryErr != nil {
			return len(items), fmt.Errorf("retry workflow outbox: %w", retryErr)
		}
	}
	return len(items), nil
}
