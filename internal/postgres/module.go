package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
)

const defaultConnectTimeout = 5 * time.Second

// Module fornece o *pgxpool.Pool. O pool nasce sem conexões; o ping acontece no
// OnStart, para que um banco indisponível falhe o startup (e não a construção).
var Module = fx.Module("postgres",
	fx.Provide(providePool),
	fx.Invoke(registerHooks),
)

func providePool(cfg config.Config) (*pgxpool.Pool, error) {
	return newPool(cfg.DatabaseURL, defaultConnectTimeout)
}

func registerHooks(lc fx.Lifecycle, pool *pgxpool.Pool) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := ping(ctx, pool, defaultConnectTimeout); err != nil {
				// O Fx não executa o OnStop do hook que falhou no start.
				pool.Close()
				return err
			}
			return nil
		},
		OnStop: func(context.Context) error {
			pool.Close()
			return nil
		},
	})
}
