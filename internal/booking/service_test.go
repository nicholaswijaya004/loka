package booking

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

// fakeStore is a hand-written test double for the Store interface.
// Each method returns whatever the test configured, and records how it was called.
type fakeStore struct {
	unit    *storage.InventoryUnit
	unitErr error

	decErr         error
	decCalls       int
	decUnsafeCalls int
	decQty         int
	decNewAvail    int
	decUnitID      uuid.UUID

	forUpdateCalls int
	unitCalls      int

	insErr   error
	insCalls int
	inserted *storage.Booking

	booking    *storage.Booking
	bookingErr error

	claimResult bool
	claimErr    error
	claimCalls  int
	claimKey    string
	claimHash   string

	existingKey *storage.IdempotencyKey
	getKeyErr   error
	getKeyCalls int

	completeErr    error
	completeCalls  int
	completedID    uuid.UUID
	completeStatus int

	optimisticErrs  []error
	optimisticCalls int

	releaseErr   error
	releaseCalls int

	serializableErrs  []error
	serializableCalls int

	withTxCalls int
	withTxErr   error
}

func (f *fakeStore) GetInventoryUnit(ctx context.Context, id uuid.UUID) (*storage.InventoryUnit, error) {
	f.unitCalls++
	if f.unitErr != nil {
		return nil, f.unitErr
	}
	return f.unit, nil
}

