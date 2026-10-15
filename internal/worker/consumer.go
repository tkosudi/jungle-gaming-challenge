package worker

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
	"github.com/tkosudi/jungle-gaming-challenge/internal/metrics"
	"github.com/tkosudi/jungle-gaming-challenge/internal/sqs"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

const (
	// receiveWaitSeconds limita o long polling e, com ele, quanto o shutdown espera.
	receiveWaitSeconds = 5
	// Uma mensagem por recebimento: o visibility timeout (60s) conta desde o ReceiveMessage, então
	// num lote as últimas esperariam o tratamento das anteriores e poderiam reaparecer antes de
	// terminar. Com uma só, o prazo cobre exatamente o seu tratamento. Também mantém a ordem do
	// grupo FIFO: enquanto uma mensagem está em voo (inclusive aguardando nova tentativa), o SQS
	// não entrega a seguinte do mesmo grupo. A vazão vem de grupos distintos e de mais instâncias.
	receiveBatch = 1
	// messageTimeout limita o tratamento de uma mensagem; fica abaixo do visibility timeout da
	// fila (60s) para que a mensagem não reapareça enquanto ainda é processada.
	messageTimeout = 30 * time.Second
	sqsCallTimeout = 5 * time.Second
	receiveBackoff = time.Second
)

// Motivos estáveis de envio à DLQ (atributo failureReason da mensagem e rótulo da métrica).
const (
	reasonInvalidMessage      = "INVALID_MESSAGE"
	reasonUntrustedProvider   = "UNTRUSTED_PROVIDER"
	reasonMessageConflict     = "MESSAGE_CONFLICT"
	reasonIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
)

// Consumer lê wager-transactions.fifo e aplica cada mensagem com Service.ProcessMessage.
//
// Destino de cada mensagem:
//   - concluída (processada, rejeitada, pendente ou duplicata): DeleteMessage, só depois do commit;
//   - recusa permanente (contrato, provedor fora da fronteira de confiança, conflito):
//     cópia para a DLQ com o motivo e DeleteMessage;
//   - falha transitória (banco, lock, commit incerto, erro inesperado): fica na fila e volta
//     após um atraso exponencial; esgotado o maxReceiveCount, o redrive a leva à DLQ.
//
// Se o processo cai depois do commit e antes do DeleteMessage, a reentrega encontra a inbox
// concluída e só apaga a mensagem.
type Consumer struct {
	client  *awssqs.Client
	service *usecase.Service
	cfg     config.SQS
	trusted map[string]bool

	requestsURL, deadLetterURL string
}

func NewConsumer(client *awssqs.Client, service *usecase.Service, cfg config.SQS) *Consumer {
	trusted := make(map[string]bool, len(cfg.TrustedProviders))
	for _, p := range cfg.TrustedProviders {
		trusted[p] = true
	}
	return &Consumer{client: client, service: service, cfg: cfg, trusted: trusted}
}

func (c *Consumer) start(ctx context.Context) error {
	var err error
	if c.requestsURL, err = sqs.QueueURL(ctx, c.client, c.cfg.RequestsQueue); err != nil {
		return err
	}
	c.deadLetterURL, err = sqs.QueueURL(ctx, c.client, c.cfg.DeadLetterQueue)
	return err
}

// run busca mensagens até o contexto ser cancelado. O ReceiveMessage em curso NÃO é cancelado:
// abandonar um long polling no meio pode fazer o broker entregar mensagens a uma requisição
// que ninguém lê, e elas ficariam invisíveis até o visibility timeout. Ele termina (no máximo
// receiveWaitSeconds); a mensagem em tratamento termina, e uma recebida durante o shutdown
// volta à fila sem ser tratada.
func (c *Consumer) run(ctx context.Context) {
	for ctx.Err() == nil {
		recvCtx, cancel := workContext(ctx, receiveWaitSeconds*time.Second+sqsCallTimeout)
		out, err := c.client.ReceiveMessage(recvCtx, &awssqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.requestsURL),
			MaxNumberOfMessages: receiveBatch,
			WaitTimeSeconds:     receiveWaitSeconds,
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameApproximateReceiveCount,
				types.MessageSystemAttributeNameApproximateFirstReceiveTimestamp,
				types.MessageSystemAttributeNameMessageGroupId,
			},
		})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			metrics.WorkerErrors.WithLabelValues("consumer").Inc()
			slog.Error("sqs receive failed", "queue", c.cfg.RequestsQueue, "error", err)
			sleep(ctx, receiveBackoff)
			continue
		}
		for _, m := range out.Messages {
			if ctx.Err() != nil {
				c.release(m)
				continue
			}
			c.handle(ctx, m)
		}
	}
}

