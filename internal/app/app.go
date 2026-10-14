// Package app compõe os módulos da aplicação com Uber Fx.
package app

import (
	"go.uber.org/fx"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
	"github.com/tkosudi/jungle-gaming-challenge/internal/httpapi"
	"github.com/tkosudi/jungle-gaming-challenge/internal/postgres"
)

// New monta a aplicação Fx. Os hooks rodam na ordem em que os módulos são
// listados no start e na ordem inversa no stop: o PostgreSQL inicia antes do
// HTTP e, no shutdown, é fechado depois dele. Por isso postgres.Module vem primeiro.
func New(cfg config.Config, opts ...fx.Option) *fx.App {
	return fx.New(
		fx.Supply(cfg),
		postgres.Module,
		httpapi.Module,
		fx.Options(opts...),
	)
}
