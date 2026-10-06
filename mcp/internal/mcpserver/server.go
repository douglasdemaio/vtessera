// Package mcpserver exposes vtessera's public API as MCP tools.
//
// The tool set is read-only on purpose. Every tool reaches an endpoint an outside
// agent can reach unauthenticated. Nothing here can register an agent, publish an
// offer, accept a trade or run a probe, because the marketplace requires a session
// for those and this server has none: a directory or a model asking "is this agent
// real" is answered from the same signed records a buyer would check.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/douglasdemaio/vtessera/mcp/internal/vtessera"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server is the MCP server together with the instructions it was built with.
//
// The instructions are kept rather than discarded because they are the only place a
// connected client is told this server cannot register, publish, trade or probe. A
// client that assumed otherwise would tell a user it could buy something.
type Server struct {
	*mcp.Server
	text string
}

// Instructions is the text a connected client is shown.
func (s *Server) Instructions() string { return s.text }

// New builds the MCP server and its tool set for a marketplace.
func New(client *vtessera.Client, baseURL string) *Server {
	text := fmt.Sprintf(`vtessera is an agent marketplace at %s.

Agents offer services for a price and buyers negotiate; trades settle either
off-chain against a signed receipt the marketplace can issue, or on-chain with the
buyer signing their own transfer. This server is read-only: it can find offers,
read cards, route an intent to an agent, and report what the marketplace has
attested and probed. Registering an agent, publishing an offer, accepting a trade
and running a probe all require the agent's own session against the marketplace's
HTTP API, so none of them are here.

Two answers are worth reading carefully. A card attestation says who vouched for an
agent's card, and it is two-sided: this marketplace's signature and the agent's
own. A capability report is an observation the marketplace made, signed by the same
key, and an agent that has never been probed reports NOT_PROBED rather than a
report with nothing in it.`, baseURL)
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "vtessera",
		Title:   "vtessera agent marketplace",
		Version: Version,
	}, &mcp.ServerOptions{Instructions: text})
	register(server, client)
	return &Server{Server: server, text: text}
}

// Version is the MCP server's own version, reported to clients during initialize
// and named in the registry listing.
//
// It is a constant rather than a linker flag because a listing and a binary that
// disagree is worse than either alone: the registry would describe a build nobody
// is running.
const Version = "0.1.0"

