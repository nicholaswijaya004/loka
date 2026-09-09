package booking

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

// fakeStore is a hand-written test double for the Store interface.
// Each method returns whatever the test configured, and records what it was called with.
type fakeStore struct {
	unit    *storage.InventoryUnit
	unitErr error

	decErr    error
	decCalls  int
	decQty    int
	decUnitID uuid.UUID

	insErr   error
	insCalls int
	inserted *storage.Booking

	booking    *storage.Booking
	bookingErr error
}

func (f *fakeStore) GetInventoryUnit(ctx context.Context, id uuid.UUID) (*storage.InventoryUnit, error) {
	if f.unitErr != nil {
		return nil, f.unitErr
	}
	return f.unit, nil
}

func (f *fakeStore) DecrementAvailability(ctx context.Context, id uuid.UUID, qty int) error {
	f.decCalls++
	f.decQty = qty
	f.decUnitID = id
	return f.decErr
}

func (f *fakeStore) InsertBooking(ctx context.Context, b *storage.Booking) error {
	f.insCalls++
	if f.insErr != nil {
		return f.insErr
	}
	b.BookingID = uuid.New()
	b.CreatedAt = time.Now()
	b.UpdatedAt = b.CreatedAt
	f.inserted = b
	return nil
}

func (f *fakeStore) GetBooking(ctx context.Context, id uuid.UUID) (*storage.Booking, error) {
	if f.bookingErr != nil {
		return nil, f.bookingErr
	}
	return f.booking, nil
}

// testUnit returns an inventory unit with sensible defaults, adjustable per test.
func testUnit(available, total, minBook int) *storage.InventoryUnit {
	return &storage.InventoryUnit{
		UnitID:         uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Name:           "Deluxe Cabin",
		AvailableUnits: available,
		TotalUnits:     total,
		Currency:       "IDR",
		PriceMinor:     150_000_000,
		MinBook:        minBook,
		Version:        0,
	}
}

var (
	testUnitID     = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	testCustomerID = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	testVisit      = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
)

func TestServiceCreate(t *testing.T) {
	otherErr := errors.New("connection reset by peer")

	tests := []struct {
		name string

		qty   int
		store *fakeStore

		wantErr      error
		wantErrIs    bool // true when wantErr should be matched with errors.Is
		wantDecCalls int
		wantInsCalls int
	}{
		{
			name:         "rejects zero quantity before touching the store",
			qty:          0,
			store:        &fakeStore{unit: testUnit(10, 10, 1)},
			wantErr:      ErrInvalidQty,
			wantErrIs:    true,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "rejects negative quantity",
			qty:          -3,
			store:        &fakeStore{unit: testUnit(10, 10, 1)},
			wantErr:      ErrInvalidQty,
			wantErrIs:    true,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "propagates unit not found",
			qty:          1,
			store:        &fakeStore{unitErr: storage.ErrUnitNotFound},
			wantErr:      storage.ErrUnitNotFound,
			wantErrIs:    true,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "propagates unexpected store error on lookup",
			qty:          1,
			store:        &fakeStore{unitErr: otherErr},
			wantErr:      otherErr,
			wantErrIs:    true,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "rejects quantity below the unit minimum",
			qty:          1,
			store:        &fakeStore{unit: testUnit(10, 10, 4)},
			wantErr:      ErrMinBook,
			wantErrIs:    true,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "rejects quantity above availability",
			qty:          5,
			store:        &fakeStore{unit: testUnit(3, 10, 1)},
			wantErr:      ErrSoldOut,
			wantErrIs:    true,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "rejects when availability is exactly zero",
			qty:          1,
			store:        &fakeStore{unit: testUnit(0, 10, 1)},
			wantErr:      ErrSoldOut,
			wantErrIs:    true,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			// The race: availability read as sufficient, but another request
			// won between the check and the decrement, so the CHECK constraint
			// rejected the update. Storage reports ErrSoldOut; the service must
			// translate it into its own ErrSoldOut rather than leaking it.
			name:         "translates storage sold-out from a lost race",
			qty:          1,
			store:        &fakeStore{unit: testUnit(1, 1, 1), decErr: storage.ErrSoldOut},
			wantErr:      ErrSoldOut,
			wantErrIs:    true,
			wantDecCalls: 1,
			wantInsCalls: 0,
		},
		{
			name:         "propagates unexpected decrement failure",
			qty:          1,
			store:        &fakeStore{unit: testUnit(10, 10, 1), decErr: otherErr},
			wantErr:      otherErr,
			wantErrIs:    true,
			wantDecCalls: 1,
			wantInsCalls: 0,
		},
		{
			// Documents current (deliberately unsafe) behaviour: the decrement
			// has already committed when the insert fails, so a unit is consumed
			// with no booking to show for it. Day 8's transaction removes this.
			name:         "insert failure leaves the decrement committed",
			qty:          1,
			store:        &fakeStore{unit: testUnit(10, 10, 1), insErr: otherErr},
			wantErr:      otherErr,
			wantErrIs:    true,
			wantDecCalls: 1,
			wantInsCalls: 1,
		},
		{
			name:         "books a single unit",
			qty:          1,
			store:        &fakeStore{unit: testUnit(10, 10, 1)},
			wantErr:      nil,
			wantDecCalls: 1,
			wantInsCalls: 1,
		},
		{
			name:         "books the last available unit",
			qty:          1,
			store:        &fakeStore{unit: testUnit(1, 1, 1)},
			wantErr:      nil,
			wantDecCalls: 1,
			wantInsCalls: 1,
		},
		{
			name:         "books exactly the minimum quantity",
			qty:          4,
			store:        &fakeStore{unit: testUnit(10, 10, 4)},
			wantErr:      nil,
			wantDecCalls: 1,
			wantInsCalls: 1,
		},
		{
			name:         "books the entire remaining availability",
			qty:          10,
			store:        &fakeStore{unit: testUnit(10, 10, 1)},
			wantErr:      nil,
			wantDecCalls: 1,
			wantInsCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(tt.store)

			got, err := svc.Create(context.Background(), testUnitID, testCustomerID, tt.qty, testVisit)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("error: got %v, want %v", err, tt.wantErr)
				}
				if got != nil {
					t.Errorf("booking: got %+v, want nil on error", got)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got == nil {
					t.Fatal("booking: got nil, want a booking")
				}
			}

			if tt.store.decCalls != tt.wantDecCalls {
				t.Errorf("DecrementAvailability calls: got %d, want %d", tt.store.decCalls, tt.wantDecCalls)
			}
			if tt.store.insCalls != tt.wantInsCalls {
				t.Errorf("InsertBooking calls: got %d, want %d", tt.store.insCalls, tt.wantInsCalls)
			}
		})
	}
}

