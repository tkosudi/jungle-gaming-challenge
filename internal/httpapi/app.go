// Package httpapi expõe a camada de transporte HTTP baseada em Fiber v3. Os handlers só
// traduzem HTTP <-> casos de uso: autenticação, autorização por rota, validação de entrada e
// mapeamento de erros. Regras financeiras e SQL ficam em usecase/domain/postgres.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tkosudi/jungle-gaming-challenge/internal/auth"
	"github.com/tkosudi/jungle-gaming-challenge/internal/metrics"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

const (
	maxBodyBytes   = 64 << 10
	requestTimeout = 10 * time.Second
	// MaxShutdownDuration: o shutdown do Fiber espera as requisições em andamento, limitadas a
	// requestTimeout.
	MaxShutdownDuration = requestTimeout
	readinessTimeout    = 2 * time.Second
)

// ReadinessCheck verifica uma dependência necessária para atender (PostgreSQL, SQS). O IdP
// não entra: indisponibilidade do Keycloak afeta só a autenticação (503 nas rotas protegidas).
type ReadinessCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

type api struct {
	service  *usecase.Service
	verifier *auth.Verifier
	checks   []ReadinessCheck
}

// NewApp cria a aplicação Fiber com todas as rotas. Health checks são públicos; operações de
// carteira exigem o papel interno; operações de apostas exigem a identidade de um provedor.
func NewApp(service *usecase.Service, verifier *auth.Verifier, checks []ReadinessCheck) *fiber.App {
	app := fiber.New(fiber.Config{BodyLimit: maxBodyBytes, ErrorHandler: handleError})
	a := &api{service: service, verifier: verifier, checks: checks}

	app.Get("/health/live", live)
	app.Get("/health/ready", a.ready)
	// Métricas operacionais em formato Prometheus, sem dados de clientes (como os health checks).
	app.Get("/metrics", adaptor.HTTPHandler(promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{})))

	internal := a.requireRole(auth.RoleWalletOperator)
	app.Post("/wallets", internal, a.openWallet)
	app.Get("/wallets/:walletId", internal, a.getWallet)
	app.Get("/wallets/:walletId/ledger", internal, a.listLedger)
	app.Post("/wallets/:walletId/reconciliation", internal, a.reconcile)

	provider := a.requireProvider()
	app.Post("/wagering/transactions", provider, a.submitOperation)
	app.Get("/wagering/transactions/:transactionId", provider, a.getTransaction)
	app.Get("/providers/:providerId/wagering/transactions/:externalTransactionId", provider, a.getTransactionByExternalID)
	return app
}

func live(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"status": "ok"})
}

// ready responde 503 enquanto alguma dependência (PostgreSQL, SQS) não responde. Liveness
// (/health/live) não depende de nada externo.
func (a *api) ready(c fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(c.Context(), readinessTimeout)
	defer cancel()
	status, results := "ok", fiber.Map{}
	for _, check := range a.checks {
		results[check.Name] = "ok"
		if err := check.Check(ctx); err != nil {
			status, results[check.Name] = "unavailable", "unavailable"
		}
	}
	if status != "ok" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"status": status, "checks": results})
	}
	return c.JSON(fiber.Map{"status": status, "checks": results})
}

// requestContext limita o tempo de cada requisição: o contexto do Fiber não é cancelado
// quando o cliente desiste.
func requestContext(c fiber.Ctx) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Context(), requestTimeout)
}

// handleError converte erros do Fiber (rota inexistente, corpo grande demais...) e erros não
// tratados no mesmo envelope JSON das demais respostas.
func handleError(c fiber.Ctx, err error) error {
	var fe *fiber.Error
	if errors.As(err, &fe) {
		switch fe.Code {
		case fiber.StatusNotFound:
			return writeError(c, fe.Code, "NOT_FOUND", "route not found")
		case fiber.StatusMethodNotAllowed:
			return writeError(c, fe.Code, "METHOD_NOT_ALLOWED", "method not allowed")
		case fiber.StatusRequestEntityTooLarge:
			return writeError(c, fe.Code, "PAYLOAD_TOO_LARGE", "request body is too large")
		case fiber.StatusBadRequest:
			return writeError(c, fe.Code, "VALIDATION_ERROR", fe.Message)
		}
	}
	slog.ErrorContext(c.Context(), "unhandled request error", "method", c.Method(), "path", c.Path(), "error", err)
	return writeError(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
}
