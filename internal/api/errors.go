package api

import (
	"errors"
	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"net/http"
)

type errorBody struct {
	Error string `json:"error"`
}

func errorResponse(err error) (int, string) {
	switch {
	case errors.Is(err, booking.ErrInvalidQty):
		return http.StatusBadRequest, "invalid quantity"
	case errors.Is(err, booking.ErrMinBook):
		return http.StatusBadRequest, "quantity below minimum"
	case errors.Is(err, storage.ErrUnitNotFound):
		return http.StatusNotFound, "unit not found"
	case errors.Is(err, storage.ErrBookingNotFound):
		return http.StatusNotFound, "booking not found"
	case errors.Is(err, booking.ErrSoldOut):
		return http.StatusConflict, "sold out"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}
