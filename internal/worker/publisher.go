package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
	"github.com/tkosudi/jungle-gaming-challenge/internal/metrics"
	"github.com/tkosudi/jungle-gaming-challenge/internal/postgres"
	"github.com/tkosudi/jungle-gaming-challenge/internal/sqs"
)

const (
	publishBatch     = 10
	publishTimeout   = 30 * time.Second
	publishBaseDelay = time.Second
	publishMaxDelay  = time.Minute
)

// Publisher envia os eventos da outbox para a fila FIFO de eventos. Vários publishers (uma
// por instância) disputam a outbox sem lock global: cada lote trava só as cabeças de grupo
// que escolheu. A entrega é at-least-once; o eventId (MessageDeduplicationId) se repete nas
// republicações.
type Publisher struct {
	store    *postgres.Store
	client   *awssqs.Client
	queue    string
	interval time.Duration

	eventsURL string
}

func NewPublisher(store *postgres.Store, client *awssqs.Client, cfg config.Config) *Publisher {
	return &Publisher{store: store, client: client, queue: cfg.SQS.EventsQueue, interval: cfg.Workers.OutboxPollInterval}
}

func (p *Publisher) start(ctx context.Context) error {
	var err error
	p.eventsURL, err = sqs.QueueURL(ctx, p.client, p.queue)
	return err
}

func (p *Publisher) run(ctx context.Context) {
	for ctx.Err() == nil {
		res, err := p.publishBatch(ctx)
		p.observeBacklog(ctx)
		if err != nil {
			metrics.WorkerErrors.WithLabelValues("publisher").Inc()
			metrics.OutboxPublishFailures.Inc()
			slog.Error("outbox batch failed; its events stay pending", "error", err)
		}
		if err != nil || res.Published+res.Failed == 0 {
			sleep(ctx, p.interval)
		}
	}
}

// publishBatch termina o lote em andamento mesmo durante o shutdown: o contexto do laço não o
// interrompe no meio do envio.
func (p *Publisher) publishBatch(ctx context.Context) (postgres.PublishResult, error) {
	ctx, cancel := workContext(ctx, publishTimeout)
	defer cancel()
	res, err := p.store.PublishOutbox(ctx, publishBatch, retryDelay, p.send)
	metrics.OutboxPublished.Add(float64(res.Published))
	metrics.OutboxPublishFailures.Add(float64(res.Failed))
	return res, err
}

func (p *Publisher) send(ctx context.Context, m postgres.OutboxMessage) error {
	_, err := p.client.SendMessage(ctx, &awssqs.SendMessageInput{
		QueueUrl:               aws.String(p.eventsURL),
		MessageBody:            aws.String(string(m.Payload)),
		MessageGroupId:         aws.String(m.MessageGroupID),
		MessageDeduplicationId: aws.String(m.EventID.String()),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"eventType": {DataType: aws.String("String"), StringValue: aws.String(m.EventType)},
			"eventId":   {DataType: aws.String("String"), StringValue: aws.String(m.EventID.String())},
		},
	})
	if err != nil {
		slog.Warn("outbox event send failed; will retry", "eventId", m.EventID.String(), "eventType", m.EventType,
			"messageGroupId", m.MessageGroupID, "attempts", m.Attempts, "error", err)
	}
	return err
}

func (p *Publisher) observeBacklog(ctx context.Context) {
	ctx, cancel := workContext(ctx, sqsCallTimeout)
	defer cancel()
	oldest, pending, err := p.store.OutboxBacklog(ctx)
	if err != nil {
		return
	}
	metrics.OutboxOldestPendingAge.Set(oldest.Seconds())
	metrics.OutboxPending.Set(float64(pending))
}

func retryDelay(attempts int) time.Duration {
	delay := publishBaseDelay
	for i := 0; i < attempts && delay < publishMaxDelay; i++ {
		delay *= 2
	}
	return min(delay, publishMaxDelay)
}