func (f *fakeStore) GetInventoryUnitForUpdate(ctx context.Context, id uuid.UUID) (*storage.InventoryUnit, error) {
	f.forUpdateCalls++
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

func (f *fakeStore) DecrementAvailabilityUnsafe(ctx context.Context, id uuid.UUID, newAvailable int) error {
	f.decUnsafeCalls++
	f.decNewAvail = newAvailable
	f.decUnitID = id
	return f.decErr
}

func (f *fakeStore) DecrementAvailabilityOptimistic(ctx context.Context, id uuid.UUID, qty, version int) error {
	i := f.optimisticCalls
	f.optimisticCalls++
	if i < len(f.optimisticErrs) {
		return f.optimisticErrs[i]
	}
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

func (f *fakeStore) ClaimIdempotencyKey(ctx context.Context, key, requestHash string) (bool, error) {
	f.claimCalls++
	f.claimKey = key
	f.claimHash = requestHash
	if f.claimErr != nil {
		return false, f.claimErr
	}
	return f.claimResult, nil
}

func (f *fakeStore) GetIdempotencyKey(ctx context.Context, key string) (*storage.IdempotencyKey, error) {
	f.getKeyCalls++
	if f.getKeyErr != nil {
		return nil, f.getKeyErr
	}
	return f.existingKey, nil
}

func (f *fakeStore) CompleteIdempotencyKey(ctx context.Context, key string, bookingID uuid.UUID, responseStatus int, responseBody []byte) error {
	f.completeCalls++
	f.completedID = bookingID
	f.completeStatus = responseStatus
	return f.completeErr
}

func (f *fakeStore) ReleaseIdempotencyKey(ctx context.Context, key string) error {
	f.releaseCalls++
	return f.releaseErr
}

func (f *fakeStore) WithTx(ctx context.Context, fn func(Store) error) error {
	f.withTxCalls++
	if f.withTxErr != nil {
		return f.withTxErr
	}
	return fn(f)
}

func (f *fakeStore) WithSerializableTx(ctx context.Context, fn func(Store) error) error {
	i := f.serializableCalls
	f.serializableCalls++
	if i < len(f.serializableErrs) && f.serializableErrs[i] != nil {
		return f.serializableErrs[i]
	}
	return fn(f)
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
	testLogger     = slog.New(slog.NewTextHandler(io.Discard, nil))
	testUnitID     = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	testCustomerID = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	testVisit      = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	testKey        = "idem-key-001"
	testHash       = "e3b0c44298fc1c149afbf4c8996fb924"
)

// errStore stands in for any unexpected storage failure — a dropped connection,
// a timeout, anything the service cannot interpret and must propagate unchanged.
var errStore = errors.New("connection reset by peer")

// -----------------------------------------------------------------------------
// Create — the booking work itself, with no idempotency involved
// -----------------------------------------------------------------------------

func TestServiceCreate(t *testing.T) {
	tests := []struct {
		name string

		qty   int
		store *fakeStore

		wantErr      error
		wantDecCalls int
		wantInsCalls int
	}{
		{
			name:         "rejects zero quantity before touching the store",
			qty:          0,
			store:        &fakeStore{unit: testUnit(10, 10, 1)},
			wantErr:      ErrInvalidQty,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "rejects negative quantity",
			qty:          -3,
			store:        &fakeStore{unit: testUnit(10, 10, 1)},
			wantErr:      ErrInvalidQty,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "propagates unit not found",
			qty:          1,
			store:        &fakeStore{unitErr: storage.ErrUnitNotFound},
			wantErr:      storage.ErrUnitNotFound,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "propagates unexpected store error on lookup",
			qty:          1,
			store:        &fakeStore{unitErr: errStore},
			wantErr:      errStore,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "rejects quantity below the unit minimum",
			qty:          1,
			store:        &fakeStore{unit: testUnit(10, 10, 4)},
			wantErr:      ErrMinBook,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "rejects quantity above availability",
			qty:          5,
			store:        &fakeStore{unit: testUnit(3, 10, 1)},
			wantErr:      ErrSoldOut,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			name:         "rejects when availability is exactly zero",
			qty:          1,
			store:        &fakeStore{unit: testUnit(0, 10, 1)},
			wantErr:      ErrSoldOut,
			wantDecCalls: 0,
			wantInsCalls: 0,
		},
		{
			// The race in miniature: availability was read as sufficient, but
			// another request won between the check and the decrement, so the
			// CHECK constraint rejected the update. Storage reports its own
			// ErrSoldOut; the service must translate it rather than leak it.
			name:         "translates storage sold-out from a lost race",
			qty:          1,
			store:        &fakeStore{unit: testUnit(1, 1, 1), decErr: storage.ErrSoldOut},
			wantErr:      ErrSoldOut,
			wantDecCalls: 1,
			wantInsCalls: 0,
		},
		{
			name:         "propagates unexpected decrement failure",
			qty:          1,
			store:        &fakeStore{unit: testUnit(10, 10, 1), decErr: errStore},
			wantErr:      errStore,
			wantDecCalls: 1,
			wantInsCalls: 0,
		},
		{
			// Since day 8 the flow is transactional, so a failed insert rolls the
			// decrement back. The fake cannot model a rollback (its WithTx just
			// calls fn), so this case asserts only that both calls were attempted
			// and the error propagated. The rollback itself is verified by the
			// FAIL_AFTER_DECREMENT k6 run, which held the invariant at 0+10=10.
			name:         "insert failure is propagated and the transaction rolls back",
			qty:          1,
			store:        &fakeStore{unit: testUnit(10, 10, 1), insErr: errStore},
			wantErr:      errStore,
			wantDecCalls: 1,
			wantInsCalls: 1,
		},
		{
			name:         "books a single unit",
			qty:          1,
			store:        &fakeStore{unit: testUnit(10, 10, 1)},
			wantDecCalls: 1,
			wantInsCalls: 1,
		},
		{
			name:         "books the last available unit",
			qty:          1,
			store:        &fakeStore{unit: testUnit(1, 1, 1)},
			wantDecCalls: 1,
			wantInsCalls: 1,
		},
		{
			name:         "books exactly the minimum quantity",
			qty:          4,
			store:        &fakeStore{unit: testUnit(10, 10, 4)},
			wantDecCalls: 1,
			wantInsCalls: 1,
		},
		{
			name:         "books the entire remaining availability",
			qty:          10,
			store:        &fakeStore{unit: testUnit(10, 10, 1)},
			wantDecCalls: 1,
			wantInsCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(tt.store, testLogger, false, "single") // single-statement strategy is the default

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
	svc := NewService(store, testLogger, false, "single")

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
	svc := NewService(store, testLogger, false, "single")

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

// TestServiceUnsafeFlagRouting pins the demonstration harness. The unsafe path
// computes the new availability in Go from a previously-read value, reproducing
// the lost-update anomaly measured on day 5. It must never run without the flag.
func TestServiceUnsafeFlagRouting(t *testing.T) {
	t.Run("unsafe flag set", func(t *testing.T) {
		store := &fakeStore{unit: testUnit(10, 10, 1)}
		svc := NewService(store, testLogger, true, "single")

		if _, err := svc.Create(context.Background(), testUnitID, testCustomerID, 3, testVisit); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if store.decUnsafeCalls != 1 {
			t.Errorf("DecrementAvailabilityUnsafe calls: got %d, want 1", store.decUnsafeCalls)
		}
		if store.decCalls != 0 {
			t.Errorf("DecrementAvailability calls: got %d, want 0 when unsafe is set", store.decCalls)
		}
		if want := 10 - 3; store.decNewAvail != want {
			t.Errorf("new availability: got %d, want %d", store.decNewAvail, want)
		}
	})

	t.Run("unsafe flag clear", func(t *testing.T) {
		store := &fakeStore{unit: testUnit(10, 10, 1)}
		svc := NewService(store, testLogger, false, "single")

		if _, err := svc.Create(context.Background(), testUnitID, testCustomerID, 3, testVisit); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if store.decCalls != 1 {
			t.Errorf("DecrementAvailability calls: got %d, want 1", store.decCalls)
		}
		if store.decUnsafeCalls != 0 {
			t.Errorf("DecrementAvailabilityUnsafe calls: got %d, want 0 by default", store.decUnsafeCalls)
		}
	})
}

// -----------------------------------------------------------------------------
// Get
// -----------------------------------------------------------------------------

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
			store:   &fakeStore{bookingErr: errStore},
			wantErr: errStore,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(tt.store, testLogger, false, "single")

			got, err := svc.Get(context.Background(), bookingID)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("error: got %v, want %v", err, tt.wantErr)
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

// -----------------------------------------------------------------------------
// CreateIdempotent — the claim / work / complete wrapper
// -----------------------------------------------------------------------------

func TestServiceCreateIdempotent(t *testing.T) {
	existingBookingID := uuid.New()

	completedBooking := &storage.Booking{
		BookingID:     existingBookingID,
		UnitID:        testUnitID,
		CustomerID:    testCustomerID,
		Qty:           1,
		TotalMinor:    150_000_000,
		Currency:      "IDR",
		BookingStatus: "pending",
	}

	tests := []struct {
		name  string
		store *fakeStore

		wantErr      error
		wantReplayed bool
		wantBooking  bool

		wantDecCalls      int
		wantInsCalls      int
		wantGetKeyCalls   int
		wantCompleteCalls int
		wantReleaseCalls  int
	}{
		{
			// Won the claim: the booking proceeds and the key is completed.
			name: "claim won books and completes the key",
			store: &fakeStore{
				claimResult: true,
				unit:        testUnit(10, 10, 1),
			},
			wantBooking:       true,
			wantDecCalls:      1,
			wantInsCalls:      1,
			wantGetKeyCalls:   0,
			wantCompleteCalls: 1,
			wantReleaseCalls:  0,
		},
		{
			// Lost the claim to an already-completed request: replay the original
			// booking without consuming inventory again. Zero decrements is the
			// guarantee this whole mechanism exists to provide.
			name: "claim lost to a completed request replays without booking",
			store: &fakeStore{
				claimResult: false,
				existingKey: &storage.IdempotencyKey{
					Key:         testKey,
					RequestHash: testHash,
					State:       "completed",
					BookingID:   &existingBookingID,
				},
				booking: completedBooking,
			},
			wantBooking:     true,
			wantReplayed:    true,
			wantDecCalls:    0,
			wantInsCalls:    0,
			wantGetKeyCalls: 1,
		},
		{
			// Lost the claim to a request still running. The client should retry
			// shortly rather than being told the booking failed.
			name: "claim lost to an in-flight request returns ErrRequestInFlight",
			store: &fakeStore{
				claimResult: false,
				existingKey: &storage.IdempotencyKey{
					Key:         testKey,
					RequestHash: testHash,
					State:       "in_progress",
				},
			},
			wantErr:         ErrRequestInFlight,
			wantGetKeyCalls: 1,
		},
		{
			// Same key, different payload: a client bug. Reject it rather than
			// returning the answer to a different question.
			name: "key reused with a different payload returns ErrKeyReused",
			store: &fakeStore{
				claimResult: false,
				existingKey: &storage.IdempotencyKey{
					Key:         testKey,
					RequestHash: "a-completely-different-hash",
					State:       "completed",
					BookingID:   &existingBookingID,
				},
			},
			wantErr:         ErrKeyReused,
			wantGetKeyCalls: 1,
		},
		{
			// The hash is checked before the state, so a mismatched payload is
			// reported even while the original request is still running.
			name: "hash mismatch is reported even while the original is in flight",
			store: &fakeStore{
				claimResult: false,
				existingKey: &storage.IdempotencyKey{
					Key:         testKey,
					RequestHash: "a-completely-different-hash",
					State:       "in_progress",
				},
			},
			wantErr:         ErrKeyReused,
			wantGetKeyCalls: 1,
		},
		{
			// A completed key with no booking id is a corrupt record. Guard
			// against it rather than dereferencing a nil pointer.
			name: "completed key without a booking id is an error, not a panic",
			store: &fakeStore{
				claimResult: false,
				existingKey: &storage.IdempotencyKey{
					Key:         testKey,
					RequestHash: testHash,
					State:       "completed",
					BookingID:   nil,
				},
			},
			wantErr:         ErrCorruptIdempotencyRecord,
			wantGetKeyCalls: 1,
		},
		{
			// 'failed' is a legal column value but not a state this flow produces.
			// It must not silently fall through into booking the request.
			name: "unrecognised state does not fall through to booking",
			store: &fakeStore{
				claimResult: false,
				existingKey: &storage.IdempotencyKey{
					Key:         testKey,
					RequestHash: testHash,
					State:       "failed",
				},
			},
			wantErr:         ErrUnexpectedIdempotencyState,
			wantDecCalls:    0,
			wantInsCalls:    0,
			wantGetKeyCalls: 1,
		},
		{
			name: "replay propagates a booking fetch failure",
			store: &fakeStore{
				claimResult: false,
				existingKey: &storage.IdempotencyKey{
					Key:         testKey,
					RequestHash: testHash,
					State:       "completed",
					BookingID:   &existingBookingID,
				},
				bookingErr: errStore,
			},
			wantErr:         errStore,
			wantGetKeyCalls: 1,
		},
		{
			name: "propagates a claim failure without booking",
			store: &fakeStore{
				claimErr: errStore,
			},
			wantErr:         errStore,
			wantGetKeyCalls: 0,
		},
		{
			name: "propagates a lookup failure after losing the claim",
			store: &fakeStore{
				claimResult: false,
				getKeyErr:   errStore,
			},
			wantErr:         errStore,
			wantGetKeyCalls: 1,
		},
		{
			// The booking failed after the claim was taken, so the claim is
			// released and a genuine retry can start fresh rather than seeing
			// 409 forever against work that never happened.
			name: "sold out releases the claim",
			store: &fakeStore{
				claimResult: true,
				unit:        testUnit(0, 10, 1),
			},
			wantErr:          ErrSoldOut,
			wantDecCalls:     0,
			wantReleaseCalls: 1,
		},
		{
			name: "decrement failure releases the claim",
			store: &fakeStore{
				claimResult: true,
				unit:        testUnit(10, 10, 1),
				decErr:      errStore,
			},
			wantErr:          errStore,
			wantDecCalls:     1,
			wantReleaseCalls: 1,
		},
		{
			name: "insert failure releases the claim",
			store: &fakeStore{
				claimResult: true,
				unit:        testUnit(10, 10, 1),
				insErr:      errStore,
			},
			wantErr:          errStore,
			wantDecCalls:     1,
			wantInsCalls:     1,
			wantReleaseCalls: 1,
		},
		{
			// The winner released its claim between our failed INSERT and this
			// lookup. The key is free again, so the client should retry rather
			// than receive a 500.
			name: "claim released by the winner mid-race is retriable",
			store: &fakeStore{
				claimResult: false,
				getKeyErr:   storage.ErrIdempotencyKeyNotFound,
			},
			wantErr:         ErrRequestInFlight,
			wantGetKeyCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(tt.store, testLogger, false, "single")

			got, replayed, err := svc.CreateIdempotent(
				context.Background(), testKey, testHash,
				testUnitID, testCustomerID, 1, testVisit,
			)

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
				if tt.wantBooking && got == nil {
					t.Error("booking: got nil, want a booking")
				}
			}

			if replayed != tt.wantReplayed {
				t.Errorf("replayed: got %v, want %v", replayed, tt.wantReplayed)
			}
			if tt.store.claimCalls != 1 {
				t.Errorf("ClaimIdempotencyKey calls: got %d, want 1", tt.store.claimCalls)
			}
			if tt.store.getKeyCalls != tt.wantGetKeyCalls {
				t.Errorf("GetIdempotencyKey calls: got %d, want %d", tt.store.getKeyCalls, tt.wantGetKeyCalls)
			}
			if tt.store.decCalls != tt.wantDecCalls {
				t.Errorf("DecrementAvailability calls: got %d, want %d", tt.store.decCalls, tt.wantDecCalls)
			}
			if tt.store.insCalls != tt.wantInsCalls {
				t.Errorf("InsertBooking calls: got %d, want %d", tt.store.insCalls, tt.wantInsCalls)
			}
			if tt.store.completeCalls != tt.wantCompleteCalls {
				t.Errorf("CompleteIdempotencyKey calls: got %d, want %d", tt.store.completeCalls, tt.wantCompleteCalls)
			}
			if tt.store.releaseCalls != tt.wantReleaseCalls {
				t.Errorf("ReleaseIdempotencyKey calls: got %d, want %d", tt.store.releaseCalls, tt.wantReleaseCalls)
			}
		})
	}
}

// TestServiceCreateIdempotentClaimsBeforeBooking pins the ordering the whole
// mechanism depends on. If the claim were attempted after the booking, every
// concurrent retry would consume inventory before discovering it was a
// duplicate — precisely the race this exists to prevent.
func TestServiceCreateIdempotentClaimsBeforeBooking(t *testing.T) {
	store := &fakeStore{
		claimResult: false,
		existingKey: &storage.IdempotencyKey{
			Key:         testKey,
			RequestHash: testHash,
			State:       "in_progress",
		},
	}
	svc := NewService(store, testLogger, false, "single")

	_, _, err := svc.CreateIdempotent(
		context.Background(), testKey, testHash,
		testUnitID, testCustomerID, 1, testVisit,
	)

	if !errors.Is(err, ErrRequestInFlight) {
		t.Fatalf("error: got %v, want %v", err, ErrRequestInFlight)
	}
	if store.claimCalls != 1 {
		t.Errorf("ClaimIdempotencyKey calls: got %d, want 1", store.claimCalls)
	}
	if store.decCalls != 0 {
		t.Errorf("DecrementAvailability calls: got %d, want 0 — no inventory work on a lost claim", store.decCalls)
	}
	if store.insCalls != 0 {
		t.Errorf("InsertBooking calls: got %d, want 0 — no inventory work on a lost claim", store.insCalls)
	}
}

func TestServiceCreateIdempotentCompletesWithBookingID(t *testing.T) {
	store := &fakeStore{
		claimResult: true,
		unit:        testUnit(10, 10, 1),
	}
	svc := NewService(store, testLogger, false, "single")

	got, replayed, err := svc.CreateIdempotent(
		context.Background(), testKey, testHash,
		testUnitID, testCustomerID, 1, testVisit,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if replayed {
		t.Error("replayed: got true, want false on a fresh booking")
	}
	if store.completedID != got.BookingID {
		t.Errorf("completed booking id: got %v, want %v", store.completedID, got.BookingID)
	}
	if store.completeStatus != 201 {
		t.Errorf("completed status: got %d, want 201", store.completeStatus)
	}
	if store.claimKey != testKey {
		t.Errorf("claimed key: got %q, want %q", store.claimKey, testKey)
	}
	if store.claimHash != testHash {
		t.Errorf("claimed hash: got %q, want %q", store.claimHash, testHash)
	}
}

// TestServiceCreateIdempotentReplayReturnsOriginalBooking checks that a replay
// hands back the booking recorded against the key rather than creating a new one.
func TestServiceCreateIdempotentReplayReturnsOriginalBooking(t *testing.T) {
	originalID := uuid.New()
	original := &storage.Booking{
		BookingID:     originalID,
		UnitID:        testUnitID,
		CustomerID:    testCustomerID,
		Qty:           1,
		TotalMinor:    150_000_000,
		Currency:      "IDR",
		BookingStatus: "pending",
	}

	store := &fakeStore{
		claimResult: false,
		existingKey: &storage.IdempotencyKey{
			Key:         testKey,
			RequestHash: testHash,
			State:       "completed",
			BookingID:   &originalID,
		},
		booking: original,
	}
	svc := NewService(store, testLogger, false, "single")

	got, replayed, err := svc.CreateIdempotent(
		context.Background(), testKey, testHash,
		testUnitID, testCustomerID, 1, testVisit,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !replayed {
		t.Error("replayed: got false, want true")
	}
	if got.BookingID != originalID {
		t.Errorf("booking id: got %v, want %v — a replay must return the original booking",
			got.BookingID, originalID)
	}
	if store.insCalls != 0 {
		t.Errorf("InsertBooking calls: got %d, want 0 on a replay", store.insCalls)
	}
	if store.completeCalls != 0 {
		t.Errorf("CompleteIdempotencyKey calls: got %d, want 0 — the key is already complete", store.completeCalls)
	}
}

// TestServiceCreateIdempotentCompleteFailureAfterBooking documents a case with
// no clean answer. The booking succeeded but the key could not be marked
// complete, so the key is stuck in_progress and every retry will receive 409
// for a booking that actually exists. Returning the error surfaces the problem;
// the alternative — returning the booking and leaving a poisoned key behind —
// hides it. This test pins the choice so a future change is deliberate.
func TestServiceCreateIdempotentCompleteFailureAfterBooking(t *testing.T) {
	store := &fakeStore{
		claimResult: true,
		unit:        testUnit(10, 10, 1),
		completeErr: errStore,
	}
	svc := NewService(store, testLogger, false, "single")

	got, _, err := svc.CreateIdempotent(
		context.Background(), testKey, testHash,
		testUnitID, testCustomerID, 1, testVisit,
	)

	if !errors.Is(err, errStore) {
		t.Errorf("error: got %v, want %v", err, errStore)
	}
	if got != nil {
		t.Errorf("booking: got %+v, want nil", got)
	}
	if store.insCalls != 1 {
		t.Errorf("InsertBooking calls: got %d, want 1 — the booking did happen", store.insCalls)
	}
	if store.completeCalls != 1 {
		t.Errorf("CompleteIdempotencyKey calls: got %d, want 1", store.completeCalls)
	}
}

// TestServiceCreateIdempotentReleaseFailurePreservesOriginalError checks that a
// cleanup failure never masks the error that caused the cleanup. A caller told
// "release failed" instead of "sold out" cannot respond correctly.
func TestServiceCreateIdempotentReleaseFailurePreservesOriginalError(t *testing.T) {
	releaseErr := errors.New("release failed")
	store := &fakeStore{
		claimResult: true,
		unit:        testUnit(0, 10, 1),
		releaseErr:  releaseErr,
	}
	svc := NewService(store, testLogger, false, "single")

	_, _, err := svc.CreateIdempotent(
		context.Background(), testKey, testHash,
		testUnitID, testCustomerID, 1, testVisit,
	)

	if !errors.Is(err, ErrSoldOut) {
		t.Errorf("error: got %v, want %v — the release failure must not replace it", err, ErrSoldOut)
	}
	if errors.Is(err, releaseErr) {
		t.Error("the release failure leaked into the returned error")
	}
	if store.releaseCalls != 1 {
		t.Errorf("ReleaseIdempotencyKey calls: got %d, want 1", store.releaseCalls)
	}
}

func TestServiceCreateIdempotentReleasesOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := &fakeStore{
		claimResult: true,
		unit:        testUnit(0, 10, 1), // forces Create to fail
	}
	svc := NewService(store, testLogger, false, "single")

	_, _, err := svc.CreateIdempotent(ctx, testKey, testHash,
		testUnitID, testCustomerID, 1, testVisit)

	if !errors.Is(err, ErrSoldOut) {
		t.Errorf("error: got %v, want %v", err, ErrSoldOut)
	}
	if store.releaseCalls != 1 {
		t.Errorf("ReleaseIdempotencyKey calls: got %d, want 1 — cleanup must run on a dead context",
			store.releaseCalls)
	}
}

// TestServiceCreateStrategyRouting pins which read path each strategy takes.
// The distinction matters: the single-statement strategy reads availability
// outside the transaction and relies on the decrement being safe on its own,
// while the for-update strategy reads a locked row inside the transaction so
// the value cannot change before it acts on it.
func TestServiceCreateStrategyRouting(t *testing.T) {
	tests := []struct {
		name     string
		strategy string

		wantUnitCalls      int
		wantForUpdateCalls int
	}{
		{
			name:               "for-update strategy reads the locked row",
			strategy:           "forupdate",
			wantUnitCalls:      0,
			wantForUpdateCalls: 1,
		},
		{
			name:               "single-statement strategy reads without a lock",
			strategy:           "single",
			wantUnitCalls:      1,
			wantForUpdateCalls: 0,
		},
		{
			name:               "an unrecognised strategy falls back to single-statement",
			strategy:           "nonsense",
			wantUnitCalls:      1,
			wantForUpdateCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{unit: testUnit(10, 10, 1)}
			svc := NewService(store, testLogger, false, tt.strategy)

			got, err := svc.Create(context.Background(), testUnitID, testCustomerID, 1, testVisit)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("booking: got nil, want a booking")
			}

			if store.unitCalls != tt.wantUnitCalls {
				t.Errorf("GetInventoryUnit calls: got %d, want %d", store.unitCalls, tt.wantUnitCalls)
			}
			if store.forUpdateCalls != tt.wantForUpdateCalls {
				t.Errorf("GetInventoryUnitForUpdate calls: got %d, want %d",
					store.forUpdateCalls, tt.wantForUpdateCalls)
			}
			if store.withTxCalls != 1 {
				t.Errorf("WithTx calls: got %d, want 1 — both strategies must be transactional",
					store.withTxCalls)
			}
		})
	}
}

func TestServiceCreateOptimisticRetriesOnVersionConflict(t *testing.T) {
	store := &fakeStore{
		unit:           testUnit(10, 10, 1),
		optimisticErrs: []error{storage.ErrVersionConflict, storage.ErrVersionConflict, nil},
	}
	svc := NewService(store, testLogger, false, "optimistic")
	svc.maxOptimisticRetries = 3

	got, err := svc.Create(context.Background(), testUnitID, testCustomerID, 1, testVisit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("booking: got nil, want a booking")
	}
	if svc.retries.Load() != 2 {
		t.Errorf("retries: got %d, want 2", svc.retries.Load())
	}
}

func TestServiceCreateOptimisticExhaustsRetries(t *testing.T) {
	const testRetries = 3

	errs := make([]error, testRetries)
	for i := range errs {
		errs[i] = storage.ErrVersionConflict
	}
	store := &fakeStore{unit: testUnit(10, 10, 1), optimisticErrs: errs}
	svc := NewService(store, testLogger, false, "optimistic")
	svc.maxOptimisticRetries = testRetries

	_, err := svc.Create(context.Background(), testUnitID, testCustomerID, 1, testVisit)
	if !errors.Is(err, ErrTooManyRetries) {
		t.Errorf("error: got %v, want %v", err, ErrTooManyRetries)
	}
	if got := svc.retries.Load(); got != testRetries {
		t.Errorf("retries: got %d, want %d", got, testRetries)
	}
}

func TestServiceCreateSerializableRetriesOnSerializationFailure(t *testing.T) {
	store := &fakeStore{
		unit:             testUnit(10, 10, 1),
		serializableErrs: []error{storage.ErrSerializationFailure, storage.ErrSerializationFailure, nil},
	}
	svc := NewService(store, testLogger, false, "serializable")

	got, err := svc.Create(context.Background(), testUnitID, testCustomerID, 1, testVisit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("booking: got nil, want a booking")
	}
	if svc.retries.Load() != 2 {
		t.Errorf("retries: got %d, want 2", svc.retries.Load())
	}
}

func TestServiceCreateSerializableExhaustsRetries(t *testing.T) {
	const testRetries = 3
	errs := make([]error, testRetries)
	for i := range errs {
		errs[i] = storage.ErrSerializationFailure
	}
	store := &fakeStore{unit: testUnit(10, 10, 1), serializableErrs: errs}
	svc := NewService(store, testLogger, false, "serializable")
	svc.maxSerializableRetries = testRetries

	_, err := svc.Create(context.Background(), testUnitID, testCustomerID, 1, testVisit)
	if !errors.Is(err, ErrTooManyRetries) {
		t.Errorf("error: got %v, want %v", err, ErrTooManyRetries)
	}
	if got := svc.retries.Load(); got != testRetries {
		t.Errorf("retries: got %d, want %d", got, testRetries)
	}
}
