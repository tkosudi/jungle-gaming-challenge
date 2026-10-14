//go:build integration

package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
	"github.com/tkosudi/jungle-gaming-challenge/internal/httpapi"
)

func testDatabaseURL() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://jungle:localdev-only@127.0.0.1:5432/jungle?sslmode=disable"
}

func ctxTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func newApp(t *testing.T, cfg config.Config) (*fx.App, *httpapi.Server, *pgxpool.Pool) {
	t.Helper()
	var (
		srv  *httpapi.Server
		pool *pgxpool.Pool
	)
	app := New(cfg, fx.NopLogger, fx.Populate(&srv, &pool))
	if err := app.Err(); err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return app, srv, pool
}

func TestApp_StartServesAndStopsReleasingResources(t *testing.T) {
	app, srv, pool := newApp(t, config.Config{HTTPAddr: "127.0.0.1:0", DatabaseURL: testDatabaseURL()})

	if err := app.Start(ctxTimeout(t, 10*time.Second)); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	addr := srv.Addr().String()

	resp, err := http.Get(fmt.Sprintf("http://%s/health/live", addr))
	if err != nil {
		t.Fatalf("GET /health/live: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if err := pool.Ping(ctxTimeout(t, 2*time.Second)); err != nil {
		t.Fatalf("pool ping while running: %v", err)
	}

	begin := time.Now()
	if err := app.Stop(ctxTimeout(t, 10*time.Second)); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if elapsed := time.Since(begin); elapsed > 5*time.Second {
		t.Errorf("Stop() took %v, want bounded shutdown", elapsed)
	}

	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Error("HTTP listener still open after Stop()")
	}
	if got := pool.Stat().TotalConns(); got != 0 {
		t.Errorf("pool TotalConns after Stop = %d, want 0", got)
	}
	if err := pool.Ping(ctxTimeout(t, 2*time.Second)); err == nil {
		t.Error("pool still usable after Stop()")
	}
}

func TestApp_StartFailsWhenHTTPPortBusy_ReleasesPool(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	app, _, pool := newApp(t, config.Config{HTTPAddr: busy.Addr().String(), DatabaseURL: testDatabaseURL()})

	err = app.Start(ctxTimeout(t, 10*time.Second))
	if err == nil {
		_ = app.Stop(context.Background())
		t.Fatal("Start() error = nil, want address in use")
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("Start() error = %v, want EADDRINUSE", err)
	}
	// O rollback do Fx deve ter fechado o pool já iniciado.
	if got := pool.Stat().TotalConns(); got != 0 {
		t.Errorf("pool TotalConns after failed start = %d, want 0", got)
	}
}

func TestApp_StartFailsWhenDatabaseUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()

	app, srv, _ := newApp(t, config.Config{
		HTTPAddr:    "127.0.0.1:0",
		DatabaseURL: fmt.Sprintf("postgres://jungle:x@%s/jungle?sslmode=disable", dead),
	})

	begin := time.Now()
	err = app.Start(ctxTimeout(t, 10*time.Second))
	if err == nil {
		_ = app.Stop(context.Background())
		t.Fatal("Start() error = nil, want database failure")
	}
	if elapsed := time.Since(begin); elapsed > 8*time.Second {
		t.Errorf("Start() took %v, want bounded", elapsed)
	}
	if srv.Addr() != nil {
		t.Error("HTTP server must not start when the database is unavailable")
	}
}
