package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/douglasdemaio/vtessera/internal/agp"
	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/douglasdemaio/vtessera/internal/auth"
	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/ledger"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/store"
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
	publicBaseURL string
	sandbox       bool
	agentCardBody map[string]any
	// cluster and genesis are the chain this deployment settles on, reported by
	// /healthz. Both are empty when settlement is unconfigured, which is the
	// correct state for the live deployment rather than a missing field.
	cluster cluster.Cluster
	genesis string
	// adminToken authorises the operator routes. Empty means they were never
	// registered, which is checked at routing rather than here so a missing token
	// cannot leave a route mounted and unguarded.
	adminToken []byte
}

type Options struct {
	Registry *registry.Service
	Trades   *trade.Service
	Auth     *auth.Service
	Ledger   *ledger.Ledger
	// Tokens is the governed mint allowlist published by GET /v1/tokens. It is
	// nil for a deployment with settlement unconfigured, in which case the route
	// reports that rather than inventing an empty governed set.
	Tokens  tokens.Registry
	Version string
	// Cluster is the chain this deployment settles against, and GenesisHash the
	// identity verified at boot. Both are reported by /healthz so a caller can
	// see which chain a tessera will refer to.
	Cluster     cluster.Cluster
	GenesisHash string
	// PublicBaseURL is the externally reachable origin, advertised in the agent
	// card as the gateway's own URL. An agent reads that field to decide where
	// to send its requests, so a placeholder here sends it nowhere. When it is
	// empty the card omits the field rather than claiming an address that is not
	// this service.
	PublicBaseURL string
	// AdminToken authorises the operator routes. Empty means those routes are not
	// registered at all, so a deployment without a token has no way to retire
	// anybody rather than a way anybody can try.
	AdminToken []byte
	// Sandbox marks a deployment where no real value moves. It is carried into
	// /healthz and /v1/tokens so an agent can tell before it commits to
	// something that cannot be unwound.
	Sandbox bool
}

// SettlementTier is the maturity label reported wherever on-chain settlement is
// advertised. It is deliberately a constant rather than configuration: the
// service does not get to declare itself out of beta.
const SettlementTier = "beta"

