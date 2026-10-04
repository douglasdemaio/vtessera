package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/douglasdemaio/vtessera/internal/limits"
	"github.com/douglasdemaio/vtessera/internal/money"
)

// limitsPayload is what an agent is told about its own caps. It reports the caps
// in force and the ceilings above them, so an agent can find out what it would
// have to ask for rather than discovering the ceiling by being refused.
func limitsPayload(effective limits.Limits, ceilingPerTrade, ceilingPerDay money.Amount) map[string]any {
	out := map[string]any{
		"perTradeUsd": effective.PerTrade,
		"perDayUsd":   effective.PerDay,
		"raised":      effective.Raised,
		"currency":    "USD",
	}
	if !ceilingPerTrade.IsZero() {
		out["maxPerTradeUsd"] = ceilingPerTrade
	}
	if !ceilingPerDay.IsZero() {
		out["maxPerDayUsd"] = ceilingPerDay
	}
	return out
}

func (s *Server) handleGetLimits(w http.ResponseWriter, r *http.Request) {
	policy, ok := s.trades.Limits()
	if !ok {
		writeErrorStatus(w, http.StatusNotImplemented, "LIMITS_UNCONFIGURED",
			"spending caps are not configured on this deployment")
		return
	}
	agentID := agentFrom(r)
	effective, err := s.trades.EffectiveLimits(r.Context(), agentID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, limitsPayload(effective, policy.CeilingPerTrade, policy.CeilingPerDay))
}

type putLimitsRequest struct {
	PerTradeUSD *string `json:"perTradeUsd"`
	PerDayUSD   *string `json:"perDayUsd"`
}

// handlePutLimits records an agent's opt-in to caps above the deployment
// default. The agent it applies to is the one that authenticated, never a path
// value: there is no path value on this route, so an agent cannot raise
// somebody else's cap by naming them.
func (s *Server) handlePutLimits(w http.ResponseWriter, r *http.Request) {
	policy, ok := s.trades.Limits()
	if !ok {
		writeErrorStatus(w, http.StatusNotImplemented, "LIMITS_UNCONFIGURED",
			"spending caps are not configured on this deployment")
		return
	}
	var in putLimitsRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	// An absent field means "leave this cap as it is", so a caller raising only
	// the daily figure does not silently drop its per-trade cap to the default.
	effective, err := s.trades.EffectiveLimits(r.Context(), agentFrom(r))
	if err != nil {
		writeError(w, err)
		return
	}
	perTrade, perDay := effective.PerTrade, effective.PerDay
	if in.PerTradeUSD != nil {
		if perTrade, err = money.Parse(*in.PerTradeUSD); err != nil {
			writeErrorStatus(w, http.StatusBadRequest, "INVALID_REQUEST",
				"perTradeUsd must be a decimal amount")
			return
		}
	}
	if in.PerDayUSD != nil {
		if perDay, err = money.Parse(*in.PerDayUSD); err != nil {
			writeErrorStatus(w, http.StatusBadRequest, "INVALID_REQUEST",
				"perDayUsd must be a decimal amount")
			return
		}
	}
	if err := policy.Check(perTrade, perDay); err != nil {
		// Refused rather than clamped. Clamping would tell an agent its cap was
		// raised when it was not, and it would go on to spend against a number
		// it chose.
		if errors.Is(err, limits.ErrAboveCeiling) {
			writeErrorStatus(w, http.StatusConflict, "CAP_ABOVE_CEILING", err.Error())
			return
		}
		writeErrorStatus(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	raised, err := s.trades.RaiseLimits(r.Context(), agentFrom(r), perTrade, perDay)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, limitsPayload(raised, policy.CeilingPerTrade, policy.CeilingPerDay))
}

// decodeStrict reads one JSON body and refuses fields this service does not know.
//
// It is stricter than the shared decode on purpose, and only here. Everywhere else
// an unrecognised field is harmless. On a route whose whole purpose is to set a
// money limit, "perTradeAmount" sent instead of "perTradeUsd" would otherwise
// return 200 and change nothing, and an agent that trusted the success would go on
// trading under a cap it did not choose.
//
// A field differing only in case is not this: Go matches JSON keys to fields
// case-insensitively, so "perTradeUsD" is the same field and takes effect. That
// is a caller and this service disagreeing about spelling rather than about
// intent, and the value sent is the value applied.
//
// A second document in the same body is refused. The caller and the service would
// disagree about which half was meant, and the first document would be the one
// that took effect.
func decodeStrict(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErrorStatus(w, http.StatusBadRequest, "INVALID_REQUEST", "request body must hold exactly one JSON object")
		return false
	}
	return true
}
