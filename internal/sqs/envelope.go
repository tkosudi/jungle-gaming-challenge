// Package sqs integra a aplicação ao AWS SQS: cliente, filas e o contrato das mensagens de
// entrada. O consumo e a publicação (laços) ficam em internal/worker.
package sqs

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
)

// RequestType é o único tipo aceito na fila de entrada.
const RequestType = "WagerTransactionRequested"

// ErrInvalidMessage: a mensagem não segue o contrato. É permanente: reentregar não ajuda.
var ErrInvalidMessage = errors.New("sqs: invalid message")

type moneyJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type requestData struct {
	ProviderID                     string     `json:"providerId"`
	ExternalTransactionID          string     `json:"externalTransactionId"`
	IdempotencyKey                 string     `json:"idempotencyKey"`
	PlayerID                       string     `json:"playerId"`
	WalletID                       string     `json:"walletId"`
	RoundID                        string     `json:"roundId"`
	GameID                         string     `json:"gameId"`
	Kind                           string     `json:"kind"`
	Money                          *moneyJSON `json:"money"`
	ReferenceExternalTransactionID string     `json:"referenceExternalTransactionId"`
}

type envelope struct {
	MessageID  string       `json:"messageId"`
	Type       string       `json:"type"`
	OccurredAt string       `json:"occurredAt"`
	Data       *requestData `json:"data"`
}

// Request é uma mensagem de entrada validada. ProviderID é o declarado na mensagem: só vale
// como identidade depois de o consumidor conferi-lo contra a fronteira de confiança do broker.
type Request struct {
	MessageID  string
	ProviderID string
	Operation  domain.Operation
	// ContentHash identifica o conteúdo para a inbox: tipo, chave de idempotência e a forma
	// canônica da operação (que inclui o provedor). messageId e occurredAt ficam de fora: o
	// primeiro é a identidade, o segundo metadado de transporte que um reenvio pode mudar.
	ContentHash [32]byte
}

// ParseRequest valida o envelope e converte data no mesmo domain.Operation do HTTP. Como no
// HTTP, valores monetários são strings, campos desconhecidos e dados extras são recusados.
func ParseRequest(body []byte) (Request, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var env envelope
	if err := dec.Decode(&env); err != nil {
		return Request{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if dec.More() {
		return Request{}, fmt.Errorf("%w: unexpected data after the envelope", ErrInvalidMessage)
	}
	switch {
	case env.Type != RequestType:
		return Request{}, fmt.Errorf("%w: type %q is not %s", ErrInvalidMessage, env.Type, RequestType)
	case !validIdentifier(env.MessageID):
		return Request{}, fmt.Errorf("%w: messageId is required", ErrInvalidMessage)
	case env.Data == nil || env.Data.Money == nil:
		return Request{}, fmt.Errorf("%w: data and data.money are required", ErrInvalidMessage)
	case !validIdentifier(env.Data.ProviderID):
		return Request{}, fmt.Errorf("%w: data.providerId is required", ErrInvalidMessage)
	}
	if _, err := time.Parse(time.RFC3339, env.OccurredAt); err != nil {
		return Request{}, fmt.Errorf("%w: occurredAt must be RFC 3339", ErrInvalidMessage)
	}
	d := env.Data
	op, err := domain.NewOperation(domain.OperationInput{
		Kind: d.Kind, ExternalTransactionID: d.ExternalTransactionID, IdempotencyKey: d.IdempotencyKey,
		PlayerID: d.PlayerID, WalletID: d.WalletID, RoundID: d.RoundID, GameID: d.GameID,
		Amount: d.Money.Amount, Currency: d.Money.Currency, ReferenceExternalTransactionID: d.ReferenceExternalTransactionID,
	})
	if err != nil {
		return Request{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	content, err := json.Marshal(struct {
		Type           string          `json:"type"`
		IdempotencyKey string          `json:"idempotencyKey"`
		Operation      json.RawMessage `json:"operation"`
	}{RequestType, op.IdempotencyKey, op.CanonicalJSON(d.ProviderID)})
	if err != nil {
		return Request{}, fmt.Errorf("sqs: encode content hash: %w", err)
	}
	return Request{MessageID: env.MessageID, ProviderID: d.ProviderID, Operation: op, ContentHash: sha256.Sum256(content)}, nil
}

func validIdentifier(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && utf8.ValidString(s) && len(s) <= 255
}
