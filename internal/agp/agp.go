package agp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/douglasdemaio/vtessera/internal/money"
)

const (
	ExtensionURI = "https://github.com/a2aproject/a2a-samples/tree/main/extensions/agp"
	Version      = "1.0"
	GatewayRole  = "gateway"
	MaxRoutes    = 128
)

var SupportedVersions = []string{"1.0"}

const (
	CodeRouteNotFound   = -32200
	CodePolicyViolation = -32201
	CodeTableStale      = -32202
)

const (
	PolicySettlementModes = "settlement_modes"
	PolicyRequiresPII     = "requires_pii"
	PolicySecurityLevel   = "security_level"
	PolicyCurrencies      = "currencies"
	PolicyDirection       = "direction"
	PolicyCost            = "cost"
	PolicyMint            = "mint"
)

const CapabilitySeparator = ":"

var (
	ErrRouteNotFound   = &Error{Code: CodeRouteNotFound, Name: "AGP_ROUTE_NOT_FOUND", Message: "no agent or squad has announced the requested target_capability"}
	ErrPolicyViolation = &Error{Code: CodePolicyViolation, Name: "AGP_POLICY_VIOLATION", Message: "routes were found but none satisfy the intent policy_constraints"}
	ErrTableStale      = &Error{Code: CodeTableStale, Name: "AGP_TABLE_STALE", Message: "the routing table is stale and cannot serve this intent"}
	ErrInvalidIntent   = errors.New("intent is invalid")
)

type Error struct {
	Code    int
	Name    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s (%d): %s", e.Name, e.Code, e.Message)
}

func CodeOf(err error) (int, bool) {
	var agpErr *Error
	if errors.As(err, &agpErr) {
		return agpErr.Code, true
	}
	return 0, false
}

func IsRouteNotFound(err error) bool {
	code, ok := CodeOf(err)
	return ok && code == CodeRouteNotFound
}

func IsPolicyViolation(err error) bool {
	code, ok := CodeOf(err)
	return ok && code == CodePolicyViolation
}

func NormalizeCapability(capability string) string {
	return strings.ToLower(strings.TrimSpace(capability))
}

func ValidateCapability(capability string) error {
	normalized := NormalizeCapability(capability)
	if normalized == "" {
		return fmt.Errorf("%w: capability is required", ErrInvalidIntent)
	}
	if len(capability) > 200 {
		return fmt.Errorf("%w: capability exceeds 200 characters", ErrInvalidIntent)
	}
	parts := strings.Split(normalized, CapabilitySeparator)
	if len(parts) < 2 {
		return fmt.Errorf("%w: capability %q must be domain:action", ErrInvalidIntent, capability)
	}
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("%w: capability %q has an empty segment", ErrInvalidIntent, capability)
		}
	}
	return nil
}

type Intent struct {
	TargetCapability  string         `json:"target_capability"`
	Payload           map[string]any `json:"payload"`
	PolicyConstraints map[string]any `json:"policy_constraints,omitempty"`
}

func (i Intent) Validate() error {
	if err := ValidateCapability(i.TargetCapability); err != nil {
		return err
	}
	if i.Payload == nil {
		return fmt.Errorf("%w: payload is required", ErrInvalidIntent)
	}
	return nil
}

type RouteEntry struct {
	Path         string                  `json:"path"`
	Cost         float64                 `json:"cost"`
	Policy       map[string]any          `json:"policy"`
	CostAmount   string                  `json:"cost_amount"`
	CostMint     string                  `json:"cost_mint"`
	OfferID      string                  `json:"offer_id,omitempty"`
	AgentID      string                  `json:"agent_id,omitempty"`
	Direction    string                  `json:"direction,omitempty"`
	Announcement *CapabilityAnnouncement `json:"announcement,omitempty"`

	parsedCost    money.Amount
	hasParsedCost bool
}

type RouteResult struct {
	TargetCapability string         `json:"target_capability"`
	Route            RouteEntry     `json:"route"`
	Considered       int            `json:"considered"`
	Rejected         map[string]int `json:"rejected,omitempty"`
	TableFingerprint string         `json:"table_fingerprint"`
	TableAsOf        string         `json:"table_as_of,omitempty"`
}

type Table struct {
	Entries       map[string][]RouteEntry  `json:"entries"`
	AsOf          string                   `json:"as_of,omitempty"`
	Announcements []CapabilityAnnouncement `json:"announcements"`
}

type TableSource interface {
	Announcements(ctx context.Context) ([]CapabilityAnnouncement, error)
}

type Routing struct{}

func NewRouting() *Routing {
	return &Routing{}
}

func (r *Routing) BuildTable(ctx context.Context, src TableSource) (Table, error) {
	announcements, err := src.Announcements(ctx)
	if err != nil {
		return Table{}, err
	}
	if len(announcements) > MaxRoutes {
		return Table{}, fmt.Errorf("%w: %d announcements exceed the table limit of %d", ErrInvalidIntent, len(announcements), MaxRoutes)
	}
	table := Table{Entries: map[string][]RouteEntry{}, Announcements: announcements}
	for i := range announcements {
		entry, err := EntryFor(announcements[i])
		if err != nil {
			return Table{}, fmt.Errorf("announcement %s: %w", announcements[i].SquadPath, err)
		}
		capability := NormalizeCapability(announcements[i].Capability)
		table.Entries[capability] = append(table.Entries[capability], entry)
	}
	table.sortEntries()
	table.AsOf = latestAnnouncement(announcements)
	return table, nil
}

