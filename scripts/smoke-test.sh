#!/usr/bin/env bash
# Smoke test da stack do Compose em execução (docker compose up -d --build --wait, migrations
# aplicadas com docker compose run --rm migrate up): health checks, tokens reais do Keycloak,
# carteira, aposta com replay, isolamento entre provedores, mensagem SQS, DLQ, eventos da
# outbox, reconciliação e métricas. Requer bash, curl, jq e o .env local.
set -euo pipefail
cd "$(dirname "$0")/.."
set -a
. ./.env
set +a

API="http://127.0.0.1:${APP_PORT:-8080}"
KEYCLOAK="http://localhost:${KEYCLOAK_PORT:-8180}"
QUEUE_BASE="http://localhost:4566/000000000000"
RUN="smoke-$(date +%s)-$RANDOM"
PLAYER=$(cat /proc/sys/kernel/random/uuid 2>/dev/null || uuidgen | tr 'A-Z' 'a-z')

pass() { printf 'ok   %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*" >&2; exit 1; }
# expect STATUS BODY_FILE JQ_FILTER: o status HTTP e o filtro jq (que deve valer true) conferem.
expect() {
  [ "$1" = "$2" ] || fail "$4: status $2, want $1: $(cat "$3")"
  jq -e "$5" "$3" >/dev/null || fail "$4: $(cat "$3")"
  pass "$4"
}
body=$(mktemp)
trap 'rm -f "$body"' EXIT
http() { curl -s -o "$body" -w '%{http_code}' "$@"; }
token() {
  curl -sf -d grant_type=client_credentials -d client_id="$1" --data-urlencode client_secret="$2" \
    "$KEYCLOAK/realms/jungle/protocol/openid-connect/token" | jq -r .access_token
}
sqs() { docker compose exec -T localstack awslocal sqs "$@"; }

expect 200 "$(http "$API/health/live")" "$body" "GET /health/live" '.status == "ok"'
expect 200 "$(http "$API/health/ready")" "$body" "GET /health/ready" '.checks.postgres == "ok" and .checks.sqs == "ok"'

INTERNAL=$(token internal-service "$INTERNAL_SERVICE_CLIENT_SECRET")
PROVIDER_A=$(token provider-a "$PROVIDER_A_CLIENT_SECRET")
PROVIDER_B=$(token provider-b "$PROVIDER_B_CLIENT_SECRET")
[ -n "$INTERNAL" ] && [ "$INTERNAL" != null ] && [ -n "$PROVIDER_A" ] && [ -n "$PROVIDER_B" ] || fail "Keycloak tokens"
pass "client_credentials tokens from Keycloak (internal-service, provider-a, provider-b)"

expect 401 "$(http -X POST "$API/wallets" -H 'Content-Type: application/json' -d '{}')" "$body" \
  "POST /wallets without a token" '.error.code == "UNAUTHENTICATED"'
expect 401 "$(http -X POST "$API/wallets" -H "Authorization: Bearer not-a-jwt" -H 'Content-Type: application/json' -d '{}')" "$body" \
  "POST /wallets with an invalid token" '.error.code == "UNAUTHENTICATED"'
expect 403 "$(http -X POST "$API/wallets" -H "Authorization: Bearer $PROVIDER_A" -H 'Content-Type: application/json' -d '{}')" "$body" \
  "POST /wallets with a provider token" '.error.code == "FORBIDDEN"'

expect 201 "$(http -X POST "$API/wallets" -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}")" "$body" \
  "POST /wallets" '.balance.amount == "100.00"'
WALLET=$(jq -r .id "$body")

operation() { # kind external amount
  printf '{"providerId":"provider-a","externalTransactionId":"%s","playerId":"%s","walletId":"%s","roundId":"round-1","gameId":"game-1","kind":"%s","money":{"amount":"%s","currency":"BRL"}}' \
    "$2" "$PLAYER" "$WALLET" "$1" "$3"
}
BET=$(operation BET "$RUN-bet-1" 25.00)
expect 200 "$(http -X POST "$API/wagering/transactions" -H "Authorization: Bearer $PROVIDER_A" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: provider-a:$RUN-bet-1" -d "$BET")" "$body" \
  "POST /wagering/transactions BET 25.00" '.status == "PROCESSED" and .balance.amount == "75.00" and .idempotentReplay == false'
TX=$(jq -r .transactionId "$body")
expect 200 "$(http -X POST "$API/wagering/transactions" -H "Authorization: Bearer $PROVIDER_A" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: provider-a:$RUN-bet-1" -d "$BET")" "$body" \
  "same BET again (replay)" ".transactionId == \"$TX\" and .idempotentReplay == true"
