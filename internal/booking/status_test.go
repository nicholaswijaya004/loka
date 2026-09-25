package booking

import (
	"errors"
	"testing"
)

func TestCheckTransition(t *testing.T) {
	tests := []struct {
		from, to string
		allowed  bool
	}{
		{StatusPending, StatusPaymentPending, true},
		{StatusPending, StatusCancelled, true},
		{StatusPaymentPending, StatusConfirmed, true},
		{StatusPaymentPending, StatusCancelled, true},
		{StatusPending, StatusConfirmed, false},      // must pay first
		{StatusConfirmed, StatusCancelled, false},    // refunds are a separate flow
		{StatusCancelled, StatusConfirmed, false},    // final state
		{StatusCancelled, StatusPending, false},      // final state
		{StatusPaymentPending, StatusPending, false}, // can't undo a started charge
	}
	for _, tt := range tests {
		t.Run(tt.from+"→"+tt.to, func(t *testing.T) {
			err := checkTransition(tt.from, tt.to)
			if tt.allowed && err != nil {
				t.Errorf("got %v, want allowed", err)
			}
			if !tt.allowed && !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("got %v, want ErrIllegalTransition", err)
			}
		})
	}
}
