package settlement

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/gagliardetto/solana-go"
	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/programs/memo"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
)

// account is a transaction account reduced to its canonical, comparable form.
type account struct {
	key      solana.PublicKey
	signer   bool
	writable bool
}

// instruction is a program call reduced to its canonical, comparable form.
// Build and verify share this representation, and both sides of a comparison are
// produced by the same Solana message compiler, so the transaction the service
// issues and the transaction the verifier accepts cannot drift apart.
type instruction struct {
	program  solana.PublicKey
	accounts []account
	data     []byte
}

func (i instruction) String() string {
	parts := make([]string, 0, len(i.accounts))
	for _, a := range i.accounts {
		flag := ""
		switch {
		case a.signer && a.writable:
			flag = "S"
		case a.signer:
			flag = "s"
		case a.writable:
			flag = "W"
		}
		parts = append(parts, a.key.String()+flag)
	}
	return fmt.Sprintf("%s[%s] %x", i.program, strings.Join(parts, ","), i.data)
}

func (i instruction) equals(other instruction) bool {
	if !i.program.Equals(other.program) || len(i.accounts) != len(other.accounts) {
		return false
	}
	if !bytes.Equal(i.data, other.data) {
		return false
	}
	for n := range i.accounts {
		if !i.accounts[n].key.Equals(other.accounts[n].key) {
			return false
		}
		if i.accounts[n].signer != other.accounts[n].signer || i.accounts[n].writable != other.accounts[n].writable {
			return false
		}
	}
	return true
}

func sameInstructions(a, b []instruction) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].equals(b[i]) {
			return false
		}
	}
	return true
}

// compile reduces a compiled legacy message to its canonical instructions,
// resolving account indices back to addresses and re-deriving signer and
// writable flags from the message header. Writability is a property of the
// merged account list rather than of any single instruction: an account that is
// read-only for the transfer is still writable because the fee comes out of it.
func compile(m solana.Message) ([]instruction, error) {
	if version := m.GetVersion(); version != solana.MessageVersionLegacy {
		return nil, fmt.Errorf("message version %d is not supported; settlement uses legacy messages", version)
	}
	numSigners := int(m.Header.NumRequiredSignatures)
	numReadonlySigned := int(m.Header.NumReadonlySignedAccounts)
	numReadonlyUnsigned := int(m.Header.NumReadonlyUnsignedAccounts)
	keys := m.AccountKeys

	resolve := func(index uint16) account {
		if int(index) >= len(keys) {
			return account{}
		}
		i := int(index)
		signer := i < numSigners
		var writable bool
		if signer {
			writable = i < numSigners-numReadonlySigned
		} else {
			writable = i < len(keys)-numReadonlyUnsigned
		}
		return account{key: keys[i], signer: signer, writable: writable}
	}

	out := make([]instruction, 0, len(m.Instructions))
	for n, ix := range m.Instructions {
		if int(ix.ProgramIDIndex) >= len(keys) {
			return nil, fmt.Errorf("instruction %d: program id index %d out of range", n, ix.ProgramIDIndex)
		}
		accounts := make([]account, 0, len(ix.Accounts))
		for _, index := range ix.Accounts {
			accounts = append(accounts, resolve(index))
		}
		out = append(out, instruction{
			program:  keys[ix.ProgramIDIndex],
			accounts: accounts,
			data:     []byte(ix.Data),
		})
	}
	return out, nil
}

// expectedInstructions returns the settlement instructions for the terms, in the
// order fixed by the design spec §6.1: the token transfer, the trade memo, then
// the service fee transfer, with the seller's associated token account creation
// prepended when that account does not exist yet.
func expectedInstructions(t Terms, sellerATAExists bool) ([]solana.Instruction, error) {
	transfer, err := transferChecked(t)
	if err != nil {
		return nil, err
	}
	note := memo.NewMemoInstruction(t.Memo(), t.Buyer).Build()
	fee := system.NewTransferInstruction(t.Fee.Lamports(), t.Buyer, t.Fee.Wallet()).Build()

	built := make([]solana.Instruction, 0, 4)
	if !sellerATAExists {
		built = append(built, associatedtokenaccount.NewCreateInstruction(t.Buyer, t.Seller, t.Mint).Build())
	}
	return append(built, transfer, note, fee), nil
}

// expectedCanonical is the compiled form of expectedInstructions, used by the
// verifier. Compiling rather than reading builder metadata keeps the comparison
// honest about merged account writability.
func expectedCanonical(t Terms, sellerATAExists bool) ([]instruction, error) {
	raw, err := expectedInstructions(t, sellerATAExists)
	if err != nil {
		return nil, err
	}
	tx, err := solana.NewTransaction(raw, solana.Hash{}, solana.TransactionPayer(t.Buyer))
	if err != nil {
		return nil, fmt.Errorf("assemble expected transaction: %w", err)
	}
	return compile(tx.Message)
}

func transferChecked(t Terms) (solana.Instruction, error) {
	buyerATA, err := t.BuyerATA()
	if err != nil {
		return nil, err
	}
	sellerATA, err := t.SellerATA()
	if err != nil {
		return nil, err
	}
	return token.NewTransferCheckedInstructionBuilder().
		SetAmount(t.Amount).
		SetDecimals(uint8(t.Decimals)).
		SetSourceAccount(buyerATA).
		SetMintAccount(t.Mint).
		SetDestinationAccount(sellerATA).
		SetOwnerAccount(t.Buyer).
		Build(), nil
}