expect 404 "$(http "$API/wagering/transactions/$TX" -H "Authorization: Bearer $PROVIDER_B")" "$body" \
  "provider-b reading provider-a's transaction" '.error.code == "TRANSACTION_NOT_FOUND"'

# SQS: a mesma BET pela fila é replay (sem novo débito); outra BET é aplicada.
envelope() { # messageId kind external amount
  jq -cn --arg id "$1" --argjson data "$(operation "$2" "$3" "$4")" --arg key "provider-a:$3" \
    '{messageId:$id,type:"WagerTransactionRequested",occurredAt:(now|todate),data:($data + {idempotencyKey:$key})}'
}
sqs send-message --queue-url "$QUEUE_BASE/wager-transactions.fifo" --message-group-id "$WALLET" \
  --message-deduplication-id "$RUN-msg-1" --message-body "$(envelope "$RUN-msg-1" BET "$RUN-bet-1" 25.00)" >/dev/null
sqs send-message --queue-url "$QUEUE_BASE/wager-transactions.fifo" --message-group-id "$WALLET" \
  --message-deduplication-id "$RUN-msg-2" --message-body "$(envelope "$RUN-msg-2" BET "$RUN-bet-2" 10.00)" >/dev/null
for _ in $(seq 1 30); do
  http "$API/wallets/$WALLET" -H "Authorization: Bearer $INTERNAL" >/dev/null
  [ "$(jq -r .balance.amount "$body")" = "65.00" ] && break
  sleep 1
done
expect 200 "$(http "$API/wallets/$WALLET" -H "Authorization: Bearer $INTERNAL")" "$body" \
  "SQS: duplicate BET replayed, new BET 10.00 applied" '.balance.amount == "65.00"'

# DLQ: provedor fora da fronteira de confiança da fila.
sqs send-message --queue-url "$QUEUE_BASE/wager-transactions.fifo" --message-group-id "$WALLET" \
  --message-deduplication-id "$RUN-msg-3" \
  --message-body "$(envelope "$RUN-msg-3" BET "$RUN-bet-3" 1.00 | jq -c '.data.providerId = "provider-x"')" >/dev/null
reason=""
for _ in $(seq 1 30); do
  out=$(sqs receive-message --queue-url "$QUEUE_BASE/wager-transactions-dlq.fifo" --message-attribute-names All \
    --max-number-of-messages 10 --wait-time-seconds 1)
  reason=$(jq -r --arg id "$RUN-msg-3" '.Messages[]? | select((.Body | fromjson | .messageId) == $id) | .MessageAttributes.failureReason.StringValue' <<<"${out:-{\}}")
  [ -n "$reason" ] && break
done
[ "$reason" = "UNTRUSTED_PROVIDER" ] || fail "DLQ message with failureReason UNTRUSTED_PROVIDER (got '$reason')"
pass "SQS: untrusted provider sent to the DLQ with failureReason=$reason"

expect 200 "$(http -X POST "$API/wallets/$WALLET/reconciliation" -H "Authorization: Bearer $INTERNAL")" "$body" \
  "POST /wallets/{id}/reconciliation" '.consistent == true and .storedBalance.amount == "65.00" and .checkedEntries == 3'

# Outbox: os eventos desta carteira chegam a wager-events.fifo (sem apagar nada da fila).
events=0
for _ in $(seq 1 30); do
  http "$API/metrics" >/dev/null
  if grep -qx 'outbox_pending_events 0' "$body"; then
    events=$(sqs get-queue-attributes --queue-url "$QUEUE_BASE/wager-events.fifo" --attribute-names ApproximateNumberOfMessages |
      jq -r .Attributes.ApproximateNumberOfMessages)
    break
  fi
  sleep 1
done
[ "$events" -gt 0 ] || fail "outbox drained and events published"
pass "outbox drained (outbox_pending_events 0); wager-events.fifo holds $events events"

http "$API/metrics" >/dev/null
for series in 'wagering_operations_total{source="http",status="PROCESSED"}' 'wagering_operations_total{source="sqs",status="PROCESSED"}' \
  'wagering_duplicates_total{kind="http"}' 'wagering_duplicates_total{kind="sqs"}' 'sqs_dead_lettered_total{reason="UNTRUSTED_PROVIDER"}' \
  'wagering_processing_duration_seconds_count{source="http"}' 'wallet_reconciliations_total{result="consistent"}' \
  'outbox_events_published_total'; do
  grep -qF "$series " "$body" || fail "metric $series"
done
pass "GET /metrics exposes operations, duplicates, DLQ, latency, reconciliation and outbox series"
echo "smoke test passed (wallet $WALLET)"
