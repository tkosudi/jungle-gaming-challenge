// Package app compõe os módulos da aplicação com Uber Fx.
package app

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/tkosudi/jungle-gaming-challenge/internal/auth"
	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
	"github.com/tkosudi/jungle-gaming-challenge/internal/httpapi"
	"github.com/tkosudi/jungle-gaming-challenge/internal/postgres"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

// New monta a aplicação Fx. Os hooks rodam na ordem em que os módulos são
// listados no start e na ordem inversa no stop: o PostgreSQL inicia antes do
// HTTP e, no shutdown, é fechado depois dele. Por isso postgres.Module vem primeiro.
// Service e verificador de tokens não têm hooks: não abrem recursos próprios (o JWKS é
// buscado sob demanda e mantido em cache).
func New(cfg config.Config, opts ...fx.Option) *fx.App {
	return fx.New(
		fx.Supply(cfg),
		postgres.Module,
		fx.Provide(newService, newVerifier, newPinger),
		httpapi.Module,
		fx.Options(opts...),
	)
}

func newService(store usecase.Store) (*usecase.Service, error) {
	return usecase.NewService(store, usecase.DefaultReferencePolicy())
}

func newVerifier(cfg config.Config) *auth.Verifier {
	return auth.NewVerifier(cfg.OIDC)
}

func newPinger(pool *pgxpool.Pool) httpapi.Pinger {
	return pool
}
