// Package worker contém os laços de fundo: consumidor SQS, publicador da outbox e worker de
// referências pendentes, todos sob o ciclo de vida do Fx.
package worker

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/fx"
)

// runInBackground registra um laço no ciclo de vida: OnStart dispara a goroutine; OnStop
// cancela o contexto do laço (que para de buscar trabalho novo) e espera ele terminar o que
// está em andamento, no limite do prazo de shutdown do Fx.
func runInBackground(lc fx.Lifecycle, name string, start func(context.Context) error, run func(context.Context)) {
	var (
		cancel context.CancelFunc
		done   chan struct{}
	)
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// Preparação síncrona (resolver filas): falha aqui interrompe o startup.
			if err := start(ctx); err != nil {
				return fmt.Errorf("worker %s: %w", name, err)
			}
			var loopCtx context.Context
			loopCtx, cancel = context.WithCancel(context.Background())
			done = make(chan struct{})
			go func() {
				defer close(done)
				run(loopCtx)
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			cancel()
			select {
			case <-done:
				return nil
			case <-ctx.Done():
				return fmt.Errorf("worker %s did not stop in time: %w", name, ctx.Err())
			}
		},
	})
}

// sleep espera d ou o cancelamento; devolve false se o contexto acabou.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// workContext é o contexto de uma unidade de trabalho já iniciada: não é cancelado pelo
// shutdown (a transação termina ou é desfeita), mas tem prazo próprio.
func workContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// MaxStopDuration é o pior caso de parada dos três laços, que param em sequência: cada um
// termina a unidade de trabalho em andamento (sem cancelá-la no meio) antes de sair.
//   - consumidor: long polling em curso, uma mensagem e o DeleteMessage/ChangeMessageVisibility;
//   - publicador: um lote e a leitura do atraso da outbox;
//   - referências: um passo.
const MaxStopDuration = (receiveWaitSeconds*time.Second + sqsCallTimeout + messageTimeout + sqsCallTimeout) +
	(publishTimeout + sqsCallTimeout) +
	referenceStepTimeout
