package worker

import (
	"context"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
	"github.com/tkosudi/jungle-gaming-challenge/internal/postgres"
	"github.com/tkosudi/jungle-gaming-challenge/internal/sqs"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

// Module fornece o cliente SQS e registra os três laços. No start, cada um resolve suas filas
// antes de iniciar (fila ausente falha o startup); no stop, na ordem inversa, cada laço para
// de buscar trabalho e termina o que está em andamento.
var Module = fx.Module("worker",
	fx.Provide(newSQSClient),
	fx.Invoke(registerConsumer, registerPublisher, registerReferences),
)

func newSQSClient(cfg config.Config) (*awssqs.Client, error) {
	return sqs.NewClient(context.Background(), cfg.SQS)
}

func registerConsumer(lc fx.Lifecycle, client *awssqs.Client, service *usecase.Service, cfg config.Config) {
	c := NewConsumer(client, service, cfg.SQS)
	runInBackground(lc, "consumer", c.start, c.run)
}

func registerPublisher(lc fx.Lifecycle, store *postgres.Store, client *awssqs.Client, cfg config.Config) {
	p := NewPublisher(store, client, cfg)
	runInBackground(lc, "publisher", p.start, p.run)
}

func registerReferences(lc fx.Lifecycle, service *usecase.Service, cfg config.Config) {
	runInBackground(lc, "references", func(context.Context) error { return nil }, func(ctx context.Context) {
		runReferences(ctx, service, cfg.Workers.ReferencePollInterval)
	})
}