// handle trata uma mensagem e decide seu destino: apagar, DLQ ou nova tentativa.
func (c *Consumer) handle(ctx context.Context, m types.Message) {
	ctx, cancel := workContext(ctx, messageTimeout)
	defer cancel()
	sqsID := aws.ToString(m.MessageId)

	req, err := sqs.ParseRequest([]byte(aws.ToString(m.Body)))
	if err != nil {
		c.deadLetter(ctx, m, reasonInvalidMessage, err)
		return
	}
	log := slog.With("messageId", req.MessageID, "sqsMessageId", sqsID, "providerId", req.ProviderID,
		"walletId", req.Operation.WalletID.String())
	// O SQS não diz quem enviou cada mensagem: providerId só é aceito se estiver dentro da
	// fronteira de confiança configurada para esta fila (ver ARCHITECTURE.md).
	if !c.trusted[req.ProviderID] {
		c.deadLetter(ctx, m, reasonUntrustedProvider, errors.New("provider is not trusted on this queue"))
		return
	}

	res, err := c.service.ProcessMessage(ctx, usecase.InboxMessage{
		ConsumerName: c.cfg.ConsumerName, MessageID: req.MessageID, PayloadHash: req.ContentHash,
		ReceivedAt: firstReceive(m),
	}, req.ProviderID, req.Operation)
	switch {
	case errors.Is(err, usecase.ErrMessageConflict):
		c.deadLetter(ctx, m, reasonMessageConflict, err)
		return
	case errors.Is(err, usecase.ErrIdempotencyConflict):
		c.deadLetter(ctx, m, reasonIdempotencyConflict, err)
		return
	case errors.Is(err, domain.ErrInvalidOperation), errors.Is(err, usecase.ErrInvalidProvider):
		c.deadLetter(ctx, m, reasonInvalidMessage, err)
		return
	case err != nil:
		c.retry(m, log, err)
		return
	}

	switch {
	case res.Duplicate:
		metrics.Duplicates.WithLabelValues("sqs_inbox").Inc()
	case res.Replay:
		metrics.Duplicates.WithLabelValues("sqs").Inc()
	default:
		metrics.Operations.WithLabelValues("sqs", string(res.Status)).Inc()
	}
	log.Info("sqs message completed", "transactionId", res.TransactionID.String(), "status", string(res.Status),
		"duplicate", res.Duplicate, "replay", res.Replay)
	c.delete(m, log)
	metrics.SQSMessages.WithLabelValues("completed").Inc()
}

// retry devolve a mensagem à fila com atraso exponencial pelo número de recebimentos.
func (c *Consumer) retry(m types.Message, log *slog.Logger, cause error) {
	receives, _ := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
	delay := c.cfg.RetryBaseDelay
	for i := 1; i < receives && delay < c.cfg.RetryMaxDelay; i++ {
		delay *= 2
	}
	delay = min(delay, c.cfg.RetryMaxDelay)
	log.Warn("sqs message will be retried", "receiveCount", receives, "retryIn", delay.String(), "error", cause)
	metrics.SQSMessages.WithLabelValues("retried").Inc()
	c.changeVisibility(m, delay)
}

// deadLetter copia a mensagem para a DLQ com o motivo e só então a apaga da fila de entrada.
// Se a cópia falhar, a mensagem fica para nova tentativa (e o redrive é o destino final).
func (c *Consumer) deadLetter(ctx context.Context, m types.Message, reason string, cause error) {
	sqsID := aws.ToString(m.MessageId)
	group := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
	if group == "" {
		group = "unknown"
	}
	log := slog.With("sqsMessageId", sqsID, "reason", reason)
	_, err := c.client.SendMessage(ctx, &awssqs.SendMessageInput{
		QueueUrl:               aws.String(c.deadLetterURL),
		MessageBody:            m.Body,
		MessageGroupId:         aws.String(group),
		MessageDeduplicationId: aws.String(sqsID),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"failureReason":   {DataType: aws.String("String"), StringValue: aws.String(reason)},
			"sourceMessageId": {DataType: aws.String("String"), StringValue: aws.String(sqsID)},
		},
	})
	if err != nil {
		c.retry(m, log, errors.Join(cause, err))
		return
	}
	log.Warn("sqs message dead-lettered", "error", cause)
	metrics.DeadLettered.WithLabelValues(reason).Inc()
	metrics.SQSMessages.WithLabelValues("dead_lettered").Inc()
	c.delete(m, log)
}

// delete remove a mensagem tratada. Se falhar, a reentrega cai na inbox como duplicata.
func (c *Consumer) delete(m types.Message, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), sqsCallTimeout)
	defer cancel()
	if _, err := c.client.DeleteMessage(ctx, &awssqs.DeleteMessageInput{
		QueueUrl: aws.String(c.requestsURL), ReceiptHandle: m.ReceiptHandle,
	}); err != nil {
		metrics.WorkerErrors.WithLabelValues("consumer").Inc()
		log.Error("sqs delete failed; the redelivery will be a duplicate", "error", err)
	}
}

// release devolve à fila, sem tratar, uma mensagem recebida durante o shutdown.
func (c *Consumer) release(m types.Message) {
	metrics.SQSMessages.WithLabelValues("released").Inc()
	c.changeVisibility(m, 0)
}

func (c *Consumer) changeVisibility(m types.Message, delay time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), sqsCallTimeout)
	defer cancel()
	seconds := int32((delay + time.Second - 1) / time.Second)
	if _, err := c.client.ChangeMessageVisibility(ctx, &awssqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(c.requestsURL), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: seconds,
	}); err != nil {
		// Sem a mudança, a mensagem volta quando o visibility timeout da fila expirar.
		metrics.WorkerErrors.WithLabelValues("consumer").Inc()
		slog.Error("sqs change visibility failed", "sqsMessageId", aws.ToString(m.MessageId), "error", err)
	}
}

func firstReceive(m types.Message) time.Time {
	ms, err := strconv.ParseInt(m.Attributes[string(types.MessageSystemAttributeNameApproximateFirstReceiveTimestamp)], 10, 64)
	if err != nil {
		return time.Now().UTC()
	}
	return time.UnixMilli(ms).UTC()
}
