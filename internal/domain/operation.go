package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrInvalidOperation = errors.New("domain: invalid operation")

const maxIdentifierLength = 255

// Kind é o tipo de uma operação externa. OPENING existe só no banco: a abertura de carteira é
// interna e nunca entra pelo transporte.
type Kind string

const (
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

// OperationInput reúne os campos de negócio de uma operação como chegam do transporte (HTTP ou
// SQS). O provedor não está aqui de propósito: ele deve vir da identidade autenticada.
type OperationInput struct {
	Kind                           string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Amount                         string
	Currency                       string
	ReferenceExternalTransactionID string
}

// Operation é uma operação externa validada: identificadores bem formados, valor coerente com
// o tipo e referência presente exatamente quando o tipo a exige.
type Operation struct {
	Kind                           Kind
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Money                          Money
	ReferenceExternalTransactionID string // vazio quando não há referência
}

// NewOperation valida a entrada. Valor: LOSS exige zero; os demais, maior que zero.
// Referência: obrigatória em REFUND e ROLLBACK, opcional em WIN, proibida em BET e LOSS.
func NewOperation(in OperationInput) (Operation, error) {
	kind := Kind(in.Kind)
	switch kind {
	case KindBet, KindWin, KindLoss, KindRefund, KindRollback:
	default:
		return Operation{}, fmt.Errorf("%w: kind %q is not accepted", ErrInvalidOperation, in.Kind)
	}
	for _, f := range []struct{ name, value string }{
		{"externalTransactionId", in.ExternalTransactionID},
		{"idempotencyKey", in.IdempotencyKey},
		{"roundId", in.RoundID},
		{"gameId", in.GameID},
	} {
		if err := validateIdentifier(f.name, f.value); err != nil {
			return Operation{}, err
		}
	}
	playerID, err := uuid.Parse(in.PlayerID)
	if err != nil {
		return Operation{}, fmt.Errorf("%w: playerId is not a UUID", ErrInvalidOperation)
	}
	walletID, err := uuid.Parse(in.WalletID)
	if err != nil {
		return Operation{}, fmt.Errorf("%w: walletId is not a UUID", ErrInvalidOperation)
	}
	money, err := NewMoney(in.Amount, in.Currency)
	if err != nil {
		return Operation{}, err
	}
	switch {
	case kind == KindLoss && !money.IsZero():
		return Operation{}, fmt.Errorf("%w: LOSS amount must be zero", ErrInvalidOperation)
	case kind != KindLoss && !money.IsPositive():
		return Operation{}, fmt.Errorf("%w: amount must be greater than zero", ErrInvalidOperation)
	}

	reference := in.ReferenceExternalTransactionID
	switch {
	case reference != "":
		if err := validateIdentifier("referenceExternalTransactionId", reference); err != nil {
			return Operation{}, err
		}
		if kind == KindBet || kind == KindLoss {
			return Operation{}, fmt.Errorf("%w: %s does not accept a reference", ErrInvalidOperation, kind)
		}
		if reference == in.ExternalTransactionID {
			return Operation{}, fmt.Errorf("%w: an operation cannot reference itself", ErrInvalidOperation)
		}
	case kind == KindRefund || kind == KindRollback:
		return Operation{}, fmt.Errorf("%w: %s requires referenceExternalTransactionId", ErrInvalidOperation, kind)
	}

	return Operation{
		Kind:                           kind,
		ExternalTransactionID:          in.ExternalTransactionID,
		IdempotencyKey:                 in.IdempotencyKey,
		PlayerID:                       playerID,
		WalletID:                       walletID,
		RoundID:                        in.RoundID,
		GameID:                         in.GameID,
		Money:                          money,
		ReferenceExternalTransactionID: reference,
	}, nil
}

func validateIdentifier(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: %s must be non-empty without surrounding spaces", ErrInvalidOperation, name)
	}
	// json troca bytes inválidos por U+FFFD, o que faria valores distintos colidirem no hash.
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidOperation, name)
	}
	if len(value) > maxIdentifierLength {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidOperation, name, maxIdentifierLength)
	}
	return nil
}

// canonicalPayload fixa a forma canônica: campos declarados em ordem alfabética do nome JSON,
// que é a ordem em que encoding/json serializa uma struct.
type canonicalPayload struct {
	Amount                         string `json:"amount"`
	Currency                       string `json:"currency"`
	ExternalTransactionID          string `json:"externalTransactionId"`
	GameID                         string `json:"gameId"`
	Kind                           string `json:"kind"`
	PlayerID                       string `json:"playerId"`
	ProviderID                     string `json:"providerId"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId,omitempty"`
	RoundID                        string `json:"roundId"`
	WalletID                       string `json:"walletId"`
}

// CanonicalJSON é o documento de negócio sobre o qual o hash de idempotência é calculado:
// JSON compacto, chaves em ordem alfabética, UUIDs em minúsculas, valor como "d.dd" derivado
// dos centavos, referência omitida quando ausente e sem escape de HTML. A chave de
// idempotência e metadados de transporte ficam de fora; o provedor entra porque delimita o
// namespace da operação. Mudar qualquer detalhe invalida a comparação com hashes persistidos.
func (o Operation) CanonicalJSON(providerID string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// A struct só tem strings válidas em UTF-8 (NewOperation valida): o encode não falha.
	_ = enc.Encode(canonicalPayload{
		Amount:                         o.Money.Amount(),
		Currency:                       string(o.Money.Currency()),
		ExternalTransactionID:          o.ExternalTransactionID,
		GameID:                         o.GameID,
		Kind:                           string(o.Kind),
		PlayerID:                       o.PlayerID.String(),
		ProviderID:                     providerID,
		ReferenceExternalTransactionID: o.ReferenceExternalTransactionID,
		RoundID:                        o.RoundID,
		WalletID:                       o.WalletID.String(),
	})
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// PayloadHash é o SHA-256 de CanonicalJSON; distingue replay (mesma operação) de conflito
// (mesma chave, outra operação).
func (o Operation) PayloadHash(providerID string) [sha256.Size]byte {
	return sha256.Sum256(o.CanonicalJSON(providerID))
}
