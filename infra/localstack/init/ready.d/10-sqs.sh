#!/bin/sh
set -eu

# Filas FIFO do desafio e suas políticas de acesso. O LocalStack Community grava as
# políticas, mas NÃO as aplica (não há IAM): elas documentam a fronteira de confiança que um
# ambiente AWS real precisa impor (ver ARCHITECTURE.md, M3).
ACCOUNT=000000000000
GATEWAY="arn:aws:iam::${ACCOUNT}:role/wager-provider-gateway"   # único produtor da entrada
API="arn:aws:iam::${ACCOUNT}:role/wager-api"                    # consumidor e publicador
EVENTS_CONSUMER="arn:aws:iam::${ACCOUNT}:role/wager-events-consumer"

echo "Provisioning Jungle Gaming SQS queues..."

DLQ_URL=$(awslocal sqs create-queue \
  --queue-name wager-transactions-dlq.fifo \
  --attributes FifoQueue=true \
  --query QueueUrl \
  --output text)

DLQ_ARN=$(awslocal sqs get-queue-attributes \
  --queue-url "$DLQ_URL" \
  --attribute-names QueueArn \
  --query 'Attributes.QueueArn' \
  --output text)

MAIN_URL=$(awslocal sqs create-queue \
  --queue-name wager-transactions.fifo \
  --attributes FifoQueue=true,VisibilityTimeout=60 \
  --query QueueUrl \
  --output text)
MAIN_ARN="arn:aws:sqs:us-east-1:${ACCOUNT}:wager-transactions.fifo"

EVENTS_URL=$(awslocal sqs create-queue \
  --queue-name wager-events.fifo \
  --attributes FifoQueue=true \
  --query QueueUrl \
  --output text)
EVENTS_ARN="arn:aws:sqs:us-east-1:${ACCOUNT}:wager-events.fifo"

# Redrive: depois de 5 recebimentos sem conclusão, a mensagem vai para a DLQ.
cat > /tmp/wager-main.json <<JSON
{
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"$DLQ_ARN\",\"maxReceiveCount\":\"5\"}",
  "Policy": "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Sid\":\"GatewaySends\",\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"$GATEWAY\"},\"Action\":\"sqs:SendMessage\",\"Resource\":\"$MAIN_ARN\"},{\"Sid\":\"ApiConsumes\",\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"$API\"},\"Action\":[\"sqs:ReceiveMessage\",\"sqs:DeleteMessage\",\"sqs:ChangeMessageVisibility\",\"sqs:GetQueueUrl\",\"sqs:GetQueueAttributes\"],\"Resource\":\"$MAIN_ARN\"}]}"
}
JSON
awslocal sqs set-queue-attributes --queue-url "$MAIN_URL" --attributes file:///tmp/wager-main.json

cat > /tmp/wager-dlq.json <<JSON
{
  "Policy": "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Sid\":\"ApiDeadLetters\",\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"$API\"},\"Action\":[\"sqs:SendMessage\",\"sqs:GetQueueUrl\"],\"Resource\":\"$DLQ_ARN\"}]}"
}
JSON
awslocal sqs set-queue-attributes --queue-url "$DLQ_URL" --attributes file:///tmp/wager-dlq.json

cat > /tmp/wager-events.json <<JSON
{
  "Policy": "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Sid\":\"ApiPublishes\",\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"$API\"},\"Action\":[\"sqs:SendMessage\",\"sqs:GetQueueUrl\"],\"Resource\":\"$EVENTS_ARN\"},{\"Sid\":\"DownstreamConsumes\",\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"$EVENTS_CONSUMER\"},\"Action\":[\"sqs:ReceiveMessage\",\"sqs:DeleteMessage\",\"sqs:ChangeMessageVisibility\"],\"Resource\":\"$EVENTS_ARN\"}]}"
}
JSON
awslocal sqs set-queue-attributes --queue-url "$EVENTS_URL" --attributes file:///tmp/wager-events.json

echo "Jungle Gaming SQS queues provisioned."
