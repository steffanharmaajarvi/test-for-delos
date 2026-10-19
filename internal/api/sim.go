package api

import (
	"log/slog"
	"net/http"

	"delos/internal/aml"
	"delos/internal/chain"
	"delos/internal/chain/sim"
	"delos/internal/money"
)

// SimServer exposes the chain simulator and the AML mock over HTTP for manual
// testing. It must never be enabled in production.
type SimServer struct {
	sim   *sim.Simulator
	aml   *aml.Mock
	token chain.Address
	log   *slog.Logger
}

func NewSimServer(s *sim.Simulator, m *aml.Mock, token chain.Address, log *slog.Logger) *SimServer {
	return &SimServer{sim: s, aml: m, token: token, log: log}
}

func (s *SimServer) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /sim/transfers", s.transfer)
	mux.HandleFunc("POST /sim/mine", s.mine)
	mux.HandleFunc("POST /sim/reorg", s.reorg)
	mux.HandleFunc("GET /sim/head", s.head)
	mux.HandleFunc("POST /sim/aml", s.setAML)
}

type simTransferRequest struct {
	From   string       `json:"from"`
	To     string       `json:"to"`
	Amount money.Amount `json:"amount"`
	// Token defaults to the configured USDT contract; set it to simulate a fake token.
	Token  string `json:"token,omitempty"`
	Failed bool   `json:"failed,omitempty"`
}

func (s *SimServer) transfer(w http.ResponseWriter, r *http.Request) {
	var req simTransferRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	token := s.token
	if req.Token != "" {
		token = chain.NormalizeAddress(req.Token)
	}
	if !chain.ValidAddress(req.From) || !chain.ValidAddress(req.To) || !chain.ValidAddress(string(token)) {
		writeError(w, http.StatusBadRequest, "invalid_address", "from, to and token must be 0x-prefixed 20-byte hex addresses")
		return
	}
	log, err := s.sim.Transfer(r.Context(), sim.TransferRequest{
		Token:  token,
		From:   chain.NormalizeAddress(req.From),
		To:     chain.NormalizeAddress(req.To),
		Amount: req.Amount.BaseUnits(),
		Failed: req.Failed,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "sim_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"tx_hash":      log.TxHash,
		"block_number": log.BlockNumber,
		"block_hash":   log.BlockHash,
	})
}

func (s *SimServer) mine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Blocks int `json:"blocks"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Blocks <= 0 || req.Blocks > 1000 {
		writeError(w, http.StatusBadRequest, "invalid_request", `body must be {"blocks": 1..1000}`)
		return
	}
	head, err := s.sim.Mine(r.Context(), req.Blocks)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sim_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]uint64{"head": head})
}

func (s *SimServer) reorg(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Depth     int  `json:"depth"`
		Reinclude bool `json:"reinclude"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.sim.Reorg(r.Context(), req.Depth, req.Reinclude); err != nil {
		writeError(w, http.StatusBadRequest, "sim_error", err.Error())
		return
	}
	s.head(w, r)
}

func (s *SimServer) head(w http.ResponseWriter, r *http.Request) {
	head, err := s.sim.HeadNumber(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sim_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]uint64{"head": head})
}

func (s *SimServer) setAML(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Available *bool `json:"available"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Available == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", `body must be {"available": true|false}`)
		return
	}
	s.aml.SetAvailable(*req.Available)
	s.log.Warn("aml mock availability changed", "available", *req.Available)
	writeJSON(w, http.StatusOK, map[string]bool{"available": s.aml.Available()})
}
