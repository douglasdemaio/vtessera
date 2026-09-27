package settlement

import (
	"errors"
	"fmt"

	"github.com/gagliardetto/solana-go"
)

// ErrMismatch means the on-chain transaction is not exactly the settlement the
// two agents agreed to. A mismatch is never a soft failure: the trade goes to
// disputed and no tessera is issued.
var ErrMismatch = errors.New("settlement transaction does not match the agreed terms")

// Verifier proves from a transaction alone that it settles the terms. The fee
// policy it enforces travels with the terms, so a verifier holds no state and
// a policy change can never be silently out of step with verification.
type Verifier struct{}

// NewVerifier returns a verifier.
func NewVerifier() *Verifier { return &Verifier{} }

// Verify checks a submitted transaction against the agreed terms, exactly and in
// canonical order: the token transfer of terms.amount of terms.mint between the
// derived ATAs, the trade UUID memo, and the fee transfer of the exact policy
// amount to the exact service wallet, with the buyer as fee payer and signer.
//
// The seller's associated token account may legitimately appear as a leading
// CreateAssociatedTokenAccount instruction; nothing else may.
func (v *Verifier) Verify(t Terms, tx *solana.Transaction) error {
	if tx == nil {
		return fmt.Errorf("%w: no transaction", ErrMismatch)
	}
	if err := v.checkPayer(t, tx); err != nil {
		return err
	}
	actual, err := compile(tx.Message)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMismatch, err)
	}
	withoutATA, err := expectedCanonical(t, true)
	if err != nil {
		return err
	}
	withATA, err := expectedCanonical(t, false)
	if err != nil {
		return err
	}
	switch {
	case sameInstructions(actual, withATA):
		return nil
	case sameInstructions(actual, withoutATA):
		return nil
	default:
		return mismatchError(actual, withATA, withoutATA)
	}
}

// checkPayer enforces that the buyer is the fee payer and a required signer, so
// no third party can submit a settlement on the buyer's behalf.
func (v *Verifier) checkPayer(t Terms, tx *solana.Transaction) error {
	keys := tx.Message.AccountKeys
	if len(keys) == 0 {
		return fmt.Errorf("%w: message has no accounts", ErrMismatch)
	}
	if !keys[0].Equals(t.Buyer) {
		return fmt.Errorf("%w: fee payer is %s, want the buyer %s", ErrMismatch, keys[0], t.Buyer)
	}
	numSigners := int(tx.Message.Header.NumRequiredSignatures)
	if numSigners != 1 {
		return fmt.Errorf("%w: transaction requires %d signatures, want exactly the buyer", ErrMismatch, numSigners)
	}
	// The chain already accepted the transaction, but confirm the signature is
	// genuinely the buyer's over the exact message bytes.
	if err := tx.VerifySignatures(); err != nil {
		return fmt.Errorf("%w: buyer signature is not valid: %v", ErrMismatch, err)
	}
	return nil
}

// mismatchError reports the first instruction that differs, naming both
// candidates so an operator can see exactly what was tampered with.
func mismatchError(actual, withATA, withoutATA []instruction) error {
	best := withoutATA
	if len(actual) == len(withATA) {
		best = withATA
	}
	for i := 0; i < len(actual) && i < len(best); i++ {
		if !actual[i].equals(best[i]) {
			return fmt.Errorf("%w: instruction %d is %s, want %s", ErrMismatch, i, actual[i], best[i])
		}
	}
	switch {
	case len(actual) < len(best):
		return fmt.Errorf("%w: transaction has %d instructions, want %d: %v", ErrMismatch, len(actual), len(best), describe(actual))
	case len(actual) > len(best):
		return fmt.Errorf("%w: transaction has %d instructions, want %d: %v", ErrMismatch, len(actual), len(best), describe(actual))
	default:
		return fmt.Errorf("%w: %v", ErrMismatch, describe(actual))
	}
}

func describe(instructions []instruction) []string {
	out := make([]string, 0, len(instructions))
	for i, ix := range instructions {
		out = append(out, fmt.Sprintf("[%d] %s", i, ix))
	}
	return out
}
