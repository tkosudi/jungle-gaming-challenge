package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"

	"github.com/gofiber/fiber/v3"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError usa um envelope único: {"error":{"code":"...","message":"..."}}. code é estável
// e documentado; message é para pessoas.
func writeError(c fiber.Ctx, status int, code, message string) error {
	return c.Status(status).JSON(fiber.Map{"error": errorBody{Code: code, Message: message}})
}

func validationError(c fiber.Ctx, message string) error {
	return writeError(c, fiber.StatusBadRequest, "VALIDATION_ERROR", message)
}

// serviceError mapeia os erros dos casos de uso. Indisponibilidades transitórias são 503 com
// Retry-After: nada foi confirmado (ou, em OUTCOME_UNKNOWN, reenviar a mesma chave é seguro).
func serviceError(c fiber.Ctx, err error) error {
	retryLater := func(code, message string) error {
		c.Set(fiber.HeaderRetryAfter, "1")
		return writeError(c, fiber.StatusServiceUnavailable, code, message)
	}
	switch {
	case errors.Is(err, usecase.ErrIdempotencyConflict):
		return writeError(c, fiber.StatusConflict, "IDEMPOTENCY_CONFLICT",
			"the idempotency key or external transaction id was already used by a different operation")
	case errors.Is(err, usecase.ErrWalletNotFound):
		return writeError(c, fiber.StatusNotFound, "WALLET_NOT_FOUND", "wallet not found")
	case errors.Is(err, usecase.ErrTransactionNotFound):
		return writeError(c, fiber.StatusNotFound, "TRANSACTION_NOT_FOUND", "transaction not found")
	case errors.Is(err, usecase.ErrLockTimeout):
		return retryLater("WALLET_BUSY", "the wallet is busy; retry with the same Idempotency-Key")
	case errors.Is(err, usecase.ErrCommitUnknown):
		return retryLater("OUTCOME_UNKNOWN", "the outcome is unknown; retry with the same Idempotency-Key")
	case errors.Is(err, usecase.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		return retryLater("SERVICE_UNAVAILABLE", "temporarily unavailable; retry with the same Idempotency-Key")
	case isValidationError(err):
		return validationError(c, err.Error())
	}
	slog.ErrorContext(c.Context(), "request failed", "method", c.Method(), "path", c.Path(), "error", err)
	return writeError(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
}

func isValidationError(err error) bool {
	for _, target := range []error{
		domain.ErrInvalidOperation, domain.ErrInvalidAmount, domain.ErrNegativeAmount,
		domain.ErrAmountOverflow, domain.ErrInvalidCurrency, domain.ErrCurrencyMismatch,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// decodeJSON exige Content-Type JSON, recusa campos desconhecidos e dados após o objeto.
// Valores monetários são strings no contrato: um número JSON falha aqui, sem passar por float.
func decodeJSON(c fiber.Ctx, dst any) (ok bool, err error) {
	mediaType, _, parseErr := mime.ParseMediaType(c.Get(fiber.HeaderContentType))
	if parseErr != nil || mediaType != fiber.MIMEApplicationJSON {
		return false, writeError(c, fiber.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
	}
	dec := json.NewDecoder(bytes.NewReader(c.Body()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return false, validationError(c, fmt.Sprintf("invalid JSON body: %v", err))
	}
	if dec.More() {
		return false, validationError(c, "invalid JSON body: unexpected data after the object")
	}
	return true, nil
}

type moneyJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func toMoneyJSON(m domain.Money) moneyJSON {
	return moneyJSON{Amount: m.Amount(), Currency: string(m.Currency())}
}

func optionalMoney(m *domain.Money) *moneyJSON {
	if m == nil {
		return nil
	}
	v := toMoneyJSON(*m)
	return &v
}
