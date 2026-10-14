#!/bin/sh
set -eu

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

cat > /tmp/wager-redrive.json <<EOF
{
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"$DLQ_ARN\",\"maxReceiveCount\":\"5\"}"
}
EOF

awslocal sqs set-queue-attributes \
  --queue-url "$MAIN_URL" \
  --attributes file:///tmp/wager-redrive.json

awslocal sqs create-queue \
  --queue-name wager-events.fifo \
  --attributes FifoQueue=true

echo "Jungle Gaming SQS queues provisioned."
