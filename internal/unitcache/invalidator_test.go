package unitcache

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/consumer"
)

// fakeDeleter records the units the invalidator deletes. With err set, every
// Delete fails, like Redis being down.
type fakeDeleter struct {
	deleted []uuid.UUID
	err     error
}

func (f *fakeDeleter) Delete(_ context.Context, id uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

var errRedisDown = errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")

func TestInvalidatorHandle(t *testing.T) {
	unit := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	booking := `"booking_id":"6f1c2a5e-0b7d-4e8a-9c3f-2d4b6a8e0f13"`

	tests := []struct {
		name        string
		eventType   string
		payload     string
		deleteErr   error
		wantDeleted []uuid.UUID
		wantPoison  bool // skip it: retrying can never help
		wantRetry   bool // fail it: the framework retries without committing
		wantLog     string
	}{
		{
			name:        "created: seats taken",
			eventType:   "booking.created",
			payload:     `{"version":1,` + booking + `,"unit_id":"` + unit.String() + `","qty":2}`,
			wantDeleted: []uuid.UUID{unit},
		},
		{
			name:        "cancelled v2: seats given back",
			eventType:   "booking.cancelled",
			payload:     `{"version":2,` + booking + `,"unit_id":"` + unit.String() + `","qty":2,"reason":"expired"}`,
			wantDeleted: []uuid.UUID{unit},
		},
		{
			// Written before the deploy: the TTL bounds how stale it leaves
			// the unit. Logged, so a rollout shows how many there were.
			name:      "cancelled v1: no unit to invalidate",
			eventType: "booking.cancelled",
			payload:   `{"version":1,` + booking + `,"reason":"expired"}`,
			wantLog:   "event_id=42",
		},
		{
			// v2 promises a unit. Without one, the producer has a bug.
			name:       "cancelled v2 without a unit",
			eventType:  "booking.cancelled",
			payload:    `{"version":2,` + booking + `,"reason":"expired"}`,
			wantPoison: true,
		},
		{
			name:       "created without a unit",
			eventType:  "booking.created",
			payload:    `{"version":1,` + booking + `}`,
			wantPoison: true,
		},
		{
			name:       "not JSON",
			eventType:  "booking.created",
			payload:    `{"version":1,`,
			wantPoison: true,
		},
		{
			// A confirmation doesn't change the unit: the seats were taken
			// when the booking was created.
			name:      "confirmed: nothing to invalidate",
			eventType: "booking.confirmed",
			payload:   `{"version":1,` + booking + `}`,
		},
		{
			name:      "an event type it doesn't know",
			eventType: "something.new",
			payload:   `{}`,
		},
		{
			name:      "redis down",
			eventType: "booking.cancelled",
			payload:   `{"version":2,` + booking + `,"unit_id":"` + unit.String() + `","qty":2,"reason":"expired"}`,
			deleteErr: errRedisDown,
			wantRetry: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := &fakeDeleter{err: tt.deleteErr}
			var logs bytes.Buffer
			inv := NewInvalidator(cache, slog.New(slog.NewTextHandler(&logs, nil)))
			e := consumer.Event{ID: 42, Type: tt.eventType, Payload: []byte(tt.payload)}

			// A nil tx: the invalidator never touches Postgres.
			err := inv.Handle(context.Background(), nil, e)

			switch {
			case tt.wantPoison:
				if !errors.Is(err, consumer.ErrPoisonMessage) {
					t.Errorf("error: got %v, want consumer.ErrPoisonMessage", err)
				}
			case tt.wantRetry:
				if err == nil || errors.Is(err, consumer.ErrPoisonMessage) {
					t.Errorf("error: got %v, want a retryable error", err)
				}
				if !errors.Is(err, tt.deleteErr) {
					t.Errorf("error: got %v, want it to wrap %v", err, tt.deleteErr)
				}
			default:
				if err != nil {
					t.Errorf("error: got %v, want nil", err)
				}
			}

			if !slices.Equal(cache.deleted, tt.wantDeleted) {
				t.Errorf("deleted: got %v, want %v", cache.deleted, tt.wantDeleted)
			}
			if tt.wantLog != "" && !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("log: want a line with %q, got:\n%s", tt.wantLog, logs.String())
			}
		})
	}
}

// The invalidator plugs into the existing consumer framework.
var _ consumer.Handler = (*Invalidator)(nil)
