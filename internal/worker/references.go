package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/tkosudi/jungle-gaming-challenge/internal/metrics"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

const referenceStepTimeout = 15 * time.Second

// runReferences chama Service.ResolveNextPendingReference em laço. Tentativas, atraso e
// expiração vivem no PostgreSQL (M1.3); qualquer instância continua de onde outra parou.
// Sem pendência vencida, espera interval.
func runReferences(ctx context.Context, service *usecase.Service, interval time.Duration) {
	for ctx.Err() == nil {
		stepCtx, cancel := workContext(ctx, referenceStepTimeout)
		step, err := service.ResolveNextPendingReference(stepCtx)
		cancel()
		switch {
		case err != nil:
			metrics.WorkerErrors.WithLabelValues("references").Inc()
			slog.Error("pending reference step failed; nothing was changed", "error", err)
			sleep(ctx, interval)
		case !step.Found:
			sleep(ctx, interval)
		default:
			metrics.ReferenceSteps.WithLabelValues(string(step.Status)).Inc()
			slog.Info("pending reference step", "transactionId", step.TransactionID.String(),
				"status", string(step.Status), "failureCode", step.FailureCode, "attempts", step.Attempts)
		}
	}
}
