// Package api is the partner-facing HTTP API and the simulator control API.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"delos/internal/invoice"
	"delos/internal/ledger"
	"delos/internal/money"
	"delos/internal/partner"
)

const maxBodyBytes = 64 << 10

type Server struct {
	pool     *pgxpool.Pool
	partners *partner.Store
	invoices *invoice.Service
	log      *slog.Logger
}

func NewServer(pool *pgxpool.Pool, partners *partner.Store, invoices *invoice.Service, log *slog.Logger) *Server {
	return &Server{pool: pool, partners: partners, invoices: invoices, log: log}
}

// Register mounts the partner API on mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Ping(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "database unreachable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("POST /v1/invoices", s.auth(s.createInvoice))
	mux.Handle("GET /v1/invoices/{id}", s.auth(s.getInvoice))
	mux.Handle("GET /v1/balance", s.auth(s.balance))
}

type authedHandler func(w http.ResponseWriter, r *http.Request, p partner.Partner)

// auth resolves "Authorization: Bearer <api key>" to a partner. Handlers get
// the partner explicitly and scope every query by its ID.
func (s *Server) auth(h authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer API key")
			return
		}
		p, err := s.partners.Authenticate(r.Context(), key)
		if errors.Is(err, partner.ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid API key")
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		h(w, r, p)
	})
}

func (s *Server) createInvoice(w http.ResponseWriter, r *http.Request, p partner.Partner) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 255 {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header (1-255 chars) is required")
		return
	}
	var req invoice.CreateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	inv, created, err := s.invoices.Create(r.Context(), p, key, req)
	switch {
	case errors.Is(err, invoice.ErrInvalidAmount):
		writeError(w, http.StatusUnprocessableEntity, "invalid_amount", err.Error())
	case errors.Is(err, invoice.ErrIdempotencyMismatch):
		writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused", err.Error())
	case err != nil:
		s.internalError(w, r, err)
	case created:
		writeJSON(w, http.StatusCreated, inv)
	default:
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, inv)
	}
}

func (s *Server) getInvoice(w http.ResponseWriter, r *http.Request, p partner.Partner) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "invoice not found")
		return
	}
	d, err := s.invoices.Details(r.Context(), p.ID, id)
	if errors.Is(err, invoice.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "invoice not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type balanceResponse struct {
	Currency  string       `json:"currency"`
	Available money.Amount `json:"available"`
	OnHold    money.Amount `json:"on_hold"` // confirmed, waiting for AML
}

func (s *Server) balance(w http.ResponseWriter, r *http.Request, p partner.Partner) {
	avail, err := ledger.Balance(r.Context(), s.pool, ledger.PartnerAvailable(p.ID))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	hold, err := ledger.Balance(r.Context(), s.pool, ledger.PartnerHold(p.ID))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, balanceResponse{Currency: "USDT", Available: avail, OnHold: hold})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after JSON body")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, msg
	writeJSON(w, status, b)
}