func New(opts Options) *Server {
	s := &Server{
		registry:      opts.Registry,
		trades:        opts.Trades,
		auth:          opts.Auth,
		agp:           agp.NewRouting(),
		ledger:        opts.Ledger,
		tokens:        opts.Tokens,
		version:       opts.Version,
		cluster:       opts.Cluster,
		genesis:       opts.GenesisHash,
		publicBaseURL: strings.TrimRight(opts.PublicBaseURL, "/"),
		sandbox:       opts.Sandbox,
		adminToken:    opts.AdminToken,
		mux:           http.NewServeMux(),
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
	// Registered only with a token configured. Absent routes 404 rather than
	// refusing, because a capability a deployment did not opt into should not be
	// advertised to anybody scanning the surface.
	if len(s.adminToken) > 0 {
		s.mux.HandleFunc("POST /v1/admin/agents/{id}/retire", s.requireAdmin(s.handleRetireAgent))
		s.mux.HandleFunc("POST /v1/admin/agents/{id}/restore", s.requireAdmin(s.handleRestoreAgent))
		s.mux.HandleFunc("GET /v1/admin/agents/{id}/retirement", s.requireAdmin(s.handleGetRetirement))
		// Running a probe makes this service send a request somewhere on an
		// operator's say-so, which is why it is behind the token rather than the
		// agent's own session: it spends the deployment's egress, and an agent
		// that could trigger it at will could use this service as a way to send
		// traffic to hosts the marketplace can reach and it cannot.
		s.mux.HandleFunc("POST /v1/admin/agents/{id}/probe", s.requireAdmin(s.handleProbeAgent))
	}
	s.mux.HandleFunc("GET /v1/agents", s.handleListAgents)
	s.mux.HandleFunc("GET /v1/agents/{id}", s.handleGetAgent)
	s.mux.HandleFunc("GET /v1/agents/{id}/attestation", s.handleGetCardAttestation)
	s.mux.HandleFunc("PUT /v1/agents/{id}/card", s.authed(s.handlePutCard))
	s.mux.HandleFunc("GET /v1/agents/{id}/offers", s.handleAgentOffers)
	s.mux.HandleFunc("GET /v1/offers", s.handleSearchOffers)
	s.mux.HandleFunc("POST /v1/agents/{id}/offers", s.authed(s.handlePublishOffer))
	s.mux.HandleFunc("GET /v1/offers/{id}", s.handleGetOffer)
	s.mux.HandleFunc("GET /v1/offers/{id}/attestation", s.handleGetOfferAttestation)
	// Reading a probe result is public, the way reading an attestation is: the
	// point of recording it is that a directory can check it without asking.
	s.mux.HandleFunc("GET /v1/agents/{id}/capabilities", s.handleGetCapabilities)
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
	s.mux.HandleFunc("GET /v1/limits", s.authed(s.handleGetLimits))
	s.mux.HandleFunc("PUT /v1/limits", s.authed(s.handlePutLimits))
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
	card := map[string]any{
		"name":               "vtessera marketplace",
		"description":        "A2A marketplace gateway: routes Intents to the cheapest policy-compliant agent and issues hash-chain anchored virtual tessera receipts.",
		"version":            s.version,
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
	// An agent reads url to decide where to send requests, so it is only
	// published when the operator has told us the real origin. The reads an
	// agent needs in order to discover what is for sale are listed explicitly:
	// the A2A skill list alone does not tell it how to browse the marketplace.
	if s.publicBaseURL != "" {
		card["url"] = s.publicBaseURL
		card["readEndpoints"] = map[string]string{
			"agents":     s.publicBaseURL + "/v1/agents",
			"offers":     s.publicBaseURL + "/v1/offers",
			"metrics":    s.publicBaseURL + "/v1/metrics",
			"tokens":     s.publicBaseURL + "/v1/tokens",
			"ledgerHead": s.publicBaseURL + "/v1/ledger/head",
			"health":     s.publicBaseURL + "/healthz",
		}
	}
	return card
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// cluster and genesis are the chain identity a caller needs before trusting
	// a tessera: a signature alone does not say which chain settled it, so a
	// deployment that settles on mainnet-beta and one that settles on devnet
	// would otherwise look identical from outside.
	body := map[string]any{
		"status":          "ok",
		"version":         s.version,
		"verificationKey": s.ledger.VerificationKey(),
		"agp": map[string]any{
			"version":   agp.Version,
			"extension": agp.ExtensionURI,
		},
	}
	if s.cluster != "" {
		body["cluster"] = string(s.cluster)
	}
	if s.genesis != "" {
		body["genesisHash"] = s.genesis
	}
	if s.cluster != "" {
		// Advertised wherever a chain is configured, on the same condition as the
		// cluster itself. An agent reading healthz has to be able to tell that
		// on-chain settlement exists here and how mature it is, without having to
		// infer it from whether a settlement attempt succeeds.
		body["settlementTier"] = SettlementTier
	}
	if s.sandbox {
		body["sandbox"] = true
	}
	writeJSON(w, http.StatusOK, body)
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

// handleGetCardAttestation reports a card's provenance.
//
// Two signatures, two questions, and the answer keeps them apart. The marketplace
// signature says this listing came from this deployment and has not changed since.
// The agent signature says the agent stands behind its own claims, and it is the
// one a reader has to look at before trusting a capability list.
//
// The verdicts are computed here rather than left to the reader: the service
// holds the card and the signatures, so it is the one place that can say whether
// they agree. A reader with all three can check them independently, and this is
// the convenience that does not require them to.
//
// A card with no agent signature is reported as unsigned rather than refused, and
// so is a card that predates attestations entirely. Every agent registered before
// this existed has one of those two, and a 404 here would read as "no such
// agent".
func (s *Server) handleGetCardAttestation(w http.ResponseWriter, r *http.Request) {
	agent, err := s.registry.Agent(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	found, hasRecord, err := s.registry.CardAttestation(r.Context(), agent.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	statement := s.registry.AttestedCard(agent.Card)
	market := map[string]any{
		// attested is separate from valid on purpose. A card that predates
		// attestations has no marketplace signature to check, and reporting that
		// as valid: false would tell a directory that this marketplace does not
		// vouch for a card it published.
		"attested": false,
		"valid":    false,
		"keyId":    s.registry.MarketplaceKeyID(),
	}
	agentSide := map[string]any{
		"attested": false,
		"valid":    false,
		"keyId":    agent.ID,
	}
	payload := map[string]any{
		"agentId":       agent.ID,
		"marketplace":   market,
		"agent":         agentSide,
		"canonicalForm": attest.CanonicalForm,
	}
	if hasRecord {
		market["attested"] = true
		// Against this marketplace's own key rather than the card's: the card names
		// the agent being described, the signature names who vouched, and checking
		// the wrong one of those against the other would refuse every genuine
		// marketplace attestation.
		market["valid"] = attest.VerifyCardAttestedBy(statement, found.Market, s.registry.MarketplaceKeyID()) == nil
		market["signature"] = found.Market
	}
	if found.Agent != nil {
		agentSide["attested"] = true
		agentSide["valid"] = attest.VerifyCard(statement, *found.Agent) == nil
		agentSide["signature"] = *found.Agent
	}
	// hasRecord false means the card predates attestations: there is no row, so
	// there is nothing to verify and nothing to claim. Reported rather than
	// refused, because a 404 here would say the agent does not exist.
	payload["recorded"] = hasRecord
	writeJSON(w, http.StatusOK, payload)
}

// handleGetOfferAttestation reports whether a stored offer signature verifies
// against the offer as it currently stands.
//
// The interesting case is a signature that is present and no longer valid, which
// means the offer changed after it was signed. That is reported as valid: false
// rather than as a missing signature, because the two mean different things to a
// buyer and only one of them is a warning.
func (s *Server) handleGetOfferAttestation(w http.ResponseWriter, r *http.Request) {
	offer, err := s.registry.Offer(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	sig, verified := s.registry.VerifyOfferAttestation(r.Context(), offer)
	payload := map[string]any{
		"offerId": offer.ID,
		"seller":  offer.AgentID,
		"signed":  sig.Value != "",
		"valid":   verified,
	}
	if sig.Value != "" {
		payload["signature"] = sig
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleGetCapabilities returns an agent's last recorded capability probe.
//
// An agent that has never been probed is a 404 with a different code from an agent
// that does not exist, because they are different facts and a directory reading
// this is deciding whether a capability list has been checked. Absence of a probe
// is not a failure and is not reported as one.
func (s *Server) handleGetCapabilities(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	report, found, err := s.registry.ProbeResult(r.Context(), agentID)
	if err != nil {
		writeError(w, err)
		return
	}
	if !found {
		if _, err := s.registry.Agent(r.Context(), agentID); err != nil {
			writeError(w, err)
			return
		}
		writeErrorStatus(w, http.StatusNotFound, "NOT_PROBED",
			"this agent has never been probed")
		return
	}
	valid := s.registry.VerifyProbe(report) == nil
	payload := map[string]any{
		"agentId":   report.AgentID,
		"target":    report.Target,
		"results":   report.Results,
		"passed":    report.Passed(),
		"valid":     valid,
		"checkedAt": report.CheckedAt,
	}
	if report.Signature != nil {
		payload["signature"] = *report.Signature
		payload["attestedBy"] = report.Signature.KeyID
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleProbeAgent runs a capability probe against one agent's declared endpoint.
//
// It answers 200 with the report whether or not the probe passed, because a probe
// that ran and failed is the most useful answer this route can give. A probe that
// could not run at all is an error, and the error says why: no declared target,
// no longer active, or a target that is not permitted.
func (s *Server) handleProbeAgent(w http.ResponseWriter, r *http.Request) {
	report, err := s.registry.ProbeCapabilities(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	valid := s.registry.VerifyProbe(report) == nil
	payload := map[string]any{
		"agentId":   report.AgentID,
		"target":    report.Target,
		"results":   report.Results,
		"passed":    report.Passed(),
		"valid":     valid,
		"checkedAt": report.CheckedAt,
	}
	if report.Signature != nil {
		payload["signature"] = *report.Signature
		payload["attestedBy"] = report.Signature.KeyID
	}
	writeJSON(w, http.StatusOK, payload)
}

// requireOwnAgent refuses a write that names an agent in the path other than the
// one that authenticated.
//
// An agent ID is an Ed25519 public key and a session proves control of it, so an
// agent cannot authenticate as somebody else. It could still name somebody else
// in the path, and on the two routes that write to a named agent that was enough
// to rewrite the card an agent is listed under, including the URL it is listed
// with, and to publish offers that appear in searches as that agent's.
//
// The path value is not ignored and the write is not redirected to the caller.
// Both would leave a caller believing it had changed something it had not, which
// on a card is worse than a refusal: the listing would change and the agent would
// never know.
func requireOwnAgent(w http.ResponseWriter, r *http.Request) (string, bool) {
	caller := agentFrom(r)
	if named := r.PathValue("id"); named != caller {
		writeErrorStatus(w, http.StatusForbidden, "FORBIDDEN",
			"this session is "+caller+" and cannot write to "+named)
		return "", false
	}
	return caller, true
}

func (s *Server) handlePutCard(w http.ResponseWriter, r *http.Request) {
	agentID, ok := requireOwnAgent(w, r)
	if !ok {
		return
	}
	card, sig, ok := decodePutCard(w, r)
	if !ok {
		return
	}
	// The session, not the path, decides who is being written. requireOwnAgent
	// has already refused a mismatch, so these are the same value; passing the
	// session means a future caller that forgets the check writes to itself
	// rather than to somebody else.
	agent, _, err := s.registry.Register(r.Context(), agentID, card, sig)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// decodePutCard reads a card and an optional signature from either body shape:
// wrapped under "card", or the bare card every agent already sends.
//
// A wrapped body is read into both fields at once so a signature and the card it
// covers are validated as a pair. A bare body is decoded straight into a card,
// which leaves the signature nil.
func decodePutCard(w http.ResponseWriter, r *http.Request) (domain.AgentCard, *attest.Signature, bool) {
	body, ok := readBody(w, r)
	if !ok {
		return domain.AgentCard{}, nil, false
	}
	var envelope cardEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body: "+err.Error())
		return domain.AgentCard{}, nil, false
	}
	if envelope.Card == nil {
		var card domain.AgentCard
		if err := json.Unmarshal(body, &card); err != nil {
			writeErrorStatus(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body: "+err.Error())
			return domain.AgentCard{}, nil, false
		}
		return card, nil, true
	}
	var req putCardRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body: "+err.Error())
		return domain.AgentCard{}, nil, false
	}
	return req.Card, req.Attestation, true
}

// readBody reads a body under the same size limit every other route applies.
//
// Reading by hand rather than through a decoder is a cost of accepting two
// shapes, and the limit is what keeps that cost from becoming a way to make the
// service allocate without bound.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeErrorStatus(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is too large")
		return nil, false
	}
	return body, true
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

// putCardRequest carries a card and the agent's signature over it.
//
// It is one of two accepted body shapes, and the other is the bare card, because
// every agent written before attestations existed sends a bare card and the smoke
// script is one of them. Requiring the wrapper would turn the addition of an
// optional field into a 400 for the entire existing population, which is not a
// break anyone was warned about in advance.
//
// A bare card carries no signature, which is exactly what it says: an agent that
// does not sign is not a broken agent, it is an agent whose claims nobody but the
// marketplace has vouched for. handlePutCard decides which shape arrived.
type putCardRequest struct {
	Card        domain.AgentCard  `json:"card"`
	Attestation *attest.Signature `json:"attestation"`
}

// cardEnvelope tells a wrapped body from a bare one without guessing from field
// contents.
//
// The discriminator is whether "card" is present as a key, not whether some field
// happens to be non-empty. A bare card always carries a name, so key presence is
// the only signal that cannot be produced by a card with an empty description and
// no capabilities.
type cardEnvelope struct {
	Card *json.RawMessage `json:"card"`
}

type publishOfferRequest struct {
	Direction       string   `json:"direction"`
	Description     string   `json:"description"`
	Capabilities    []string `json:"capabilities"`
	PriceAmount     string   `json:"priceAmount"`
	PriceMint       string   `json:"priceMint"`
	SettlementModes []string `json:"settlementModes"`
	IdempotencyKey  string   `json:"idempotencyKey"`
	// OfferID lets the seller name the offer it is signing. A signature covers
	// the offer ID, so a seller that wants to publish signed terms has to know
	// the ID before the offer exists. Omit it and the service assigns one, as it
	// always has.
	OfferID string `json:"offerId"`
	// Attestation is the seller's detached signature over the offer's terms. It is
	// verified against the stored content before the offer is saved.
	Attestation *attest.Signature `json:"attestation"`
}

func (s *Server) handlePublishOffer(w http.ResponseWriter, r *http.Request) {
	agentID, ok := requireOwnAgent(w, r)
	if !ok {
		return
	}
	var req publishOfferRequest
	if !decode(w, r, &req) {
		return
	}
	offer, created, err := s.registry.PublishOffer(r.Context(), agentID, registry.NewOffer{
		Direction:       domain.OfferDirection(req.Direction),
		Description:     req.Description,
		Capabilities:    req.Capabilities,
		PriceAmount:     req.PriceAmount,
		PriceMint:       req.PriceMint,
		SettlementModes: toModes(req.SettlementModes),
	}, req.IdempotencyKey, req.OfferID, req.Attestation)
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

// requireAdmin authorises an operator action on somebody else's registration.
//
// It compares in constant time and it does not fall back to the agent session:
// an agent that could authenticate here could retire every other agent on the
// marketplace, which is the capability this gate exists to withhold. The token is
// a separate credential precisely so that the two cannot be confused.
func (s *Server) requireAdmin(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.adminToken) == 0 {
			// Unreachable through the mux, because the routes are not registered
			// without a token. Checked anyway so that a future caller wiring one of
			// these handlers up by hand cannot create an open route.
			writeErrorStatus(w, http.StatusNotFound, "NOT_FOUND", "no such route")
			return
		}
		presented, err := adminTokenFrom(r.Header.Get("Authorization"))
		if err != nil || subtle.ConstantTimeCompare(presented, s.adminToken) != 1 {
			writeErrorStatus(w, http.StatusUnauthorized, "UNAUTHORIZED", "a valid operator token is required")
			return
		}
		next(w, r)
	}
}

func adminTokenFrom(header string) ([]byte, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return nil, errors.New("admin routes take a bearer token, not an agent session")
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if token == "" {
		return nil, errors.New("empty admin token")
	}
	return []byte(token), nil
}

func (s *Server) handleRetireAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	agent, err := s.registry.Retire(r.Context(), id, body.Reason, adminActor(r))
	if err != nil {
		// A refusal while trades are live names them. The operator holding this
		// refusal is the one who can clear it, and a count alone does not tell them
		// which trades to settle, dispute or expire.
		var live *store.LiveTradesError
		if errors.As(err, &live) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":        err.Error(),
				"code":         "AGENT_HAS_LIVE_TRADES",
				"agentId":      live.AgentID,
				"liveTradeIds": live.TradeIDs,
			})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleRestoreAgent(w http.ResponseWriter, r *http.Request) {
	agent, err := s.registry.Restore(r.Context(), r.PathValue("id"), adminActor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleGetRetirement(w http.ResponseWriter, r *http.Request) {
	retirement, found, err := s.registry.Retirement(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if !found {
		writeError(w, fmt.Errorf("%w: %s", domain.ErrNotFound, r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, retirement)
}

// adminActor names who performed an operator action in the audit row. It is the
// operator's own label for the deployment rather than an agent id, because
// nobody is accountable as an Ed25519 key for a withdrawal an agent did not ask
// for.
func adminActor(r *http.Request) string {
	if actor := strings.TrimSpace(r.Header.Get("X-Operator")); actor != "" {
		return actor
	}
	return "operator"
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
	code   string
}{
	{domain.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
	{domain.ErrConflict, http.StatusConflict, "CONFLICT"},
	{domain.ErrStale, http.StatusConflict, "CONFLICT"},
	{domain.ErrInvalid, http.StatusBadRequest, "INVALID_REQUEST"},
	{registry.ErrNotOwner, http.StatusForbidden, "FORBIDDEN"},
	{registry.ErrAgentSuspended, http.StatusForbidden, "INVALID_REQUEST"},
	{registry.ErrCurrencyNotAccepted, http.StatusBadRequest, "CURRENCY_NOT_ACCEPTED"},
	{registry.ErrAmountTooPrecise, http.StatusBadRequest, "AMOUNT_TOO_PRECISE"},
	{registry.ErrOfferClosed, http.StatusConflict, "INVALID_REQUEST"},
	{trade.ErrNotParty, http.StatusForbidden, "FORBIDDEN"},
	{trade.ErrIllegalState, http.StatusConflict, "ILLEGAL_STATE"},
	{trade.ErrOfferUnavailable, http.StatusConflict, "INVALID_REQUEST"},
	{trade.ErrModeNotAccepted, http.StatusBadRequest, "INVALID_REQUEST"},
	{trade.ErrAgentUnavailable, http.StatusConflict, "INVALID_REQUEST"},
	// Two refusals that share a code but not a meaning. An unconfigured
	// deployment is a 501 because the feature is not switched on there and
	// retrying will not change that; an unreachable chain is a 503 with retry
	// semantics. Merging them would tell an operator to fix their configuration
	// during a network outage.
	{trade.ErrSettlementUnconfigured, http.StatusNotImplemented, "ONCHAIN_UNAVAILABLE"},
	{trade.ErrOnchainUnavailable, http.StatusServiceUnavailable, "ONCHAIN_UNAVAILABLE"},
	// A 409 rather than a 400: the mint was the seller's choice, the buyer
	// cannot correct it, and the fix is a governance or configuration decision.
	{trade.ErrMintUngoverned, http.StatusConflict, "MINT_UNGOVERNED"},
	{trade.ErrClusterMismatch, http.StatusConflict, "CLUSTER_MISMATCH"},
	{trade.ErrNotBuyer, http.StatusForbidden, "FORBIDDEN"},
	{trade.ErrNotOnchain, http.StatusConflict, "NOT_ONCHAIN"},
	{trade.ErrSettlementLive, http.StatusConflict, "SETTLEMENT_IN_PROGRESS"},
	{trade.ErrSettlementFailed, http.StatusConflict, "SETTLEMENT_FAILED"},
	{trade.ErrSettlementPending, http.StatusAccepted, "SETTLEMENT_PENDING"},
	{trade.ErrInvalidSignature, http.StatusBadRequest, "INVALID_SIGNATURE"},
	{settlement.ErrMismatch, http.StatusConflict, "SETTLEMENT_MISMATCH"},
	{settlement.ErrMintUnverified, http.StatusConflict, "MINT_UNGOVERNED"},
	{settlement.ErrMintUnreachable, http.StatusServiceUnavailable, "ONCHAIN_UNAVAILABLE"},
	{auth.ErrSessionInvalid, http.StatusUnauthorized, "UNAUTHORIZED"},
	{auth.ErrInvalidSignature, http.StatusUnauthorized, "UNAUTHORIZED"},
	{auth.ErrSecretTooShort, http.StatusInternalServerError, "INVALID_REQUEST"},
	{agp.ErrRouteNotFound, http.StatusNotFound, "INVALID_REQUEST"},
	{agp.ErrPolicyViolation, http.StatusUnprocessableEntity, "INVALID_REQUEST"},
	{agp.ErrTableStale, http.StatusConflict, "INVALID_REQUEST"},
	{trade.ErrSpendCapExceeded, http.StatusConflict, "SPEND_CAP_EXCEEDED"},
	// 409, and not 400: the request is well formed and the deadline is a fact
	// about the trade, so retrying unchanged will keep failing until time passes.
	{trade.ErrNotExpiredYet, http.StatusConflict, "TRADE_NOT_EXPIRED"},
	{trade.ErrMintUnpriced, http.StatusConflict, "MINT_UNPRICED"},
	{registry.ErrOfferAttestationRequired, http.StatusConflict, "OFFER_ATTESTATION_REQUIRED"},
	{registry.ErrNoProbeTarget, http.StatusConflict, "NO_PROBE_TARGET"},
	{registry.ErrMintUnpriced, http.StatusConflict, "MINT_UNPRICED"},
	{registry.ErrAgentHasLiveTrades, http.StatusConflict, "AGENT_HAS_LIVE_TRADES"},
	{registry.ErrRetirementReasonRequired, http.StatusBadRequest, "REASON_REQUIRED"},
	{registry.ErrAttestationRefused, http.StatusBadRequest, "ATTESTATION_REFUSED"},
	{ledger.ErrNotSettled, http.StatusConflict, "INVALID_REQUEST"},
	{ledger.ErrAlreadyIssued, http.StatusConflict, "ALREADY_ISSUED"},
}

func writeError(w http.ResponseWriter, err error) {
	status, code, matched := http.StatusInternalServerError, "", false
	for _, candidate := range statusByError {
		if errors.Is(err, candidate.err) {
			status, code, matched = candidate.status, candidate.code, true
			break
		}
	}
	if agpCode, ok := agp.CodeOf(err); ok {
		writeJSON(w, status, rpcError(nil, agpCode, err.Error()))
		return
	}
	if !matched {
		writeErrorStatus(w, status, "INTERNAL", err.Error())
		return
	}
	writeErrorStatus(w, status, code, err.Error())
}

func writeErrorStatus(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": message, "code": code})
}