func TestServiceCreateBuildsBookingCorrectly(t *testing.T) {
	store := &fakeStore{unit: testUnit(10, 10, 1)}
	svc := NewService(store)

	const qty = 3

	got, err := svc.Create(context.Background(), testUnitID, testCustomerID, qty, testVisit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.UnitID != testUnitID {
		t.Errorf("UnitID: got %v, want %v", got.UnitID, testUnitID)
	}
	if got.CustomerID != testCustomerID {
		t.Errorf("CustomerID: got %v, want %v", got.CustomerID, testCustomerID)
	}
	if got.Qty != qty {
		t.Errorf("Qty: got %d, want %d", got.Qty, qty)
	}
	if !got.VisitDateTime.Equal(testVisit) {
		t.Errorf("VisitDateTime: got %v, want %v", got.VisitDateTime, testVisit)
	}
	if want := int64(qty) * store.unit.PriceMinor; got.TotalMinor != want {
		t.Errorf("TotalMinor: got %d, want %d", got.TotalMinor, want)
	}
	if got.Currency != store.unit.Currency {
		t.Errorf("Currency: got %q, want %q", got.Currency, store.unit.Currency)
	}
	if got.BookingStatus != "pending" {
		t.Errorf("BookingStatus: got %q, want %q", got.BookingStatus, "pending")
	}
	if got.BookingID == uuid.Nil {
		t.Error("BookingID: got uuid.Nil, want a generated id")
	}
	if got.ConfirmedAt != nil {
		t.Errorf("ConfirmedAt: got %v, want nil on a new booking", got.ConfirmedAt)
	}
	if got.CancelledAt != nil {
		t.Errorf("CancelledAt: got %v, want nil on a new booking", got.CancelledAt)
	}
}

func TestServiceCreatePassesQuantityToStore(t *testing.T) {
	store := &fakeStore{unit: testUnit(10, 10, 1)}
	svc := NewService(store)

	const qty = 4

	if _, err := svc.Create(context.Background(), testUnitID, testCustomerID, qty, testVisit); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.decQty != qty {
		t.Errorf("decrement qty: got %d, want %d", store.decQty, qty)
	}
	if store.decUnitID != testUnitID {
		t.Errorf("decrement unit id: got %v, want %v", store.decUnitID, testUnitID)
	}
}

func TestServiceGet(t *testing.T) {
	bookingID := uuid.New()
	want := &storage.Booking{
		BookingID:     bookingID,
		UnitID:        testUnitID,
		CustomerID:    testCustomerID,
		Qty:           2,
		TotalMinor:    300_000_000,
		Currency:      "IDR",
		BookingStatus: "pending",
	}

	tests := []struct {
		name    string
		store   *fakeStore
		want    *storage.Booking
		wantErr error
	}{
		{
			name:  "returns the booking",
			store: &fakeStore{booking: want},
			want:  want,
		},
		{
			name:    "propagates booking not found",
			store:   &fakeStore{bookingErr: storage.ErrBookingNotFound},
			wantErr: storage.ErrBookingNotFound,
		},
		{
			name:    "propagates unexpected store error",
			store:   &fakeStore{bookingErr: errors.New("connection reset by peer")},
			wantErr: errors.New("connection reset by peer"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(tt.store)

			got, err := svc.Get(context.Background(), bookingID)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("error: got nil, want %v", tt.wantErr)
				}
				if got != nil {
					t.Errorf("booking: got %+v, want nil on error", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("booking: got %+v, want %+v", got, tt.want)
			}
		})
	}
}
