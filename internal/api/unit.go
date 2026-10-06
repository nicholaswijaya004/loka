package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

// unitResponse is the availability page's view of a unit. It may be up to
// one cache TTL stale; booking re-reads Postgres.
type unitResponse struct {
	UnitID         string  `json:"unit_id"`
	Name           string  `json:"name"`
	Description    *string `json:"description"`
	AvailableUnits int     `json:"available_units"`
	TotalUnits     int     `json:"total_units"`
	Currency       string  `json:"currency"`
	PriceMinor     int64   `json:"price_minor"`
	MinBook        int     `json:"min_book"`
}

func toUnitResponse(u *storage.InventoryUnit) unitResponse {
	return unitResponse{
		UnitID:         u.UnitID.String(),
		Name:           u.Name,
		Description:    u.Description,
		AvailableUnits: u.AvailableUnits,
		TotalUnits:     u.TotalUnits,
		Currency:       u.Currency,
		PriceMinor:     u.PriceMinor,
		MinBook:        u.MinBook,
	}
}

func (h *Handler) GetUnit(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid unit id")
		return
	}

	u, err := h.svc.GetUnit(r.Context(), id)
	if err != nil {
		status, msg := errorResponse(err)
		if status == http.StatusInternalServerError {
			h.logger.Error("get unit", "error", err)
		}
		writeError(w, status, msg)
		return
	}

	writeJSON(w, http.StatusOK, toUnitResponse(u))
}