// register wires every tool to its handler.
//
// This is the only place a tool is declared. The description here is the whole
// interface a model has for choosing between tools, so each one states its
// refusal conditions as well as its happy path: a caller that read NOT_PROBED as
// "nothing to report" would draw exactly the wrong conclusion from an agent nobody
// has checked.
//
// The handlers take typed arguments so the SDK derives and enforces the schema,
// rather than each handler unpacking a map and describing what it found missing in
// its own words.
func register(server *mcp.Server, client *vtessera.Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "vtessera_health",
		Title:       "Marketplace health and identity",
		Description: "Report whether the vtessera marketplace is answering, its verification key, and whether it is a sandbox where no real value can move.",
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		health, err := client.Health(ctx)
		if err != nil {
			return nil, nil, describeError(err)
		}
		return textResult(health), health, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "vtessera_search_offers",
		Title:       "Search published offers",
		Description: "Find open offers by capability, currency, settlement mode, direction or free text.",
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		// Every filter is optional, and omitempty is what makes it so: a field
		// without it becomes a required property, which would make a caller pass an
		// empty settlement mode and an empty direction to ask for all offers.
		Capability     string `json:"capability,omitempty" jsonschema:"capability the offer provides"`
		Mint           string `json:"mint,omitempty" jsonschema:"governed stablecoin mint the seller wants paid in"`
		SettlementMode string `json:"settlementMode,omitempty" jsonschema:"settlement the offer accepts: offchain or onchain"`
		Direction      string `json:"direction,omitempty" jsonschema:"ask or bid"`
		Query          string `json:"query,omitempty" jsonschema:"free text matched against the description"`
		Limit          int    `json:"limit,omitempty" jsonschema:"how many offers to return"`
	}) (*mcp.CallToolResult, any, error) {
		offers, err := client.SearchOffers(ctx, vtessera.OfferQuery{
			Capability: args.Capability,
			Mint:       args.Mint,
			Mode:       args.SettlementMode,
			Direction:  args.Direction,
			Text:       args.Query,
			Limit:      args.Limit,
		})
		if err != nil {
			return nil, nil, describeError(err)
		}
		// An empty result is stated as a count as well as a list. A model handed
		// an empty array has no way to tell "no offers matched" from a malformed
		// answer, and those lead to different next moves.
		out := map[string]any{"count": len(offers), "offers": offers}
		return textResult(out), out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "vtessera_get_offer",
		Title:       "Read one offer",
		Description: "Read a single offer by id, including its price and whether it is still open.",
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		OfferID string `json:"offerId" jsonschema:"the offer id"`
	}) (*mcp.CallToolResult, any, error) {
		offer, err := client.Offer(ctx, args.OfferID)
		if err != nil {
			return nil, nil, describeError(err)
		}
		return textResult(offer), offer, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "vtessera_get_agent",
		Title:       "Read an agent listing",
		Description: "Read an agent's published card and marketplace status, as the agent declared it.",
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		AgentID string `json:"agentId" jsonschema:"the agent id, which is its Ed25519 public key"`
	}) (*mcp.CallToolResult, any, error) {
		agent, err := client.Agent(ctx, args.AgentID)
		if err != nil {
			return nil, nil, describeError(err)
		}
		return textResult(agent), agent, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "vtessera_get_card_attestation",
		Title:       "Check who vouched for an agent's card",
		Description: "Report the marketplace's and the agent's own signatures over an agent's card, answered separately.",
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		AgentID string `json:"agentId" jsonschema:"the agent id"`
	}) (*mcp.CallToolResult, any, error) {
		verdict, err := client.CardAttestation(ctx, args.AgentID)
		if err != nil {
			return nil, nil, describeError(err)
		}
		out := map[string]any{
			"agentId":       verdict.AgentID,
			"canonicalForm": verdict.CanonicalForm,
			"marketplace":   sideView(verdict.Marketplace),
			"agent":         sideView(verdict.Agent),
		}
		return textResult(out), out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "vtessera_get_capability_report",
		Title:       "Read the last capability probe of an agent",
		Description: "Return the marketplace's most recent signed capability probe of an agent. Fails with NOT_PROBED when the agent has never been probed.",
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		AgentID string `json:"agentId" jsonschema:"the agent id"`
	}) (*mcp.CallToolResult, any, error) {
		report, err := client.CapabilityReport(ctx, args.AgentID)
		if err != nil {
			return nil, nil, describeError(err)
		}
		out := map[string]any{
			"agentId":    report.AgentID,
			"target":     report.Target,
			"passed":     report.Passed,
			"valid":      report.Valid,
			"checkedAt":  report.CheckedAt,
			"attestedBy": report.AttestedBy,
			"results":    report.Results,
			"signature":  report.Signature,
		}
		return textResult(out), out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "vtessera_route_intent",
		Title:       "Route an intent to an agent",
		Description: "Ask the marketplace which agent should serve a capability and what it charges. The payload is required: pass an empty object for a capability that takes no arguments.",
		Annotations: readOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		TargetCapability string `json:"targetCapability" jsonschema:"the capability to satisfy"`
		// Required, because the marketplace refuses an intent with no payload at all.
		// The empty object is a legitimate answer for a capability that takes no
		// arguments, so the tool asks for the field rather than defaulting it: a
		// caller who meant to pass arguments should see the marketplace's refusal
		// rather than have this server quietly route a different request.
		Payload           map[string]any `json:"payload" jsonschema:"arguments for the capability; use an empty object when it takes none"`
		PolicyConstraints map[string]any `json:"policyConstraints,omitempty" jsonschema:"constraints the chosen agent's announcement must satisfy"`
	}) (*mcp.CallToolResult, any, error) {
		result, err := client.Route(ctx, vtessera.RouteIntent{
			TargetCapability:  args.TargetCapability,
			Payload:           args.Payload,
			PolicyConstraints: args.PolicyConstraints,
		})
		if err != nil {
			return nil, nil, describeError(err)
		}
		return textResult(result), result, nil
	})
}

// readOnly marks a tool as making no changes.
//
// The hint is set on every tool because a client may use it to decide whether to
// call a tool without asking a person, and a wrong answer either interrupts a
// human for no reason or skips the confirmation a destructive tool needs.
func readOnly() *mcp.ToolAnnotations {
	trueValue, falseValue := true, false
	return &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &trueValue, DestructiveHint: &falseValue}
}

// sideView reduces an attestation side to the two questions a reader has. The raw
// signature is left out of the summary because it is long and the verdict already
// says whether it verifies; the full signature is available from the marketplace's
// own endpoint.
func sideView(side vtessera.Side) map[string]any {
	return map[string]any{
		"attested": side.Attested,
		"valid":    side.Valid,
		"keyId":    side.KeyID,
	}
}

// textResult renders a value as the text content an MCP client displays.
//
// The JSON is returned as text rather than only as structured content because the
// models reading this see the text, and the structured output is there for a
// program that wants to skip the parse.
func textResult(v any) *mcp.CallToolResult {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "could not render the response: " + err.Error()}},
		}
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
		StructuredContent: v,
	}
}

// describeError turns a refusal into a message a model can act on.
//
// A tool error here is usually not a bug: an unprobed agent, a missing offer and an
// unreachable marketplace are three different answers, and a caller that cannot
// tell them apart will retry the wrong thing.
func describeError(err error) error {
	if apiErr, ok := err.(*vtessera.APIError); ok {
		if code := apiErr.Code(); code != "" {
			return fmt.Errorf("%s (%s)", apiErr.Error(), code)
		}
	}
	return err
}
