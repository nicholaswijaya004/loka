package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Outbox struct {
	ID            int64
	AggregateType string
	AggregateID   uuid.UUID
	EventType     string
	Payload       json.RawMessage
	AttemptNumber int
	Error         *string
	CreatedAt     time.Time
	PublishedAt   *time.Time
}

func (s *Store) InsertOutboxEvent(ctx context.Context, o *Outbox) error {
	err := s.db.QueryRow(ctx, `
		INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, 
									payload)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at
		`, o.AggregateType, o.AggregateID, o.EventType, o.Payload,
	).Scan(&o.ID, &o.CreatedAt)
	if isSerializationFailure(err) {
		return ErrSerializationFailure
	}
	if err != nil {
		return fmt.Errorf("failed to insert outbox event: %w", err)
	}
	return nil
}

func (s *Store) FetchUnpublishedOutboxEvents(ctx context.Context, limit int) ([]Outbox, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, aggregate_type, aggregate_id, event_type, payload,
		       attempt_number, error, created_at, published_at
		FROM outbox_events
		WHERE published_at IS NULL
		ORDER BY id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("fetch unpublished outbox events: %w", err)
	}
	defer rows.Close()

	var events []Outbox
	for rows.Next() {
		var e Outbox
		if err := rows.Scan(&e.ID, &e.AggregateType, &e.AggregateID, &e.EventType, &e.Payload,
			&e.AttemptNumber, &e.Error, &e.CreatedAt, &e.PublishedAt); err != nil {
			return nil, fmt.Errorf("scan outbox event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) MarkOutboxEventsPublished(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	tag, err := s.db.Exec(ctx, `
		UPDATE outbox_events
		SET published_at = now()
		WHERE id = ANY($1) AND published_at IS NULL
	`, ids)
	if err != nil {
		return fmt.Errorf("mark outbox events published: %w", err)
	}
	if got := tag.RowsAffected(); got != int64(len(ids)) {
		return fmt.Errorf("mark outbox events published: updated %d rows, want %d", got, len(ids))
	}
	return nil
}

func (s *Store) RecordOutboxFailure(ctx context.Context, id int64, errMsg string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE outbox_events
		SET attempt_number = attempt_number + 1,
		    error = $2
		WHERE id = $1
	`, id, errMsg)
	if err != nil {
		return fmt.Errorf("record outbox failure: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record outbox failure: event %d not found", id)
	}
	return nil
}
