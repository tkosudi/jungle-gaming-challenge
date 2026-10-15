//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/tkosudi/jungle-gaming-challenge/internal/testenv"
)

// Banco de teste: TEST_DATABASE_URL ou o PostgreSQL local do Compose, com as credenciais
// do ambiente ou do .env local (ver internal/testenv).
func testDatabaseURL() string { return testenv.DatabaseURL() }

func TestNewPool_ConnectsToRealPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, testDatabaseURL(), 5*time.Second)
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	defer pool.Close()

	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 = %d", one)
	}
}

func TestNewPool_DatabaseUnavailable(t *testing.T) {
	// Reserva uma porta livre e a libera: nada escuta nela (connection refused).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	url := fmt.Sprintf("postgres://jungle:x@%s/jungle?sslmode=disable", addr)
	start := time.Now()
	pool, err := NewPool(context.Background(), url, 3*time.Second)
	if err == nil {
		pool.Close()
		t.Fatal("NewPool() error = nil, want connection failure")
	}
	if pool != nil {
		t.Error("pool must be nil on failure")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("failure took %v, want fast failure", elapsed)
	}
}

func TestNewPool_ConnectTimeout(t *testing.T) {
	// Servidor TCP que aceita conexões mas nunca responde ao handshake do PostgreSQL.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		var conns []net.Conn
		defer func() {
			for _, c := range conns {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c)
		}
	}()
	defer ln.Close()

	url := fmt.Sprintf("postgres://jungle:x@%s/jungle?sslmode=disable", ln.Addr())
	start := time.Now()
	pool, err := NewPool(context.Background(), url, 300*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		pool.Close()
		t.Fatal("NewPool() error = nil, want timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed < 250*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want about 300ms", elapsed)
	}
}

func TestNewPool_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pool, err := NewPool(ctx, testDatabaseURL(), 5*time.Second)
	if err == nil {
		pool.Close()
		t.Fatal("NewPool() error = nil, want context canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestNewPool_InvalidURL(t *testing.T) {
	if _, err := NewPool(context.Background(), "://bad", time.Second); err == nil {
		t.Fatal("NewPool() error = nil, want parse error")
	}
}

func TestPool_CloseReleasesConnections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, testDatabaseURL(), 5*time.Second)
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	if got := pool.Stat().TotalConns(); got < 1 {
		t.Fatalf("TotalConns before Close = %d, want >= 1", got)
	}

	done := make(chan struct{})
	go func() { pool.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pool.Close() did not return in time")
	}

	if got := pool.Stat().TotalConns(); got != 0 {
		t.Errorf("TotalConns after Close = %d, want 0", got)
	}
	if err := pool.Ping(ctx); err == nil {
		t.Error("Ping after Close succeeded, want error")
	}
}
