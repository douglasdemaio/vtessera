package httpapi

import (
	"net/http"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
)

// settlementPayload is the buyer's view of an issued settlement request. The
// service never returns a signed transaction: the buyer signs and submits.
func settlementPayload(request domain.SettlementRequest) map[string]any {
	return map[string]any{
		"requestId":            request.ID,
		"tradeId":              request.TradeID,
		"unsignedTx":           request.UnsignedTx,
		"blockhash":            request.Blockhash,
		"lastValidBlockHeight": request.LastValid,
		"blockhashExpiresAt":   request.ExpiresAt,
		"feeLamports":          request.FeeLamports,
		"feeWallet":            request.FeeWallet,
		"buyerAta":             request.BuyerATA,
		"sellerAta":            request.SellerATA,
		"createdSellerAta":     request.CreatedATA,
	}
}

type confirmRequest struct {
	Signature string `json:"signature"`
}

// handleBuildSettlement implements design spec §6.2. The unsigned transaction is
// persisted before it is returned, so a later confirm can be matched against
// exactly what was offered.
func (s *Server) handleBuildSettlement(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	request, err := s.trades.BuildSettlement(r.Context(), agentFrom(r), id)
	if err != nil {
		writeError(w, err)
		return
	}
	tr, err := s.trades.Get(r.Context(), agentFrom(r), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"settlement": settlementPayload(request),
		"trade":      tr,
	})
}

// handleConfirmSettlement implements design spec §6.3. Verification is exact and
// canonical; a mismatch leaves the trade disputed with no tessera, and a
// signature the chain has not seen yet is reported as still pending.
func (s *Server) handleConfirmSettlement(w http.ResponseWriter, r *http.Request) {
	var req confirmRequest
	if !decode(w, r, &req) {
		return
	}
	tr, receipt, err := s.trades.ConfirmSettlement(r.Context(), agentFrom(r), r.PathValue("id"), req.Signature)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"trade":   tr,
		"receipt": receipt,
		"tessera": tesseraPayload(s.ledger, receipt.JWS),
	})
}

// handleSettlementRequest lets a buyer whose client restarted see whether the
// transaction it holds is still live.
func (s *Server) handleSettlementRequest(w http.ResponseWriter, r *http.Request) {
	request, err := s.trades.SettlementRequestFor(r.Context(), agentFrom(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	payload := settlementPayload(request)
	payload["status"] = request.Status
	payload["signature"] = request.Signature
	payload["live"] = request.Live(time.Now().UTC())
	payload["createdAt"] = request.CreatedAt
	writeJSON(w, http.StatusOK, payload)
}

// handleListTokens publishes the governed mint allowlist. Token identity is by
// address only: a symbol is decoration, never an identity.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil {
		writeErrorStatus(w, http.StatusNotImplemented, "NOT_CONFIGURED", "no token registry is configured")
		return
	}
	payload := map[string]any{"tokens": s.tokens.List()}
	// The cluster travels with the list. A mint address names an account on a
	// particular chain, so a bare address list is exactly the ambiguity this
	// work exists to remove, and a caller that read it without the cluster would
	// be back to guessing.
	if c := s.tokens.Cluster(); c != "" {
		payload["cluster"] = string(c)
	}
	if s.genesis != "" {
		payload["genesisHash"] = s.genesis
	}
	if fee, ok := s.trades.SettlementFee(); ok {
		payload["settlementFee"] = map[string]any{
			"lamports":    fee.Lamports(),
			"wallet":      fee.WalletAddress(),
			"description": fee.Description(),
			"payer":       "buyer",
		}
	}
	writeJSON(w, http.StatusOK, payload)
}
