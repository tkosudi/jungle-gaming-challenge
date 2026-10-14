package sqs

import (
	"errors"
	"strings"
	"testing"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
)

const validBody = `{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z",
"data":{"providerId":"provider-a","externalTransactionId":"transaction-123","idempotencyKey":"provider-a:transaction-123",
"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
"roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`

func TestParseRequest_ValidEnvelopeBecomesTheSameOperationAsHTTP(t *testing.T) {
	req, err := ParseRequest([]byte(validBody))
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}
	if req.MessageID != "msg-123" || req.ProviderID != "provider-a" {
		t.Fatalf("request = %+v", req)
	}
	want, err := domain.NewOperation(domain.OperationInput{
		Kind: "BET", ExternalTransactionID: "transaction-123", IdempotencyKey: "provider-a:transaction-123",
		PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37",
		RoundID: "round-987", GameID: "fortune-chimp", Amount: "25.00", Currency: "BRL",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Operation != want || req.Operation.PayloadHash("provider-a") != want.PayloadHash("provider-a") {
		t.Fatalf("operation = %+v, want %+v", req.Operation, want)
	}
}

func TestParseRequest_ContentHash(t *testing.T) {
	base, _ := ParseRequest([]byte(validBody))
	same, _ := ParseRequest([]byte(strings.Replace(validBody, `"occurredAt":"2026-09-08T12:00:00.000Z"`, `"occurredAt":"2026-09-08T12:05:00.000Z"`, 1)))
	if same.ContentHash != base.ContentHash {
		t.Error("occurredAt (transport metadata) changed the content hash")
	}
	for name, body := range map[string]string{
		"amount":          strings.Replace(validBody, `"25.00"`, `"25.01"`, 1),
		"idempotency key": strings.Replace(validBody, `"provider-a:transaction-123"`, `"other-key"`, 1),
		"kind":            strings.Replace(validBody, `"kind":"BET"`, `"kind":"WIN"`, 1),
		"provider":        strings.Replace(validBody, `"providerId":"provider-a"`, `"providerId":"provider-b"`, 1),
	} {
		other, err := ParseRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if other.ContentHash == base.ContentHash {
			t.Errorf("changing %s kept the content hash", name)
		}
	}
}

func TestParseRequest_RejectsInvalidMessages(t *testing.T) {
	replace := func(old, new string) string { return strings.Replace(validBody, old, new, 1) }
	for name, body := range map[string]string{
		"not JSON":             `kind=BET`,
		"wrong type":           replace(`"WagerTransactionRequested"`, `"WagerTransactionProcessed"`),
		"missing message id":   replace(`"messageId":"msg-123",`, ``),
		"blank message id":     replace(`"msg-123"`, `" "`),
		"missing occurredAt":   replace(`"occurredAt":"2026-09-08T12:00:00.000Z",`, ``),
		"invalid occurredAt":   replace(`"2026-09-08T12:00:00.000Z"`, `"yesterday"`),
		"missing provider":     replace(`"providerId":"provider-a",`, ``),
		"missing key":          replace(`"idempotencyKey":"provider-a:transaction-123",`, ``),
		"amount as a number":   replace(`"amount":"25.00"`, `"amount":25.00`),
		"negative amount":      replace(`"amount":"25.00"`, `"amount":"-25.00"`),
		"OPENING is internal":  replace(`"kind":"BET"`, `"kind":"OPENING"`),
		"unknown field":        replace(`"kind":"BET"`, `"kind":"BET","extra":true`),
		"unknown envelope key": replace(`"type":`, `"extra":1,"type":`),
		"missing money":        replace(`,"money":{"amount":"25.00","currency":"BRL"}`, ``),
		"trailing data":        validBody + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRequest([]byte(body)); !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("ParseRequest() error = %v, want ErrInvalidMessage", err)
			}
		})
	}
}
