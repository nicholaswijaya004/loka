package api

import (
	"github.com/nicholaswijaya004/loka/internal/booking"
	"log/slog"
)

type Handler struct {
	svc    *booking.Service
	logger *slog.Logger
}

func NewHandler(svc *booking.Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}
