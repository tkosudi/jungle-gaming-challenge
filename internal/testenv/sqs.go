package testenv

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
	"github.com/tkosudi/jungle-gaming-challenge/internal/sqs"
)

// Queues são as filas exclusivas de um teste, já com os endereços resolvidos.
type Queues struct {
	Config                                config.SQS
	Client                                *awssqs.Client
	RequestsURL, DeadLetterURL, EventsURL string
}

// SQSQueues cria no LocalStack filas FIFO só deste teste (entrada com redrive para a DLQ
// após maxReceiveCount recebimentos, DLQ e eventos) e as remove no fim. As credenciais AWS
// vêm do ambiente ou do .env (o LocalStack aceita qualquer segredo).
func SQSQueues(t *testing.T, maxReceiveCount int) Queues {
	t.Helper()
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		v := Lookup(key)
		if v == "" {
			t.Fatalf("%s is not set: define it in the environment or in .env (see .env.example)", key)
		}
		t.Setenv(key, v)
	}
	endpoint := Lookup("TEST_SQS_ENDPOINT")
	if endpoint == "" {
		port := Lookup("LOCALSTACK_PORT")
		if port == "" {
			port = "4566"
		}
		endpoint = "http://localhost:" + port
	}
	region := Lookup("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	prefix := "t" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	cfg := config.SQS{
		Region: region, EndpointURL: endpoint,
		RequestsQueue: prefix + "-requests.fifo", DeadLetterQueue: prefix + "-dlq.fifo", EventsQueue: prefix + "-events.fifo",
		ConsumerName: "wager-transactions-consumer", TrustedProviders: []string{"provider-a", "provider-b"},
		RetryBaseDelay: time.Second, RetryMaxDelay: 4 * time.Second,
	}
	ctx := context.Background()
	client, err := sqs.NewClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	q := Queues{Config: cfg, Client: client}
	create := func(name string, attrs map[string]string) string {
		attrs["FifoQueue"] = "true"
		out, err := client.CreateQueue(ctx, &awssqs.CreateQueueInput{QueueName: aws.String(name), Attributes: attrs})
		if err != nil {
			t.Fatalf("create queue %s: %v", name, err)
		}
		url := aws.ToString(out.QueueUrl)
		t.Cleanup(func() {
			_, _ = client.DeleteQueue(context.Background(), &awssqs.DeleteQueueInput{QueueUrl: aws.String(url)})
		})
		return url
	}
	q.DeadLetterURL = create(cfg.DeadLetterQueue, map[string]string{})
	attrs, err := client.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
		QueueUrl: aws.String(q.DeadLetterURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatal(err)
	}
	q.RequestsURL = create(cfg.RequestsQueue, map[string]string{
		"VisibilityTimeout": "60",
		"RedrivePolicy": fmt.Sprintf(`{"deadLetterTargetArn":"%s","maxReceiveCount":"%d"}`,
			attrs.Attributes[string(types.QueueAttributeNameQueueArn)], maxReceiveCount),
	})
	q.EventsURL = create(cfg.EventsQueue, map[string]string{})
	return q
}

// Send publica uma mensagem na fila de entrada. dedupID distinto permite reenviar o mesmo
// conteúdo sem a deduplicação de 5 minutos do SQS FIFO (simula reenvio do produtor).
func (q Queues) Send(t *testing.T, body, group, dedupID string) {
	t.Helper()
	_, err := q.Client.SendMessage(context.Background(), &awssqs.SendMessageInput{
		QueueUrl: aws.String(q.RequestsURL), MessageBody: aws.String(body),
		MessageGroupId: aws.String(group), MessageDeduplicationId: aws.String(dedupID),
	})
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
}

// Drain recebe e apaga tudo o que estiver visível na fila, na ordem de entrega.
func (q Queues) Drain(t *testing.T, url string, wait time.Duration) []types.Message {
	t.Helper()
	var all []types.Message
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		out, err := q.Client.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
			QueueUrl: aws.String(url), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageAttributeNames:       []string{"All"},
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
		})
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		for _, m := range out.Messages {
			all = append(all, m)
			if _, err := q.Client.DeleteMessage(context.Background(), &awssqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: m.ReceiptHandle}); err != nil {
				t.Fatalf("delete: %v", err)
			}
		}
	}
	return all
}

// Depth devolve as mensagens ainda na fila: visíveis mais em voo.
func (q Queues) Depth(t *testing.T, url string) int {
	t.Helper()
	out, err := q.Client.GetQueueAttributes(context.Background(), &awssqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		t.Fatalf("queue attributes: %v", err)
	}
	var visible, inFlight int
	fmt.Sscan(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)], &visible)
	fmt.Sscan(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)], &inFlight)
	return visible + inFlight
}
