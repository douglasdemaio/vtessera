package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/douglasdemaio/vtessera/internal/agp"
	"github.com/douglasdemaio/vtessera/internal/auth"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/ledger"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/douglasdemaio/vtessera/internal/trade"
)

const (
	codeInternal       = -32603
	codeInvalidParams  = -32602
	codeMethodNotFound = -32601
)

type Server struct {
	registry      *registry.Service
	trades        *trade.Service
	auth          *auth.Service
	agp           *agp.Routing
	ledger        *ledger.Ledger
	tokens        tokens.Registry
	mux           *http.ServeMux
	version       string
	agentCardBody map[string]any
}

type Options struct {
	Registry *registry.Service
	Trades   *trade.Service
	Auth     *auth.Service
	Ledger   *ledger.Ledger
	// Tokens is the governed mint allowlist published by GET /v1/tokens.
	Tokens  tokens.Registry
	Version string
}

func New(opts Options) *Server {
	s := &Server{
		registry: opts.Registry,
		trades:   opts.Trades,
		auth:     opts.Auth,
		agp:      agp.NewRouting(),
		ledger:   opts.Ledger,
		tokens:   opts.Tokens,
		version:  opts.Version,
		mux:      http.NewServeMux(),
	}
	s.agentCardBody = s.buildAgentCard()
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Vtessera-Version", s.version)
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /.well-known/agent-card.json", s.handleAgentCard)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /v1/agents", s.handleListAgents)
	s.mux.HandleFunc("GET /v1/agents/{id}", s.handleGetAgent)
	s.mux.HandleFunc("PUT /v1/agents/{id}/card", s.authed(s.handlePutCard))
	s.mux.HandleFunc("GET /v1/agents/{id}/offers", s.handleAgentOffers)
	s.mux.HandleFunc("GET /v1/offers", s.handleSearchOffers)
	s.mux.HandleFunc("POST /v1/agents/{id}/offers", s.authed(s.handlePublishOffer))
	s.mux.HandleFunc("GET /v1/offers/{id}", s.handleGetOffer)
	s.mux.HandleFunc("POST /v1/offers/{id}/close", s.authed(s.handleCloseOffer))
	s.mux.HandleFunc("POST /v1/trades", s.authed(s.handleCreateTrade))
	s.mux.HandleFunc("GET /v1/trades/{id}", s.authed(s.handleGetTrade))
	s.mux.HandleFunc("POST /v1/trades/{id}/negotiate", s.authed(s.handleNegotiate))
	s.mux.HandleFunc("POST /v1/trades/{id}/accept", s.authed(s.handleAccept))
	s.mux.HandleFunc("POST /v1/trades/{id}/record", s.authed(s.handleRecord))
	s.mux.HandleFunc("POST /v1/trades/{id}/cancel", s.authed(s.handleCancel))
	s.mux.HandleFunc("POST /v1/trades/{id}/dispute", s.authed(s.handleDispute))
	s.mux.HandleFunc("POST /v1/trades/{id}/settlement", s.authed(s.handleBuildSettlement))
	s.mux.HandleFunc("GET /v1/trades/{id}/settlement", s.authed(s.handleSettlementRequest))
	s.mux.HandleFunc("POST /v1/trades/{id}/confirm", s.authed(s.handleConfirmSettlement))
	s.mux.HandleFunc("GET /v1/tokens", s.handleListTokens)
	s.mux.HandleFunc("GET /v1/tesseras/{tradeID}", s.authed(s.handleTessera))
	s.mux.HandleFunc("GET /v1/ledger", s.handleLedger)
	s.mux.HandleFunc("GET /v1/ledger/head", s.handleLedgerHead)
	s.mux.HandleFunc("GET /v1/metrics", s.handleMetrics)
	s.mux.HandleFunc("POST /v1/auth/challenge", s.handleChallenge)
	s.mux.HandleFunc("POST /v1/auth/verify", s.handleVerify)
	s.mux.HandleFunc("POST /agp/route", s.handleAGPRoute)
	s.mux.HandleFunc("GET /agp/table", s.handleAGPTable)
}

