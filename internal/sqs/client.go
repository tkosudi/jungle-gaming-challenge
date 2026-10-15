package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
)

// NewClient cria o cliente SQS. Credenciais seguem a cadeia padrão do SDK (variáveis de
// ambiente, perfis); com EndpointURL o cliente fala com o LocalStack.
func NewClient(ctx context.Context, cfg config.SQS) (*awssqs.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("sqs: load AWS configuration: %w", err)
	}
	return awssqs.NewFromConfig(awsCfg, func(o *awssqs.Options) {
		if cfg.EndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.EndpointURL)
		}
	}), nil
}

// QueueURL resolve o endereço de uma fila pelo nome; falha se ela não existir.
func QueueURL(ctx context.Context, client *awssqs.Client, name string) (string, error) {
	out, err := client.GetQueueUrl(ctx, &awssqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return "", fmt.Errorf("sqs: resolve queue %q: %w", name, err)
	}
	return aws.ToString(out.QueueUrl), nil
}
