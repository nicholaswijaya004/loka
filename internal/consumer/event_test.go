package consumer

import (
	"bytes"
	"errors"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
)

func record(headers ...kgo.RecordHeader) *kgo.Record {
	return &kgo.Record{
		Key:       []byte("booking-123"),
		Value:     []byte(`{"version":1}`),
		Headers:   headers,
		Partition: 2,
		Offset:    41,
	}
}

func h(key, value string) kgo.RecordHeader {
	return kgo.RecordHeader{Key: key, Value: []byte(value)}
}

func TestParseEvent(t *testing.T) {
	tests := []struct {
		name       string
		rec        *kgo.Record
		wantPoison bool
		wantEvent  Event
	}{
		{
			name: "valid record",
			rec:  record(h("event_id", "7"), h("event_type", "booking.created")),
			wantEvent: Event{
				ID:      7,
				Type:    "booking.created",
				Key:     "booking-123",
				Payload: []byte(`{"version":1}`),
			},
		},
		{
			name: "header order does not matter",
			rec:  record(h("event_type", "booking.created"), h("event_id", "7")),
			wantEvent: Event{
				ID:      7,
				Type:    "booking.created",
				Key:     "booking-123",
				Payload: []byte(`{"version":1}`),
			},
		},
		{
			name:       "event_id missing",
			rec:        record(h("event_type", "booking.created")),
			wantPoison: true,
		},
		{
			name:       "event_id not a number",
			rec:        record(h("event_id", "abc"), h("event_type", "booking.created")),
			wantPoison: true,
		},
		{
			name:       "event_id empty",
			rec:        record(h("event_id", ""), h("event_type", "booking.created")),
			wantPoison: true,
		},
		{
			name:       "event_id too large for int64",
			rec:        record(h("event_id", "99999999999999999999"), h("event_type", "booking.created")),
			wantPoison: true,
		},
		{
			name:       "event_type missing",
			rec:        record(h("event_id", "7")),
			wantPoison: true,
		},
		{
			name:       "event_type empty",
			rec:        record(h("event_id", "7"), h("event_type", "")),
			wantPoison: true,
		},
		{
			name:       "no headers at all",
			rec:        record(),
			wantPoison: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEvent(tt.rec)

			if tt.wantPoison {
				if !errors.Is(err, ErrPoisonMessage) {
					t.Fatalf("error: got %v, want one wrapping ErrPoisonMessage", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ID != tt.wantEvent.ID || got.Type != tt.wantEvent.Type || got.Key != tt.wantEvent.Key {
				t.Errorf("event: got %+v, want %+v", got, tt.wantEvent)
			}
			if !bytes.Equal(got.Payload, tt.wantEvent.Payload) {
				t.Errorf("payload: got %s, want %s", got.Payload, tt.wantEvent.Payload)
			}
		})
	}
}

// The error must say where the bad message is, or nobody can find it later.
func TestParseEventPoisonErrorNamesLocation(t *testing.T) {
	_, err := parseEvent(record(h("event_type", "booking.created")))
	if err == nil {
		t.Fatal("got nil, want a poison error")
	}
	msg := err.Error()
	for _, want := range []string{"partition 2", "offset 41"} {
		if !bytes.Contains([]byte(msg), []byte(want)) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}