func (t Table) sortEntries() {
	for capability, entries := range t.Entries {
		sort.SliceStable(entries, func(i, j int) bool { return lessCost(entries[i], entries[j]) })
		t.Entries[capability] = entries
	}
}

func (t Table) Capabilities() []string {
	out := make([]string, 0, len(t.Entries))
	for capability := range t.Entries {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

func (t Table) Fingerprint() string {
	parts := make([]string, 0)
	for _, capability := range t.Capabilities() {
		for _, entry := range t.Entries[capability] {
			parts = append(parts, strings.Join([]string{capability, entry.Path, entry.CostAmount, entry.CostMint}, "|"))
		}
	}
	return "sha256:" + sha256Hex(strings.Join(parts, "\n"))
}

func (t Table) Route(intent Intent) (RouteResult, error) {
	if err := intent.Validate(); err != nil {
		return RouteResult{}, err
	}
	capability := NormalizeCapability(intent.TargetCapability)
	candidates, ok := t.Entries[capability]
	if !ok || len(candidates) == 0 {
		return RouteResult{}, fmt.Errorf("%w: %s", ErrRouteNotFound, capability)
	}
	compliantRoutes := make([]RouteEntry, 0, len(candidates))
	rejected := map[string]int{}
	for _, entry := range candidates {
		if err := CheckPolicy(entry.Policy, intent.PolicyConstraints); err != nil {
			if entry.OfferID != "" {
				rejected[entry.OfferID]++
			}
			continue
		}
		compliantRoutes = append(compliantRoutes, entry)
	}
	if len(compliantRoutes) == 0 {
		return RouteResult{}, fmt.Errorf("%w: %s matched %d route(s), none satisfied the constraints", ErrPolicyViolation, capability, len(candidates))
	}
	best := compliantRoutes[0]
	for _, entry := range compliantRoutes[1:] {
		if lessCost(entry, best) {
			best = entry
		}
	}
	result := RouteResult{
		TargetCapability: capability,
		Route:            best,
		Considered:       len(candidates),
		TableFingerprint: t.Fingerprint(),
		TableAsOf:        t.AsOf,
	}
	if len(rejected) > 0 {
		result.Rejected = rejected
	}
	return result, nil
}

func lessCost(a, b RouteEntry) bool {
	if a.hasParsedCost && b.hasParsedCost {
		if cmp := a.parsedCost.Cmp(b.parsedCost); cmp != 0 {
			return cmp < 0
		}
		return a.Path < b.Path
	}
	if a.Cost != b.Cost {
		return a.Cost < b.Cost
	}
	return a.Path < b.Path
}

func CheckPolicy(announced, constraints map[string]any) error {
	for _, key := range sortedKeys(constraints) {
		want := constraints[key]
		have, ok := announced[key]
		if !ok {
			return fmt.Errorf("route does not announce policy %q", key)
		}
		if err := matchPolicy(key, want, have); err != nil {
			return err
		}
	}
	return nil
}

func matchPolicy(key string, want, have any) error {
	switch key {
	case PolicyRequiresPII:
		wantBool, err := toBool(want)
		if err != nil {
			return err
		}
		haveBool, err := toBool(have)
		if err != nil {
			return err
		}
		if wantBool && !haveBool {
			return fmt.Errorf("policy %q must be true, route announces %v", key, haveBool)
		}
		return nil
	case PolicySecurityLevel:
		wantNum, err := toNumber(want)
		if err != nil {
			return err
		}
		haveNum, err := toNumber(have)
		if err != nil {
			return err
		}
		if haveNum < wantNum {
			return fmt.Errorf("policy %q requires at least %v, route announces %v", key, wantNum, haveNum)
		}
		return nil
	case PolicySettlementModes, PolicyCurrencies:
		return matchAnyOf(key, want, have)
	default:
		if fmt.Sprint(normalizeValue(want)) != fmt.Sprint(normalizeValue(have)) {
			return fmt.Errorf("policy %q requires %v, route announces %v", key, normalizeValue(want), normalizeValue(have))
		}
		return nil
	}
}

func matchAnyOf(key string, want, have any) error {
	wants, err := toStringList(want)
	if err != nil {
		return err
	}
	haves, err := toStringList(have)
	if err != nil {
		return err
	}
	haveSet := map[string]bool{}
	for _, h := range haves {
		haveSet[strings.ToLower(h)] = true
	}
	for _, w := range wants {
		if haveSet[strings.ToLower(w)] {
			return nil
		}
	}
	return fmt.Errorf("policy %q requires one of %v, route offers %v", key, wants, haves)
}

func toBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		b, err := strconv.ParseBool(t)
		if err != nil {
			return false, fmt.Errorf("policy value %q is not a boolean", t)
		}
		return b, nil
	default:
		return false, fmt.Errorf("policy value %v is not a boolean", v)
	}
}

func toNumber(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, fmt.Errorf("policy value %q is not a number", t.String())
		}
		return f, nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0, fmt.Errorf("policy value %q is not a number", t)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("policy value %v is not a number", v)
	}
}

func toStringList(v any) ([]string, error) {
	switch t := v.(type) {
	case []string:
		return t, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, fmt.Sprint(normalizeValue(item)))
		}
		return out, nil
	case string:
		return []string{t}, nil
	default:
		return nil, fmt.Errorf("policy value %v is not a list", v)
	}
}

func normalizeValue(v any) any {
	if number, ok := v.(json.Number); ok {
		if f, err := number.Float64(); err == nil {
			return f
		}
		return number.String()
	}
	return v
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
