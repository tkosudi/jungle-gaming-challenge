// Package postgres gerencia a conexão com o PostgreSQL via pgx/v5.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool cria um pgxpool e verifica a conectividade com Ping.
// O handshake e o ping respeitam ctx e o limite connectTimeout; em caso de falha,
// o pool é fechado e nenhum recurso permanece aberto.
func NewPool(ctx context.Context, databaseURL string, connectTimeout time.Duration) (*pgxpool.Pool, error) {
	pool, err := newPool(databaseURL, connectTimeout)
	if err != nil {
		return nil, err
	}
	if err := ping(ctx, pool, connectTimeout); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// newPool constrói o pool sem abrir conexões (MinConns = 0).
func newPool(databaseURL string, connectTimeout time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse database url: %w", err)
	}
	cfg.ConnConfig.ConnectTimeout = connectTimeout
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	return pool, nil
}

func ping(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: ping: %w", err)
	}
	return nil
}
