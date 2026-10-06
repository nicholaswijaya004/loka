package booking

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// A v1 event, written before unit_id and qty existed, still decodes: the
// relay may deliver old rows after the deploy. The new fields come back
// zero, which is how the cache invalidator recognises an event it must skip.
func TestCancelledPayloadV1StillDecodes(t *testing.T) {
	v1 := `{"version":1,"booking_id":"6f1c2a5e-0b7d-4e8a-9c3f-2d4b6a8e0f13",` +
		`"customer_id":"11111111-1111-1111-1111-111111111111",` +
		`"reason":"expired","cancelled_at":"2026-10-06T09:00:00Z"}`

	var p CancelledPayload
	if err := json.Unmarshal([]byte(v1), &p); err != nil {
		t.Fatalf("decode v1: %v", err)
	}
	if p.Version != 1 || p.Reason != ReasonExpired {
		t.Errorf("v1 fields: got version %d reason %q", p.Version, p.Reason)
	}
	if p.UnitID != uuid.Nil || p.Qty != 0 {
		t.Errorf("fields v1 never had: got unit %s qty %d, want uuid.Nil and 0", p.UnitID, p.Qty)
	}
}
