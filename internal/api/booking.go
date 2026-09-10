package api

import (
	"github.com/google/uuid"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"net/http"
	"time"
)

type createBookingRequest struct {
	UnitID        string    `json:"unit_id"`
	CustomerID    string    `json:"customer_id"`
	Qty           int       `json:"qty"`
	VisitDateTime time.Time `json:"visit_date_time"`
}

type bookingResponse struct {
	BookingID     string    `json:"booking_id"`
	UnitID        string    `json:"unit_id"`
	CustomerID    string    `json:"customer_id"`
	Qty           int       `json:"qty"`
	VisitDateTime time.Time `json:"visit_date_time"`
	TotalMinor    int64     `json:"total_minor"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

func toResponse(b *storage.Booking) bookingResponse {
	return bookingResponse{
		BookingID:     b.BookingID.String(),
		UnitID:        b.UnitID.String(),
		CustomerID:    b.CustomerID.String(),
		Qty:           b.Qty,
		TotalMinor:    b.TotalMinor,
		Currency:      b.Currency,
		Status:        b.BookingStatus,
		VisitDateTime: b.VisitDateTime,
		CreatedAt:     b.CreatedAt,
	}
}

func (h *Handler) CreateBooking(w http.ResponseWriter, r *http.Request) {
	req, body, ok := decodeJSON[createBookingRequest](w, r)
	if !ok {
		return
	}

	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}

	hash, err := hashRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	unitID, err := uuid.Parse(req.UnitID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid unit_id")
		return
	}

	customerID, err := uuid.Parse(req.CustomerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid customer_id")
		return
	}

	b, replayed, err := h.svc.CreateIdempotent(r.Context(), key, hash, unitID, customerID, req.Qty, req.VisitDateTime)
	if err != nil {
		status, msg := errorResponse(err)
		if status == http.StatusInternalServerError {
			h.logger.Error("create booking", "error", err)
		}
		writeError(w, status, msg)
		return
	}

	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replay", "true")
	}

	writeJSON(w, status, toResponse(b))
}

func (h *Handler) GetBooking(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid booking id")
		return
	}

	b, err := h.svc.Get(r.Context(), id)
	if err != nil {
		status, msg := errorResponse(err)
		if status == http.StatusInternalServerError {
			h.logger.Error("get booking", "error", err)
		}
		writeError(w, status, msg)
		return
	}

	writeJSON(w, http.StatusOK, toResponse(b))
}
