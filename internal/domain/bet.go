package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

var ErrInvalidBet = errors.New("domain: invalid bet")

// Códigos de falha estáveis persistidos em wager_transactions.failure_code.
const (
	FailureInsufficientFunds = "INSUFFICIENT_FUNDS"
	FailureWalletNotFound    = "WALLET_NOT_FOUND"
	FailureCurrencyMismatch  = "CURRENCY_MISMATCH"
)

const maxIdentifierLength = 255

// BetInput reúne os campos de negócio de uma BET como chegam do transporte (HTTP ou SQS).
// O provedor não está aqui de propósito: ele deve vir da identidade autenticada.
type BetInput struct {
	ExternalTransactionID string
	IdempotencyKey        string
	PlayerID              string
	WalletID              string
	RoundID               string
	GameID                string
	Amount                string
	Currency              string
}

// Bet é uma aposta validada: identificadores bem formados e valor estritamente positivo.
type Bet struct {
	ExternalTransactionID string
	IdempotencyKey        string
	PlayerID              uuid.UUID
	WalletID              uuid.UUID
	RoundID               string
	GameID                string
	Money                 Money
}

func NewBet(in BetInput) (Bet, error) {
	for _, f := range []struct{ name, value string }{
		{"externalTransactionId", in.ExternalTransactionID},
		{"idempotencyKey", in.IdempotencyKey},
		{"roundId", in.RoundID},
		{"gameId", in.GameID},
	} {
		if err := validateIdentifier(f.name, f.value); err != nil {
			return Bet{}, err
		}
	}
	playerID, err := uuid.Parse(in.PlayerID)
	if err != nil {
		return Bet{}, fmt.Errorf("%w: playerId is not a UUID", ErrInvalidBet)
	}
	walletID, err := uuid.Parse(in.WalletID)
	if err != nil {
		return Bet{}, fmt.Errorf("%w: walletId is not a UUID", ErrInvalidBet)
	}
	money, err := NewMoney(in.Amount, in.Currency)
	if err != nil {
		return Bet{}, err
	}
	if !money.IsPositive() {
		return Bet{}, fmt.Errorf("%w: amount must be greater than zero", ErrInvalidBet)
	}
	return Bet{
		ExternalTransactionID: in.ExternalTransactionID,
		IdempotencyKey:        in.IdempotencyKey,
		PlayerID:              playerID,
		WalletID:              walletID,
		RoundID:               in.RoundID,
		GameID:                in.GameID,
		Money:                 money,
	}, nil
}

func validateIdentifier(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: %s must be non-empty without surrounding spaces", ErrInvalidBet, name)
	}
	if len(value) > maxIdentifierLength {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidBet, name, maxIdentifierLength)
	}
	return nil
}

// PayloadHash é o SHA-256 da forma canônica dos campos de negócio, usado para distinguir
// replay (mesma operação) de conflito (mesma chave, outra operação).
//
// Canonicalização v1: tag de versão, depois cada campo como uvarint(tamanho)+bytes, na ordem
// fixa abaixo; UUIDs em minúsculas com hífens e o valor como "d.dd" derivado dos centavos.
// A chave de idempotência e metadados de transporte ficam de fora; o provedor entra porque
// delimita o namespace da operação. Mudar qualquer detalhe exige nova tag de versão.
func (b Bet) PayloadHash(providerID string) [sha256.Size]byte {
	var buf []byte
	for _, field := range []string{
		"jungle.wager.v1",
		providerID,
		b.ExternalTransactionID,
		b.PlayerID.String(),
		b.WalletID.String(),
		b.RoundID,
		b.GameID,
		"BET",
		b.Money.Amount(),
		string(b.Money.Currency()),
	} {
		buf = binary.AppendUvarint(buf, uint64(len(field)))
		buf = append(buf, field...)
	}
	return sha256.Sum256(buf)
}
