// Command paymock is a fake payment provider for exercising Loka's saga. It
// is not part of Loka. POST /charges is idempotent on the Idempotency-Key
// header, and environment knobs make it slow, decline, or succeed without the
// caller hearing back in time.
//
// Knobs (defaults behave perfectly):
//
//	PORT                    8081
//	LATENCY_MS              0     delay added to every response
//	DECLINE_RATE            0     fraction of new charges declined (0..1)
//	LOST_RESPONSE_RATE      0     fraction of new charges that succeed but answer late (0..1)
//	LOST_RESPONSE_DELAY_MS  30000 how late "late" is; set it above the caller's timeout
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

type chargeRequest struct {
	BookingID  uuid.UUID `json:"booking_id"`
	TotalMinor int64     `json:"total_minor"`
	Currency   string    `json:"currency"`
}

type chargeResponse struct {
	ChargeID      uuid.UUID `json:"charge_id"`
	PaymentStatus string    `json:"payment_status"`
	FailureReason string    `json:"failure_reason,omitempty"`
}

type chargeStore struct {
	mu      sync.Mutex
	charges map[string]chargeResponse
}

func newChargeStore() *chargeStore {
	return &chargeStore{charges: make(map[string]chargeResponse)}
}

type knobs struct {
	port        string
	latency     time.Duration
	declineRate float64
	lostRate    float64
	lostDelay   time.Duration
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	k, err := readKnobs()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store := newChargeStore()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /charges", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req chargeRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			writeError(w, http.StatusBadRequest, "Idempotency-Key header is required")
			return
		}
		if req.BookingID == uuid.Nil {
			writeError(w, http.StatusBadRequest, "booking_id is required")
			return
		}
		if req.TotalMinor <= 0 {
			writeError(w, http.StatusBadRequest, "total_minor must be positive")
			return
		}
		if req.Currency == "" {
			writeError(w, http.StatusBadRequest, "currency is required")
			return
		}

		// Set only when create runs, i.e. for a brand-new charge. A replay
		// never sets it, so a retry is always answered promptly.
		var lostResponse bool

		result, existed := store.getOrCreate(key, func() chargeResponse {
			resp := chargeResponse{ChargeID: uuid.New()}

			if rand.Float64() < k.declineRate {
				resp.PaymentStatus = "declined"
				resp.FailureReason = "insufficient_funds"
				return resp
			}

			resp.PaymentStatus = "succeeded"
			// The money is taken either way; this only decides whether the
			// caller hears about it in time.
			lostResponse = rand.Float64() < k.lostRate
			return resp
		})

		logger.Info("charge",
			"key", key, "booking_id", req.BookingID, "charge_id", result.ChargeID,
			"status", result.PaymentStatus, "replay", existed, "lost_response", lostResponse)

		// Outside the store's lock: sleeping here never blocks other requests.
		delay := k.latency
		if lostResponse {
			delay += k.lostDelay
		}
		if !sleep(r.Context(), delay) {
			return // the caller gave up waiting; nothing left to send
		}

		if existed {
			w.Header().Set("Idempotent-Replay", "true")
		}
		writeJSON(w, http.StatusOK, result)
	})

	srv := &http.Server{
		Addr:        ":" + k.port,
		Handler:     mux,
		ReadTimeout: 5 * time.Second,
		// Must outlast the slowest deliberate response, or the server itself
		// would cut off the lost-response case instead of the caller.
		WriteTimeout: k.latency + k.lostDelay + 10*time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("paymock started",
			"addr", srv.Addr, "latency", k.latency, "decline_rate", k.declineRate,
			"lost_response_rate", k.lostRate, "lost_response_delay", k.lostDelay)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		logger.Error("server failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed, forcing close", "error", err)
		if err := srv.Close(); err != nil {
			logger.Error("force close failed", "error", err)
		}
		os.Exit(1)
	}

	logger.Info("shutdown complete")
}

func readKnobs() (knobs, error) {
	var k knobs
	var err error

	k.port = envOr("PORT", "8081")

	if k.latency, err = envMillis("LATENCY_MS", 0); err != nil {
		return k, err
	}
	if k.lostDelay, err = envMillis("LOST_RESPONSE_DELAY_MS", 30000); err != nil {
		return k, err
	}
	if k.declineRate, err = envRate("DECLINE_RATE"); err != nil {
		return k, err
	}
	if k.lostRate, err = envRate("LOST_RESPONSE_RATE"); err != nil {
		return k, err
	}
	return k, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envMillis(key string, fallback int) (time.Duration, error) {
	n, err := strconv.Atoi(envOr(key, strconv.Itoa(fallback)))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer (milliseconds)", key)
	}
	return time.Duration(n) * time.Millisecond, nil
}

func envRate(key string) (float64, error) {
	f, err := strconv.ParseFloat(envOr(key, "0"), 64)
	if err != nil || f < 0 || f > 1 {
		return 0, fmt.Errorf("%s must be a number between 0 and 1", key)
	}
	return f, nil
}

func (s *chargeStore) getOrCreate(key string, create func() chargeResponse) (chargeResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.charges[key]; ok {
		return r, true
	}
	r := create()
	s.charges[key] = r
	return r, false
}

// sleep waits for d, or returns false early if ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
