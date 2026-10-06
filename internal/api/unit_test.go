package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/storage"
)

// fakeUnitReader is the read pool. GetBooking is never called here.
type fakeUnitReader struct {
	unit  *storage.InventoryUnit
	err   error
	calls int
}

func (f *fakeUnitReader) GetBooking(context.Context, uuid.UUID) (*storage.Booking, error) {
	panic("GET /units/{id} must not read bookings")
}

func (f *fakeUnitReader) GetInventoryUnit(_ context.Context, _ uuid.UUID) (*storage.InventoryUnit, error) {
	f.calls++
	return f.unit, f.err
}

// newUnitServer serves GET /units/{id} from a service whose write store is
// nil: if the handler ever reaches it, the test panics. It returns the
// buffer the handler logs into.
func newUnitServer(r *fakeUnitReader) (*http.ServeMux, *bytes.Buffer) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	svc := booking.NewService(nil, logger, false, "single", booking.WithReader(r))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /units/{id}", NewHandler(svc, logger).GetUnit)
	return mux, &logs
}

func TestGetUnit(t *testing.T) {
	id := uuid.MustParse("6f1c2a5e-0b7d-4e8a-9c3f-2d4b6a8e0f13")
	desc := "Sea view"
	unit := &storage.InventoryUnit{
		UnitID:         id,
		Name:           "Deluxe Cabin",
		Description:    &desc,
		AvailableUnits: 4,
		TotalUnits:     10,
		Currency:       "IDR",
		PriceMinor:     150_000_000,
		MinBook:        1,
		Version:        3,
		CreatedAt:      time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC),
	}
	noDesc := *unit
	noDesc.Description = nil

	tests := []struct {
		name      string
		path      string
		unit      *storage.InventoryUnit
		readErr   error
		wantCode  int
		wantBody  map[string]any
		wantReads int
		wantLog   string // "" means nothing logged at ERROR
	}{
		{
			name:     "found",
			path:     "/units/" + id.String(),
			unit:     unit,
			wantCode: http.StatusOK,
			// Exactly these keys: version, created_at and updated_at stay inside.
			wantBody: map[string]any{
				"unit_id":         id.String(),
				"name":            "Deluxe Cabin",
				"description":     "Sea view",
				"available_units": float64(4),
				"total_units":     float64(10),
				"currency":        "IDR",
				"price_minor":     float64(150_000_000),
				"min_book":        float64(1),
			},
			wantReads: 1,
		},
		{
			name:     "found without a description",
			path:     "/units/" + id.String(),
			unit:     &noDesc,
			wantCode: http.StatusOK,
			// The key is still there, as null: the page shouldn't guess.
			wantBody: map[string]any{
				"unit_id":         id.String(),
				"name":            "Deluxe Cabin",
				"description":     nil,
				"available_units": float64(4),
				"total_units":     float64(10),
				"currency":        "IDR",
				"price_minor":     float64(150_000_000),
				"min_book":        float64(1),
			},
			wantReads: 1,
		},
		{
			name:      "not a uuid",
			path:      "/units/not-a-uuid",
			wantCode:  http.StatusBadRequest,
			wantBody:  map[string]any{"error": "invalid unit id"},
			wantReads: 0, // rejected before the service
		},
		{
			name:      "unknown unit",
			path:      "/units/" + id.String(),
			readErr:   fmt.Errorf("get inventory unit: %w", storage.ErrUnitNotFound),
			wantCode:  http.StatusNotFound,
			wantBody:  map[string]any{"error": "unit not found"},
			wantReads: 1,
		},
		{
			name:      "postgres down",
			path:      "/units/" + id.String(),
			readErr:   errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
			wantCode:  http.StatusInternalServerError,
			wantBody:  map[string]any{"error": "internal error"},
			wantReads: 1,
			wantLog:   "connection refused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeUnitReader{unit: tt.unit, err: tt.readErr}
			mux, logs := newUnitServer(r)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if rec.Code != tt.wantCode {
				t.Errorf("status: got %d, want %d (body %s)", rec.Code, tt.wantCode, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type: got %q, want application/json", ct)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, rec.Body)
			}
			if !reflect.DeepEqual(got, tt.wantBody) {
				t.Errorf("body:\n got %v\nwant %v", got, tt.wantBody)
			}
			if r.calls != tt.wantReads {
				t.Errorf("reads: got %d, want %d", r.calls, tt.wantReads)
			}

			// The client gets "internal error"; the cause goes to the log only.
			logged := logs.String()
			if tt.wantLog == "" && strings.Contains(logged, "level=ERROR") {
				t.Errorf("logged an error for a %d:\n%s", tt.wantCode, logged)
			}
			if tt.wantLog != "" {
				if !strings.Contains(logged, "level=ERROR") || !strings.Contains(logged, tt.wantLog) {
					t.Errorf("log: want an ERROR containing %q, got:\n%s", tt.wantLog, logged)
				}
				if strings.Contains(rec.Body.String(), tt.wantLog) {
					t.Errorf("body leaks the cause: %s", rec.Body)
				}
			}
		})
	}
}