func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.agentCardBody)
}

func (s *Server) buildAgentCard() map[string]any {
	return map[string]any{
		"name":               "vtessera marketplace",
		"description":        "A2A marketplace gateway: routes Intents to the cheapest policy-compliant agent and issues hash-chain anchored virtual tessera receipts.",
		"version":            s.version,
		"url":                "https://vtessera.example.com",
		"protocolVersion":    "0.3.0",
		"preferredTransport": "JSONRPC",
		"capabilities": map[string]any{
			"streaming":              false,
			"pushNotifications":      false,
			"stateTransitionHistory": true,
			"extensions": []map[string]any{
				{
					"uri": agp.ExtensionURI,
					"params": map[string]any{
						"agent_role":             agp.GatewayRole,
						"supported_agp_versions": agp.SupportedVersions,
					},
				},
			},
		},
		"defaultInputModes":  []string{"application/json"},
		"defaultOutputModes": []string{"application/json"},
		"skills": []map[string]any{
			{
				"id":          "agp_route_intent",
				"name":        "AGP Intent routing",
				"description": "Routes an AGP Intent to the cheapest agent whose announced policy satisfies every constraint.",
				"tags":        []string{"agp", "gateway", "routing"},
			},
			{
				"id":          "tessera_receipt",
				"name":        "Virtual tessera receipt",
				"description": "Issues and verifies a hash-chain anchored, Ed25519 signed tessera for a recorded trade.",
				"tags":        []string{"tessera", "receipt", "ledger"},
			},
		},
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"version":         s.version,
		"verificationKey": s.ledger.VerificationKey(),
		"agp": map[string]any{
			"version":   agp.Version,
			"extension": agp.ExtensionURI,
		},
	})
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.registry.Agents(r.Context(), domain.AgentStatus(r.URL.Query().Get("status")))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	agent, err := s.registry.Agent(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handlePutCard(w http.ResponseWriter, r *http.Request) {
	var card domain.AgentCard
	if !decode(w, r, &card) {
		return
	}
	agent, _, err := s.registry.Register(r.Context(), r.PathValue("id"), card)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleAgentOffers(w http.ResponseWriter, r *http.Request) {
	offers, err := s.registry.Search(r.Context(), domain.OfferQuery{
		AgentID: r.PathValue("id"),
		Status:  domain.OfferOpen,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"offers": offers})
}

func (s *Server) handleSearchOffers(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, _ := strconv.Atoi(query.Get("limit"))
	offset, _ := strconv.Atoi(query.Get("offset"))
	offers, err := s.registry.Search(r.Context(), domain.OfferQuery{
		Direction:  domain.OfferDirection(query.Get("direction")),
		Status:     domain.OfferStatus(query.Get("status")),
		Capability: query.Get("capability"),
		Text:       query.Get("q"),
		Mint:       query.Get("mint"),
		Mode:       domain.SettlementMode(query.Get("mode")),
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"offers": offers})
}

func (s *Server) handleGetOffer(w http.ResponseWriter, r *http.Request) {
	offer, err := s.registry.Offer(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, offer)
}

type publishOfferRequest struct {
	Direction       string   `json:"direction"`
	Description     string   `json:"description"`
	Capabilities    []string `json:"capabilities"`
	PriceAmount     string   `json:"priceAmount"`
	PriceMint       string   `json:"priceMint"`
	SettlementModes []string `json:"settlementModes"`
	IdempotencyKey  string   `json:"idempotencyKey"`
}

func (s *Server) handlePublishOffer(w http.ResponseWriter, r *http.Request) {
	var req publishOfferRequest
	if !decode(w, r, &req) {
		return
	}
	offer, created, err := s.registry.PublishOffer(r.Context(), r.PathValue("id"), registry.NewOffer{
		Direction:       domain.OfferDirection(req.Direction),
		Description:     req.Description,
		Capabilities:    req.Capabilities,
		PriceAmount:     req.PriceAmount,
		PriceMint:       req.PriceMint,
		SettlementModes: toModes(req.SettlementModes),
	}, req.IdempotencyKey)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, statusFor(created), offer)
}

func toModes(in []string) []domain.SettlementMode {
	out := make([]domain.SettlementMode, 0, len(in))
	for _, m := range in {
		out = append(out, domain.SettlementMode(m))
	}
	return out
}

func (s *Server) handleCloseOffer(w http.ResponseWriter, r *http.Request) {
	offer, err := s.registry.CloseOffer(r.Context(), agentFrom(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, offer)
}

type createTradeRequest struct {
	OfferID        string `json:"offerId"`
	SettlementMode string `json:"settlementMode"`
	IdempotencyKey string `json:"idempotencyKey"`
}

func (s *Server) handleCreateTrade(w http.ResponseWriter, r *http.Request) {
	var req createTradeRequest
	if !decode(w, r, &req) {
		return
	}
	created, isNew, err := s.trades.Create(r.Context(), agentFrom(r), req.OfferID, domain.SettlementMode(req.SettlementMode), req.IdempotencyKey)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, statusFor(isNew), created)
}

func (s *Server) handleGetTrade(w http.ResponseWriter, r *http.Request) {
	created, err := s.trades.Get(r.Context(), agentFrom(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (s *Server) handleNegotiate(w http.ResponseWriter, r *http.Request) {
	s.tradeAction(w, r, func(id string) (any, error) {
		return s.trades.BeginNegotiation(r.Context(), agentFrom(r), id)
	})
}

func (s *Server) handleAccept(w http.ResponseWriter, r *http.Request) {
	s.tradeAction(w, r, func(id string) (any, error) {
		return s.trades.Accept(r.Context(), agentFrom(r), id)
	})
}

func (s *Server) handleRecord(w http.ResponseWriter, r *http.Request) {
	created, receipt, err := s.trades.Record(r.Context(), agentFrom(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"trade":   created,
		"receipt": receipt,
		"tessera": tesseraPayload(s.ledger, receipt.JWS),
	})
}

func (s *Server) tradeAction(w http.ResponseWriter, r *http.Request, action func(string) (any, error)) {
	result, err := action(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	var req reasonRequest
	if !decodeOptional(w, r, &req) {
		return
	}
	created, err := s.trades.Cancel(r.Context(), agentFrom(r), r.PathValue("id"), req.Reason)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (s *Server) handleDispute(w http.ResponseWriter, r *http.Request) {
	var req reasonRequest
	if !decodeOptional(w, r, &req) {
		return
	}
	created, err := s.trades.Dispute(r.Context(), agentFrom(r), r.PathValue("id"), req.Reason)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (s *Server) handleTessera(w http.ResponseWriter, r *http.Request) {
	receipt, err := s.trades.Tessera(r.Context(), agentFrom(r), r.PathValue("tradeID"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tesseraPayload(s.ledger, receipt.JWS))
}

func tesseraPayload(led *ledger.Ledger, jws string) map[string]any {
	payload := map[string]any{
		"jws":              jws,
		"verificationKey":  led.VerificationKey(),
		"signingAlgorithm": "EdDSA",
		"keyEncoding":      "base58",
	}
	if claims, err := led.Verify(jws); err == nil {
		payload["claims"] = claims
	}
	return payload
}

func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	entries, err := s.ledger.Entries(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"genesis":         domain.GenesisHash,
		"entries":         entries,
		"verificationKey": s.ledger.VerificationKey(),
	})
}

func (s *Server) handleLedgerHead(w http.ResponseWriter, r *http.Request) {
	entries, err := s.ledger.Entries(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	body := map[string]any{"genesis": domain.GenesisHash, "head": nil, "length": len(entries)}
	if len(entries) > 0 {
		body["head"] = entries[len(entries)-1]
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m, err := s.trades.UsageMetrics(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

type challengeRequest struct {
	AgentID string `json:"agentId"`
}

func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request) {
	var req challengeRequest
	if !decode(w, r, &req) {
		return
	}
	challenge, err := s.auth.IssueChallenge(r.Context(), req.AgentID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"challengeId":     challenge.ID,
		"agentId":         challenge.AgentID,
		"nonce":           challenge.Nonce,
		"expiresAt":       challenge.ExpiresAt,
		"algorithm":       "Ed25519",
		"messageTemplate": auth.MessageTemplate,
	})
}

type verifyRequest struct {
	ChallengeID string `json:"challengeId"`
	Signature   string `json:"signature"`
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if !decode(w, r, &req) {
		return
	}
	session, err := s.auth.Redeem(r.Context(), req.ChallengeID, req.Signature)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agentId":   session.AgentID,
		"token":     session.Token,
		"tokenType": "Bearer",
		"expiresAt": session.ExpiresAt,
	})
}

func (s *Server) handleAGPTable(w http.ResponseWriter, r *http.Request) {
	table, err := s.agp.BuildTable(r.Context(), s.registry.AnnouncementSource())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, rpcError(nil, codeInternal, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, table)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (s *Server) handleAGPRoute(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Method != "" && req.Method != "agp/route_intent" && req.Method != "agp/route" {
		writeJSON(w, http.StatusOK, rpcError(req.ID, codeMethodNotFound, "unknown method: "+req.Method))
		return
	}
	if len(req.Params) == 0 {
		writeJSON(w, http.StatusOK, rpcError(req.ID, codeInvalidParams, "params with a target_capability is required"))
		return
	}
	var intent agp.Intent
	if err := json.Unmarshal(req.Params, &intent); err != nil {
		writeJSON(w, http.StatusOK, rpcError(req.ID, codeInvalidParams, "invalid intent: "+err.Error()))
		return
	}
	table, err := s.agp.BuildTable(r.Context(), s.registry.AnnouncementSource())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, rpcError(req.ID, codeInternal, err.Error()))
		return
	}
	result, err := table.Route(intent)
	if err != nil {
		code := codeInternal
		if mapped, ok := agp.CodeOf(err); ok {
			code = mapped
		} else if errors.Is(err, agp.ErrInvalidIntent) {
			code = codeInvalidParams
		}
		writeJSON(w, http.StatusOK, rpcError(req.ID, code, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      rawOrNull(req.ID),
		"result":  result,
	})
}

func rawOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      rawOrNull(id),
		"error":   map[string]any{"code": code, "message": message},
	}
}

type agentContextKey struct{}

func (s *Server) authed(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID, err := s.authenticate(r)
		if err != nil {
			writeError(w, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), agentContextKey{}, agentID)))
	}
}

func agentFrom(r *http.Request) string {
	if id, ok := r.Context().Value(agentContextKey{}).(string); ok {
		return id
	}
	return ""
}

func (s *Server) authenticate(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if strings.TrimSpace(header) == "" {
		return "", fmt.Errorf("%w: missing Authorization header", auth.ErrSessionInvalid)
	}
	return s.auth.Authenticate(header)
}

const maxBodyBytes = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(dst); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func decodeOptional(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 {
		return true
	}
	return decode(w, r, dst)
}

func statusFor(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
}

var statusByError = []struct {
	err    error
	status int
}{
	{domain.ErrNotFound, http.StatusNotFound},
	{domain.ErrConflict, http.StatusConflict},
	{domain.ErrStale, http.StatusConflict},
	{registry.ErrNotOwner, http.StatusForbidden},
	{registry.ErrAgentSuspended, http.StatusForbidden},
	{registry.ErrCurrencyNotAccepted, http.StatusBadRequest},
	{registry.ErrAmountTooPrecise, http.StatusBadRequest},
	{registry.ErrOfferClosed, http.StatusConflict},
	{trade.ErrNotParty, http.StatusForbidden},
	{trade.ErrIllegalState, http.StatusConflict},
	{trade.ErrOfferUnavailable, http.StatusConflict},
	{trade.ErrModeNotAccepted, http.StatusBadRequest},
	{trade.ErrAgentUnavailable, http.StatusConflict},
	{trade.ErrOnchainUnavailable, http.StatusNotImplemented},
	{trade.ErrNotBuyer, http.StatusForbidden},
	{trade.ErrNotOnchain, http.StatusConflict},
	{trade.ErrSettlementLive, http.StatusConflict},
	{trade.ErrSettlementFailed, http.StatusConflict},
	{trade.ErrSettlementPending, http.StatusAccepted},
	{trade.ErrInvalidSignature, http.StatusBadRequest},
	{settlement.ErrMismatch, http.StatusConflict},
	{auth.ErrSessionInvalid, http.StatusUnauthorized},
	{auth.ErrInvalidSignature, http.StatusUnauthorized},
	{auth.ErrSecretTooShort, http.StatusInternalServerError},
	{agp.ErrRouteNotFound, http.StatusNotFound},
	{agp.ErrPolicyViolation, http.StatusUnprocessableEntity},
	{agp.ErrTableStale, http.StatusConflict},
	{ledger.ErrNotSettled, http.StatusConflict},
	{ledger.ErrAlreadyIssued, http.StatusConflict},
}

func writeError(w http.ResponseWriter, err error) {
	status, matched := http.StatusInternalServerError, false
	for _, candidate := range statusByError {
		if errors.Is(err, candidate.err) {
			status, matched = candidate.status, true
			break
		}
	}
	if code, ok := agp.CodeOf(err); ok {
		writeJSON(w, status, rpcError(nil, code, err.Error()))
		return
	}
	if !matched {
		writeErrorStatus(w, status, "INTERNAL", err.Error())
		return
	}
	writeErrorStatus(w, status, codeName(err), err.Error())
}

func writeErrorStatus(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": message, "code": code})
}

func codeName(err error) string {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return "NOT_FOUND"
	case errors.Is(err, domain.ErrConflict), errors.Is(err, domain.ErrStale):
		return "CONFLICT"
	case errors.Is(err, trade.ErrIllegalState):
		return "ILLEGAL_STATE"
	case errors.Is(err, trade.ErrNotParty), errors.Is(err, registry.ErrNotOwner):
		return "FORBIDDEN"
	case errors.Is(err, trade.ErrOnchainUnavailable):
		return "ONCHAIN_UNAVAILABLE"
	case errors.Is(err, trade.ErrNotBuyer):
		return "FORBIDDEN"
	case errors.Is(err, trade.ErrNotOnchain):
		return "NOT_ONCHAIN"
	case errors.Is(err, trade.ErrSettlementLive):
		return "SETTLEMENT_IN_PROGRESS"
	case errors.Is(err, trade.ErrSettlementPending):
		return "SETTLEMENT_PENDING"
	case errors.Is(err, trade.ErrSettlementFailed):
		return "SETTLEMENT_FAILED"
	case errors.Is(err, trade.ErrInvalidSignature):
		return "INVALID_SIGNATURE"
	case errors.Is(err, settlement.ErrMismatch):
		return "SETTLEMENT_MISMATCH"
	case errors.Is(err, registry.ErrAmountTooPrecise):
		return "AMOUNT_TOO_PRECISE"
	case errors.Is(err, registry.ErrCurrencyNotAccepted):
		return "CURRENCY_NOT_ACCEPTED"
	case errors.Is(err, auth.ErrSessionInvalid), errors.Is(err, auth.ErrInvalidSignature):
		return "UNAUTHORIZED"
	case errors.Is(err, ledger.ErrAlreadyIssued):
		return "ALREADY_ISSUED"
	default:
		return "INVALID_REQUEST"
	}
}
