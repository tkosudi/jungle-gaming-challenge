package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"github.com/gofiber/fiber/v3"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
)

// Server serve a aplicação Fiber em um listener TCP. Abrir o listener (listen)
// e atender conexões (serve) são passos separados: o primeiro é síncrono e
// reporta porta ocupada; o segundo bloqueia e fica a cargo de quem chama.
type Server struct {
	addr string
	app  *fiber.App
	ln   net.Listener

	stopping atomic.Bool // true quando o shutdown foi pedido de propósito
}

func NewServer(cfg config.Config, app *fiber.App) *Server {
	return &Server{addr: cfg.HTTPAddr, app: app}
}

// Addr devolve o endereço efetivo do listener, ou nil se ele ainda não foi aberto.
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

func (s *Server) listen(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("httpapi: listen on %q: %w", s.addr, err)
	}
	s.ln = ln
	return nil
}

// serve bloqueia até o servidor encerrar. Retorna nil apenas após shutdown
// intencional, inclusive quando ele ocorre antes de o serve começar. O Fiber
// devolve nil também se o listener for fechado por fora; por isso o retorno é
// comparado com a flag, e esse caso vira erro.
func (s *Server) serve() error {
	err := s.app.Listener(s.ln, fiber.ListenConfig{DisableStartupMessage: true})
	if s.stopping.Load() {
		if err == nil || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
	if err == nil {
		return errors.New("httpapi: listener closed unexpectedly")
	}
	return err
}

func (s *Server) shutdown(ctx context.Context) error {
	s.stopping.Store(true)
	err := s.app.ShutdownWithContext(ctx)
	// Se o shutdown chegar antes de o serve registrar o listener, o Fiber não o
	// fecha e o serve ficaria bloqueado para sempre. Fechar de novo é inofensivo.
	if closeErr := s.ln.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		err = errors.Join(err, closeErr)
	}
	return err
}
