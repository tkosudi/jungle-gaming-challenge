package usecase

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
)

const (
	eventSchemaVersion = 1

	eventTransactionProcessed = "WagerTransactionProcessed"
	eventTransactionRejected  = "WagerTransactionRejected"
	eventBalanceChanged       = "WalletBalanceChanged"
	eventPendingReference     = "WagerTransactionPendingReference"
)

type moneyJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func toMoneyJSON(m domain.Money) moneyJSON {
	return moneyJSON{Amount: m.Amount(), Currency: string(m.Currency())}
}

// transactionEventData serve a WagerTransactionProcessed e WagerTransactionRejected; os campos
// externos ficam ausentes na abertura interna (OPENING).
type transactionEventData struct {
	TransactionID                  string     `json:"transactionId"`
	WalletID                       string     `json:"walletId,omitempty"`
	PlayerID                       string     `json:"playerId"`
	ProviderID                     string     `json:"providerId,omitempty"`
	ExternalTransactionID          string     `json:"externalTransactionId,omitempty"`
	RoundID                        string     `json:"roundId,omitempty"`
	GameID                         string     `json:"gameId,omitempty"`
	Kind                           string     `json:"kind"`
	ReferenceExternalTransactionID string     `json:"referenceExternalTransactionId,omitempty"`
	Status                         string     `json:"status"`
	FailureCode                    string     `json:"failureCode,omitempty"`
	Money                          moneyJSON  `json:"money"`
	Balance                        *moneyJSON `json:"balance,omitempty"`
}

type balanceChangedData struct {
	WalletID      string    `json:"walletId"`
	TransactionID string    `json:"transactionId"`
	Direction     string    `json:"direction"`
	Money         moneyJSON `json:"money"`
	BalanceBefore moneyJSON `json:"balanceBefore"`
	BalanceAfter  moneyJSON `json:"balanceAfter"`
	WalletVersion int64     `json:"walletVersion"`
}

type envelope struct {
	EventID       string `json:"eventId"`
	EventType     string `json:"eventType"`
	AggregateID   string `json:"aggregateId"`
	CorrelationID string `json:"correlationId"`
	OccurredAt    string `json:"occurredAt"`
	Version       int    `json:"version"`
	Data          any    `json:"data"`
}

// newOutboxEvent monta o snapshot imutável do evento. O agregado é a carteira (também o
// MessageGroupId da fila FIFO); sem carteira, é a própria transação. O correlationId é o
// ID da transação financeira que originou o evento.
func newOutboxEvent(eventType string, aggregateID, transactionID uuid.UUID, at time.Time, data any) (OutboxEvent, error) {
	eventID := newID()
	payload, err := json.Marshal(envelope{
		EventID:       eventID.String(),
		EventType:     eventType,
		AggregateID:   aggregateID.String(),
		CorrelationID: transactionID.String(),
		OccurredAt:    at.UTC().Format(time.RFC3339Nano),
		Version:       eventSchemaVersion,
		Data:          data,
	})
	if err != nil {
		return OutboxEvent{}, fmt.Errorf("encode %s: %w", eventType, err)
	}
	return OutboxEvent{
		EventID:        eventID,
		EventType:      eventType,
		SchemaVersion:  eventSchemaVersion,
		AggregateID:    aggregateID,
		MessageGroupID: aggregateID.String(),
		TransactionID:  transactionID,
		Payload:        payload,
		OccurredAt:     at,
	}, nil
}
