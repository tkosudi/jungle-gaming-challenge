package httpapi

import (
	"context"

	"go.uber.org/fx"
)

// Module registra a aplicação Fiber e o servidor HTTP no ciclo de vida do Fx. NewApp recebe
// o Service, o verificador de tokens e o Pinger do banco, fornecidos pela composição (app).
var Module = fx.Module("httpapi",
	fx.Provide(NewApp, NewServer),
	fx.Invoke(registerHooks),
)

func registerHooks(lc fx.Lifecycle, shutdowner fx.Shutdowner, s *Server) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// Síncrono: porta ocupada ou endereço inválido falham o startup.
			if err := s.listen(ctx); err != nil {
				return err
			}
			// O serve bloqueia, então roda em goroutine para não travar o startup.
			go func() {
				// serve retorna nil no shutdown intencional; erro aqui é falha
				// inesperada e derruba a aplicação com código 1.
				if err := s.serve(); err != nil {
					_ = shutdowner.Shutdown(fx.ExitCode(1))
				}
			}()
			return nil
		},
		OnStop: s.shutdown,
	})
}
