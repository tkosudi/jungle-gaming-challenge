package httpapi

import (
	"errors"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
	"github.com/tkosudi/jungle-gaming-challenge/internal/metrics"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

type operationRequest struct {
	ProviderID                     string     `json:"providerId"`
	ExternalTransactionID          string     `json:"externalTransactionId"`
	PlayerID                       string     `json:"playerId"`
	WalletID                       string     `json:"walletId"`
	RoundID                        string     `json:"roundId"`
	GameID                         string     `json:"gameId"`
	Kind                           string     `json:"kind"`
	Money                          *moneyJSON `json:"money"`
	ReferenceExternalTransactionID string     `json:"referenceExternalTransactionId"`
}

type operationResponse struct {
	TransactionID    string     `json:"transactionId"`
	Status           string     `json:"status"`
	FailureCode      string     `json:"failureCode,omitempty"`
	Balance          *moneyJSON `json:"balance,omitempty"`
	IdempotentReplay bool       `json:"idempotentReplay"`
}

// submitOperation recebe BET, WIN, LOSS, REFUND e ROLLBACK. O provedor da operação é o do
// token; o providerId do corpo faz parte do contrato e precisa ser igual a ele.
//
// Respostas: 200 PROCESSED; 202 PENDING_REFERENCE (Location aponta a consulta); 422 REJECTED
// (rejeição de negócio persistida, com failureCode); replays devolvem o mesmo status com
// idempotentReplay=true. 400 entrada inválida, 403 provedor divergente, 409 conflito de
// idempotência, 503 indisponibilidade transitória.
func (a *api) submitOperation(c fiber.Ctx) error {
	var req operationRequest
	if ok, err := decodeJSON(c, &req); !ok {
		return err
	}
	p := principal(c)
	switch {
	case req.ProviderID == "":
		return validationError(c, "providerId is required")
	case req.ProviderID != p.ProviderID:
		return writeError(c, fiber.StatusForbidden, "PROVIDER_MISMATCH", "providerId does not match the authenticated provider")
	case req.Money == nil:
		return validationError(c, "money is required")
	}
	// A chave é usada como recebida: o servidor não a substitui nem normaliza.
	key := c.Get("Idempotency-Key")
	if key == "" {
		return validationError(c, "Idempotency-Key header is required")
	}
	op, err := domain.NewOperation(domain.OperationInput{
		Kind: req.Kind, ExternalTransactionID: req.ExternalTransactionID, IdempotencyKey: key,
		PlayerID: req.PlayerID, WalletID: req.WalletID, RoundID: req.RoundID, GameID: req.GameID,
		Amount: req.Money.Amount, Currency: req.Money.Currency,
		ReferenceExternalTransactionID: req.ReferenceExternalTransactionID,
	})
	if err != nil {
		return validationError(c, err.Error())
	}

	ctx, cancel := requestContext(c)
	defer cancel()
	start := time.Now()
	res, err := a.service.Process(ctx, p.ProviderID, op)
	metrics.ProcessingDuration.WithLabelValues("http").Observe(time.Since(start).Seconds())
	if err != nil {
		switch {
		case errors.Is(err, usecase.ErrLockTimeout):
			metrics.Conflicts.WithLabelValues("http", "wallet_lock_timeout").Inc()
		case errors.Is(err, usecase.ErrIdempotencyConflict):
			metrics.Conflicts.WithLabelValues("http", "idempotency").Inc()
		}
		return serviceError(c, err)
	}
	if res.Replay {
		metrics.Duplicates.WithLabelValues("http").Inc()
	} else {
		metrics.Operations.WithLabelValues("http", string(res.Status)).Inc()
	}
	// correlationId é o ID da transação financeira, o mesmo dos eventos publicados pela outbox.
	slog.InfoContext(ctx, "wagering operation completed", "correlationId", res.TransactionID.String(),
		"transactionId", res.TransactionID.String(), "walletId", op.WalletID.String(), "providerId", p.ProviderID,
		"kind", string(op.Kind), "status", string(res.Status), "replay", res.Replay)
	status := fiber.StatusOK
	switch res.Status {
	case domain.StatusPendingReference:
		status = fiber.StatusAccepted
		c.Set(fiber.HeaderLocation, "/wagering/transactions/"+res.TransactionID.String())
	case domain.StatusRejected:
		status = fiber.StatusUnprocessableEntity
	case domain.StatusFailed:
		status = fiber.StatusInternalServerError
	}
	return c.Status(status).JSON(operationResponse{
		TransactionID: res.TransactionID.String(), Status: string(res.Status), FailureCode: res.FailureCode,
		Balance: optionalMoney(res.Balance), IdempotentReplay: res.Replay,
	})
}

type transactionResponse struct {
	TransactionID                  string     `json:"transactionId"`
	ProviderID                     string     `json:"providerId"`
	ExternalTransactionID          string     `json:"externalTransactionId"`
	PlayerID                       string     `json:"playerId"`
	WalletID                       string     `json:"walletId,omitempty"`
	RoundID                        string     `json:"roundId"`
	GameID                         string     `json:"gameId"`
	Kind                           string     `json:"kind"`
	Money                          moneyJSON  `json:"money"`
	Status                         string     `json:"status"`
	FailureCode                    string     `json:"failureCode,omitempty"`
	Balance                        *moneyJSON `json:"balance,omitempty"`
	ReferenceExternalTransactionID string     `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string     `json:"referenceTransactionId,omitempty"`
	ReferenceAttempts              int        `json:"referenceAttempts"`
	NextAttemptAt                  string     `json:"nextAttemptAt,omitempty"`
	CreatedAt                      string     `json:"createdAt"`
	UpdatedAt                      string     `json:"updatedAt"`
}

func toTransactionResponse(t usecase.Transaction) transactionResponse {
	r := transactionResponse{
		TransactionID: t.ID.String(), ProviderID: t.ProviderID, ExternalTransactionID: t.ExternalTransactionID,
		PlayerID: t.PlayerID.String(), RoundID: t.RoundID, GameID: t.GameID, Kind: t.Kind,
		Money: toMoneyJSON(t.Money), Status: string(t.Status), FailureCode: t.FailureCode,
		Balance: optionalMoney(t.ResultBalance), ReferenceExternalTransactionID: t.ReferenceExternalTransactionID,
		ReferenceAttempts: t.ReferenceAttempts, CreatedAt: timestamp(t.CreatedAt), UpdatedAt: timestamp(t.UpdatedAt),
	}
	if t.WalletID != nil {
		r.WalletID = t.WalletID.String()
	}
	if t.ReferenceTransactionID != nil {
		r.ReferenceTransactionID = t.ReferenceTransactionID.String()
	}
	if t.NextAttemptAt != nil {
		r.NextAttemptAt = timestamp(*t.NextAttemptAt)
	}
	return r
}

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// getTransaction só encontra transações do provedor do token: a de outro provedor responde
// 404, sem revelar que existe.
func (a *api) getTransaction(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("transactionId"))
	if err != nil {
		return validationError(c, "transactionId must be a UUID")
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	t, err := a.service.GetTransaction(ctx, principal(c).ProviderID, id)
	if err != nil {
		return serviceError(c, err)
	}
	return c.JSON(toTransactionResponse(t))
}

// getTransactionByExternalID: o providerId do caminho precisa ser o do token (403 caso
// contrário, independentemente de a transação existir).
func (a *api) getTransactionByExternalID(c fiber.Ctx) error {
	p := principal(c)
	if c.Params("providerId") != p.ProviderID {
		return forbidden(c)
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	t, err := a.service.GetTransactionByExternalID(ctx, p.ProviderID, c.Params("externalTransactionId"))
	if err != nil {
		return serviceError(c, err)
	}
	return c.JSON(toTransactionResponse(t))
}
