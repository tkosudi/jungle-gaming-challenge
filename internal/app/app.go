// Package app compõe os módulos da aplicação com Uber Fx.
package app

import (
	"context"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/tkosudi/jungle-gaming-challenge/internal/auth"
	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
	"github.com/tkosudi/jungle-gaming-challenge/internal/httpapi"
	"github.com/tkosudi/jungle-gaming-challenge/internal/postgres"
	"github.com/tkosudi/jungle-gaming-challenge/internal/sqs"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
	"github.com/tkosudi/jungle-gaming-challenge/internal/worker"
)

const (
	// worstCaseShutdown: workers (95s) + requisições HTTP em andamento (10s) + fechar o pool = 106s.
	worstCaseShutdown = worker.MaxStopDuration + httpapi.MaxShutdownDuration + time.Second
	// shutdownTimeout é o fx.StopTimeout; o padrão do Fx (15s) cortaria unidades de trabalho
	// no meio. compose.yaml usa stop_grace_period de 130s, acima deste valor.
	shutdownTimeout = 120 * time.Second
)

// New monta a aplicação Fx. Os hooks rodam na ordem em que os módulos são listados no start e
// na ordem inversa no stop:
//   - start: PostgreSQL (ping), HTTP, workers (cada um resolve suas filas antes de iniciar);
//   - stop: workers param de buscar trabalho e terminam o que estava em andamento, depois o
//     HTTP para de aceitar requisições e só então o pool do PostgreSQL é fechado.
//
// Service e verificador de tokens não têm hooks: não abrem recursos próprios (o JWKS é
// buscado sob demanda e mantido em cache).
//
// O prazo de parada (shutdownTimeout) cobre a soma dos piores casos, porque os hooks param um
// depois do outro: workers, HTTP e PostgreSQL. O Compose espera mais que isso antes de matar o
// processo (stop_grace_period).
func New(cfg config.Config, opts ...fx.Option) *fx.App {
	return fx.New(
		fx.StopTimeout(shutdownTimeout),
		fx.Supply(cfg),
		postgres.Module,
		fx.Provide(newService, newVerifier, newReadinessChecks),
		httpapi.Module,
		worker.Module,
		fx.Options(opts...),
	)
}

func newService(store usecase.Store, cfg config.Config) (*usecase.Service, error) {
	return usecase.NewService(store, usecase.ReferencePolicy{
		InitialBackoff: cfg.Workers.ReferenceInitialBackoff,
		MaxBackoff:     cfg.Workers.ReferenceMaxBackoff,
		MaxAttempts:    cfg.Workers.ReferenceMaxAttempts,
	})
}

func newVerifier(cfg config.Config) *auth.Verifier {
	return auth.NewVerifier(cfg.OIDC)
}

// newReadinessChecks: PostgreSQL responde e a fila de entrada existe e responde.
func newReadinessChecks(pool *pgxpool.Pool, client *awssqs.Client, cfg config.Config) []httpapi.ReadinessCheck {
	return []httpapi.ReadinessCheck{
		{Name: "postgres", Check: pool.Ping},
		{Name: "sqs", Check: func(ctx context.Context) error {
			_, err := sqs.QueueURL(ctx, client, cfg.SQS.RequestsQueue)
			return err
		}},
	}
}
