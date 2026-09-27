// Package settlement builds and verifies the single atomic Solana transaction
// that settles an on-chain trade. The service never holds keys: it hands the
// buyer an unsigned transaction, and later proves from the chain alone that
// what landed on chain is exactly what the two agents agreed to.
package settlement

import (
	"errors"
	"fmt"
	"strings"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/gagliardetto/solana-go"
)

var (
	// ErrTradeNotSettleable marks a trade that cannot move to settlement_pending.
	ErrTradeNotSettleable = errors.New("trade is not settleable")
	// ErrNoLiveRequest marks a trade with no unexpired settlement request.
	ErrNoLiveRequest = errors.New("trade has no live settlement request")
	// ErrRequestExpired marks a settlement request whose blockhash is stale.
	ErrRequestExpired = errors.New("settlement request has expired")
)

// Terms is the frozen, on-chain-relevant content of a trade. Both agents agreed
// to these values at acceptance, and verification compares the chain against
// exactly this struct.
type Terms struct {
	TradeID  string           `json:"tradeId"`
	Amount   uint64           `json:"amount"`
	Decimals int              `json:"decimals"`
	Mint     solana.PublicKey `json:"mint"`
	Buyer    solana.PublicKey `json:"buyer"`
	Seller   solana.PublicKey `json:"seller"`
	Fee      fees.Policy      `json:"fee"`
}

// NewTerms derives settlement terms from an accepted trade, resolving the mint
// through the governed token registry. Agent IDs are Solana addresses, so the
// buyer and seller wallets are the party agent IDs themselves.
func NewTerms(tr domain.Trade, registry tokens.Registry, policy fees.Policy) (Terms, error) {
	// settlement_pending is accepted because a buyer re-requesting a
	// transaction after an expired blockhash must settle on the very same terms.
	if tr.State != domain.TradeAccepted && tr.State != domain.TradeSettlementPending {
		return Terms{}, fmt.Errorf("%w: state is %s, want %s or %s", ErrTradeNotSettleable,
			tr.State, domain.TradeAccepted, domain.TradeSettlementPending)
	}
	if tr.SettlementMode != domain.SettlementOnchain {
		return Terms{}, fmt.Errorf("%w: settlement mode is %s, want %s", ErrTradeNotSettleable, tr.SettlementMode, domain.SettlementOnchain)
	}
	if tr.Amount.IsZero() {
		return Terms{}, fmt.Errorf("%w: amount is zero", ErrTradeNotSettleable)
	}
	entry, err := registry.Enabled(tr.Mint)
	if err != nil {
		return Terms{}, fmt.Errorf("trade terms: %w", err)
	}
	mint, err := solana.PublicKeyFromBase58(tr.Mint)
	if err != nil {
		return Terms{}, fmt.Errorf("trade terms: mint %q: %w", tr.Mint, err)
	}
	buyer, err := partyKey(tr.BuyerAgentID, "buyer")
	if err != nil {
		return Terms{}, err
	}
	seller, err := partyKey(tr.SellerAgentID, "seller")
	if err != nil {
		return Terms{}, err
	}
	amount, err := tr.Amount.BaseUnits(entry.Decimals)
	if err != nil {
		return Terms{}, fmt.Errorf("trade terms: amount: %w", err)
	}
	if amount == 0 {
		return Terms{}, fmt.Errorf("%w: amount rounds to zero base units", ErrTradeNotSettleable)
	}
	return Terms{
		TradeID:  tr.ID,
		Amount:   amount,
		Decimals: entry.Decimals,
		Mint:     mint,
		Buyer:    buyer,
		Seller:   seller,
		Fee:      policy,
	}, nil
}

func partyKey(agentID, role string) (solana.PublicKey, error) {
	key, err := solana.PublicKeyFromBase58(strings.TrimSpace(agentID))
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("trade terms: %s agent id %q is not a Solana address: %w", role, agentID, err)
	}
	return key, nil
}

// BuyerATA is the associated token account the transfer debits.
func (t Terms) BuyerATA() (solana.PublicKey, error) {
	return t.ata(t.Buyer)
}

// SellerATA is the associated token account the transfer credits.
func (t Terms) SellerATA() (solana.PublicKey, error) {
	return t.ata(t.Seller)
}

func (t Terms) ata(wallet solana.PublicKey) (solana.PublicKey, error) {
	address, _, err := solana.FindAssociatedTokenAddressWithProgram(wallet, t.Mint, solana.TokenProgramID)
	return address, err
}

// Memo is the trade UUID as UTF-8, the permanent link between the trade and the
// on-chain record.
func (t Terms) Memo() []byte { return []byte(t.TradeID) }
