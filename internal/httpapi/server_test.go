package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
)

func newTestFxApp(t *testing.T, addr string, srv **Server) *fx.App {
	t.Helper()
	return fx.New(
		fx.NopLogger,
		fx.Supply(config.Config{HTTPAddr: addr}),
		Module,
		fx.Populate(srv),
	)
}

func ctxWithTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestServer_StartServesRequestsAndStops(t *testing.T) {
	var srv *Server
	app := newTestFxApp(t, "127.0.0.1:0", &srv)
	if err := app.Err(); err != nil {
		t.Fatalf("fx.New() error = %v", err)
	}

	start := time.Now()
	if err := app.Start(ctxWithTimeout(t, 5*time.Second)); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Start() took %v; the server must not block startup", elapsed)
	}
	addr := srv.Addr().String()

	// Disponível imediatamente após o startup, sem autenticação.
	resp, err := http.Get(fmt.Sprintf("http://%s/health/live", addr))
	if err != nil {
		t.Fatalf("GET /health/live: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	stopStart := time.Now()
	if err := app.Stop(ctxWithTimeout(t, 5*time.Second)); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if elapsed := time.Since(stopStart); elapsed > 3*time.Second {
		t.Errorf("Stop() took %v, want bounded shutdown", elapsed)
	}

	// Depois do shutdown, o listener foi liberado.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("server still accepting connections after Stop()")
	}
}

func TestServer_StartFailsWhenPortInUse(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	var srv *Server
	app := newTestFxApp(t, busy.Addr().String(), &srv)
	if err := app.Err(); err != nil {
		t.Fatalf("fx.New() error = %v", err)
	}

	err = app.Start(ctxWithTimeout(t, 5*time.Second))
	if err == nil {
		_ = app.Stop(context.Background())
		t.Fatal("Start() error = nil, want address in use")
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("Start() error = %v, want errors.Is(EADDRINUSE)", err)
	}
}

// Garante que o shutdown intencional não é visto como falha do serve; do
// contrário o módulo acionaria o Shutdowner com código 1 durante o Stop.
func TestServer_ServeReturnsNilAfterShutdown(t *testing.T) {
	s := NewServer(config.Config{HTTPAddr: "127.0.0.1:0"}, NewApp())
	if err := s.listen(ctxWithTimeout(t, 5*time.Second)); err != nil {
		t.Fatalf("listen() error = %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- s.serve() }()

	// Uma resposta HTTP prova que o serve está ativo (um Dial conectaria no
	// backlog do kernel mesmo antes de o serve começar).
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://%s/health/live", s.Addr()))
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not respond: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := s.shutdown(ctxWithTimeout(t, 5*time.Second)); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve() after intentional shutdown = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() did not return after shutdown")
	}
}

// Shutdown imediatamente após iniciar o serve (Stop logo depois do Start) não
// pode deixar o serve bloqueado nem o listener aberto.
func TestServer_ShutdownBeforeServeRegistersListener(t *testing.T) {
	s := NewServer(config.Config{HTTPAddr: "127.0.0.1:0"}, NewApp())
	if err := s.listen(ctxWithTimeout(t, 5*time.Second)); err != nil {
		t.Fatalf("listen() error = %v", err)
	}
	addr := s.Addr().String()

	served := make(chan error, 1)
	go func() { served <- s.serve() }()
	if err := s.shutdown(ctxWithTimeout(t, 5*time.Second)); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve() did not return: listener leaked after early shutdown")
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Error("listener still open after shutdown")
	}
}

// Fechamento inesperado do listener (sem shutdown) não pode ser tratado como
// encerramento normal: o serve deve reportar erro para o módulo derrubar a app.
func TestServer_ServeReportsUnexpectedListenerClose(t *testing.T) {
	s := NewServer(config.Config{HTTPAddr: "127.0.0.1:0"}, NewApp())
	if err := s.listen(ctxWithTimeout(t, 5*time.Second)); err != nil {
		t.Fatalf("listen() error = %v", err)
	}
	t.Cleanup(func() { _ = s.shutdown(context.Background()) })

	served := make(chan error, 1)
	go func() { served <- s.serve() }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://%s/health/live", s.Addr()))
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not respond: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	_ = s.ln.Close() // fechamento externo, sem shutdown()

	select {
	case err := <-served:
		if err == nil {
			t.Fatal("serve() = nil after unexpected listener close, want error")
		}
		t.Logf("serve() returned: %v (errors.Is ErrClosed=%v)", err, errors.Is(err, net.ErrClosed))
	case <-time.After(3 * time.Second):
		t.Fatal("serve() did not return after listener close")
	}
}
