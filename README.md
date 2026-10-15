# Jungle Gaming Challenge — Backend de apostas em Go

Implementação do desafio técnico de processamento financeiro distribuído de apostas com API HTTP e mensagens SQS.

> **Status:** M0.1 a M1.3 (núcleo financeiro), M2/M2.1 (API HTTP com OIDC e reconciliação), M3 (consumidor SQS FIFO com inbox, publicador da outbox e workers) e M4 (teste com três processos reais, CI de integração e reprodução a partir de checkout limpo) implementados e testados. Não implementados: IAM real do broker (o LocalStack Community não aplica políticas) e separação de papéis do banco (D8). Detalhes e limitações em [`ARCHITECTURE.md`](ARCHITECTURE.md).

## Documentos

- [`docs/REQUISITOS.md`](docs/REQUISITOS.md): enunciado original (não editar).
- [`ARCHITECTURE.md`](ARCHITECTURE.md): decisões aprovadas, interpretações e limitações.
- [`CLAUDE.md`](CLAUDE.md): regras de execução das tarefas pelo Claude Code.

## Stack definida

Go 1.26+, Uber Fx, Fiber v3, PostgreSQL 17, pgx/v5, sqlc, golang-migrate v4.20.1 (biblioteca e CLI; migrations `.up.sql`/`.down.sql`), Keycloak 26.8.0, AWS SQS com LocalStack 4.6.0 e Docker Compose.

## Como executar

Disponível: a API HTTP autenticada (carteiras, operações BET/WIN/LOSS/REFUND/ROLLBACK, consultas e reconciliação), o consumidor da fila `wager-transactions.fifo` (com DLQ `wager-transactions-dlq.fifo`), o publicador da outbox em `wager-events.fifo`, o worker de referências pendentes, health checks, `GET /metrics` (Prometheus) e as migrations.

### Pré-requisitos

| Para | Precisa de |
| --- | --- |
| Subir a stack, aplicar migrations, enviar mensagens SQS | Docker e Docker Compose v2 (com `--wait`), `openssl` (gerar o `.env`) |
| Chamar a API e o smoke test | `bash`, `curl`, `jq` |
| Testes e teste distribuído | Go 1.26+ (`go.mod` declara `go 1.26.0`; o Dockerfile usa `golang:1.26-alpine`) |

### Configuração e serviços

O `.env` local (ignorado pelo Git) guarda as senhas e os client secrets. Gere-o com valores aleatórios a partir do `.env.example` (os valores não são impressos; o script recusa sobrescrever um `.env` existente sem `--force`):

```sh
./scripts/generate-env.sh                    # ou: cp .env.example .env e troque cada change-me-* (openssl rand -hex 24)
docker compose up -d --build --wait          # espera PostgreSQL, Keycloak, LocalStack e API ficarem saudáveis
docker compose run --rm migrate up           # aplica as migrations (ver abaixo)
curl -i http://127.0.0.1:8080/health/ready   # 200 {"status":"ok","checks":{"postgres":"ok","sqs":"ok"}}
```

- O Compose recusa subir sem as senhas e os client secrets (`${VAR:?}`). O Keycloak lê os secrets do ambiente ao importar o realm (placeholders `${...}` em `infra/keycloak/realm.json`); ao trocar um secret, recrie o Keycloak (`docker compose up -d --force-recreate keycloak`), porque ele só importa realms inexistentes.
- `keycloak` importa o realm `jungle` (clientes, papéis e identidades de teste). `localstack` cria as filas FIFO, o redrive (5 recebimentos) e as políticas em `infra/localstack/init/ready.d/10-sqs.sh`. Os healthchecks do Compose usam o `/health/ready` do Keycloak e da API e a existência das filas no LocalStack; `--wait` só retorna com tudo saudável.
- A API lê `DATABASE_URL`, `OIDC_ISSUER_URL` e `OIDC_AUDIENCE` (obrigatórias), `OIDC_JWKS_URL` (padrão: certificados do realm sob o issuer) e `HTTP_ADDR` (padrão `:8080`). No Compose, todas são definidas em `compose.yaml`.
- Variáveis de SQS e workers (todas com padrão, exceto `SQS_TRUSTED_PROVIDERS` e as credenciais AWS do `.env`): `AWS_REGION`, `AWS_ENDPOINT_URL`, `SQS_REQUESTS_QUEUE`, `SQS_DLQ_QUEUE`, `SQS_EVENTS_QUEUE`, `SQS_CONSUMER_NAME`, `SQS_RETRY_BASE_DELAY`, `SQS_RETRY_MAX_DELAY`, `REFERENCE_INITIAL_BACKOFF`, `REFERENCE_MAX_BACKOFF`, `REFERENCE_MAX_ATTEMPTS`, `REFERENCE_POLL_INTERVAL`, `OUTBOX_POLL_INTERVAL` (padrões em `internal/config`).
- Portas no host (todas em `127.0.0.1`, configuráveis no `.env`): API 8080, PostgreSQL 5432, Keycloak 8180, LocalStack 4566.
- As migrations **não** são aplicadas automaticamente pela API.

### Migrations

Arquivos em `db/migrations/`, aplicados com a CLI do golang-migrate v4.20.1 pelo serviço `migrate` do Compose (profile `tools`, que não sobe com `docker compose up`; usa a rede do Compose e as credenciais do `.env`):

```sh
docker compose run --rm migrate up          # aplica
docker compose run --rm migrate version     # 4
docker compose run --rm migrate down -all   # reverte tudo (destrutivo: apaga os dados financeiros)
```

Reverta só em ambiente descartável. Para um ambiente novo do zero: `docker compose down -v` apaga o volume do PostgreSQL. A reversão `up → down -all → up` com esta CLI foi executada em um PostgreSQL 17 descartável e deixou 0 tabelas da aplicação após o `down` (também roda na CI). Os testes de integração repetem `up → down → up` de cada migration com a biblioteca.

### API e autenticação

Tokens `client_credentials` do Keycloak local, com os client secrets do `.env`: `provider-a`, `provider-b` (papel `wagering-provider`, operações de apostas) e `internal-service` (papel `wallet-operator`, carteiras e reconciliação). Rotas, códigos e regras em `ARCHITECTURE.md` (seção M2).

```sh
set -a; . ./.env; set +a
token() { curl -s -d grant_type=client_credentials -d client_id="$1" --data-urlencode client_secret="$2" \
  http://localhost:8180/realms/jungle/protocol/openid-connect/token | jq -r .access_token; }
INTERNAL=$(token internal-service "$INTERNAL_SERVICE_CLIENT_SECRET")
PROVIDER=$(token provider-a "$PROVIDER_A_CLIENT_SECRET")

curl -s -X POST localhost:8080/wallets -H "Authorization: Bearer $INTERNAL" -H "Content-Type: application/json" \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}'

curl -s -X POST localhost:8080/wagering/transactions -H "Authorization: Bearer $PROVIDER" \
  -H "Content-Type: application/json" -H "Idempotency-Key: provider-a:transaction-123" \
  -d '{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"<id da carteira>","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'

curl -s -X POST localhost:8080/wallets/<id da carteira>/reconciliation -H "Authorization: Bearer $INTERNAL"
curl -s localhost:8080/metrics
```

### Mensagens SQS

Envelope `WagerTransactionRequested` do enunciado, com `MessageGroupId` = `walletId` e `MessageDeduplicationId` = `messageId`. O `providerId` precisa estar em `SQS_TRUSTED_PROVIDERS` (ver a fronteira de confiança em `ARCHITECTURE.md`).

```sh
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "<walletId>" --message-deduplication-id msg-123 \
  --message-body '{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z","data":{"providerId":"provider-a","externalTransactionId":"transaction-124","idempotencyKey":"provider-a:transaction-124","playerId":"<playerId>","walletId":"<walletId>","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}'

# eventos publicados pela outbox e mensagens recusadas (com o atributo failureReason)
docker compose exec localstack awslocal sqs receive-message --queue-url http://localhost:4566/000000000000/wager-events.fifo --max-number-of-messages 10
docker compose exec localstack awslocal sqs receive-message --queue-url http://localhost:4566/000000000000/wager-transactions-dlq.fifo --message-attribute-names All
```

### Testes

Comandos do enunciado (unitários, sem dependências externas):

```sh
go test ./...
go test -race ./...
go vet ./...
```

Os demais exigem a stack do Compose em execução e o `.env` local. Os testes leem credenciais do ambiente ou, na falta, do `.env` (o `go test` não carrega `.env` sozinho), criam um banco e filas próprios por teste e os removem no fim, sem tocar no banco de desenvolvimento:

```sh
docker compose up -d --wait postgres keycloak localstack   # dependências dos testes

go test -race -tags=integration ./...   # integração: PostgreSQL, Keycloak e LocalStack reais (~4 min)
./scripts/distributed-test.sh           # três processos reais do binário, com SIGKILL e reinício (2,5 a 3,5 min)
./scripts/smoke-test.sh                 # contra a API do Compose (exige docker compose up e migrations aplicadas)
```

- Integração (`-tags=integration`): schema, migrations `up → down → up`, constraints e ledger imutável, atomicidade, idempotência, concorrência (100/80/80, 50 requisições idênticas, carteiras distintas em paralelo), Keycloak real (tokens ausentes, inválidos, expirados, audience errada, isolamento entre provedores), inbox, reentrega, DLQ e redrive, outbox concorrente e recuperação, referências pendentes, Fx (início, encerramento e liberação de recursos) e observabilidade. Variáveis opcionais: `TEST_DATABASE_URL`, `TEST_KEYCLOAK_URL`, `TEST_SQS_ENDPOINT`.
- Distribuído (`-tags=integration,distributed`, `tests/distributed_test.go`): compila `cmd/api` com `-race` e executa três processos do sistema operacional, cada um com memória e pool próprios. Falhas são provocadas com SIGKILL em pontos controlados por um proxy SQS por processo. Os PIDs, o resumo (`summary.txt`) e os logs JSON de cada processo ficam em `DISTRIBUTED_EVIDENCE_DIR` (padrão: diretório temporário impresso no início). Cenários e resultados em `ARCHITECTURE.md` (seção M4).
- Smoke test: health, tokens reais, 401/403, carteira, BET com replay, isolamento entre provedores, BET e duplicata por SQS, DLQ com `UNTRUSTED_PROVIDER`, reconciliação, outbox drenada e métricas.

### CI

`.github/workflows/ci.yml`, em push/PR para `main` e manual:

- `quality`: `gofmt`, `go vet` (sem tag, `integration` e `integration,distributed`), testes unitários com e sem `-race`, build.
- `integration`: gera um `.env` efêmero (`scripts/generate-env.sh`, valores mascarados no log), sobe a stack com `docker compose up --build --wait`, aplica as migrations com a CLI, roda `go test -race -tags=integration ./...` e o smoke test, reverte e reaplica as migrations; em falha, imprime `docker compose ps` e os logs.
- `distributed`: mesma preparação, roda `scripts/distributed-test.sh` e publica `summary.txt` e os logs dos processos como artefato.

Nenhum `.env`, token ou segredo é versionado; cada job parte de volumes vazios e os remove ao final.

## Observabilidade

- Logs JSON (stdout) com `correlationId` (o ID da transação financeira, o mesmo dos eventos da outbox), `transactionId`, `walletId`, `providerId` e, no SQS, `messageId`/`sqsMessageId`; nunca tokens, segredos ou o payload financeiro.
- `GET /metrics`: operações por origem e status, duplicatas, `sqs_messages_total{outcome}` (inclui `retried`), DLQ por motivo, conflitos (`wagering_conflicts_total{source,kind}`: lock da carteira, idempotência, mensagem), latência (`wagering_processing_duration_seconds{source}`), atraso e tamanho da outbox, passos de referências, erros dos workers e divergências de reconciliação.
- `GET /health/live` e `GET /health/ready` (PostgreSQL e SQS).

## Estado da entrega

| Área | Situação |
| --- | --- |
| Composição Fx + HTTP + pool PostgreSQL, com início e encerramento | Implementado e testado (M0.1, M3) |
| Domínio `Money`, `Wallet`, `Operation`; operações BET/WIN/LOSS/REFUND/ROLLBACK | Implementado e testado (M1.2, M1.3) |
| Schema financeiro PostgreSQL (migrations, constraints, triggers) | Implementado e testado (M1.1) |
| API HTTP com autenticação/autorização OIDC (Keycloak real) | Implementado e testado (M2) |
| Consumer SQS FIFO, inbox transacional, DLQ e redrive | Implementado e testado com LocalStack (M3) |
| Outbox: persistência e publicador FIFO com ordem por carteira | Implementado e testado com LocalStack (M3) |
| Referências pendentes: persistência, replay e worker automático | Implementado e testado (M1.3, M3) |
| Reconciliação, logs JSON e métricas | Implementado e testado (M2.1, M3, M4) |
| Três processos independentes, SIGKILL, recuperação e reinício completo | Testado com processos reais (M4) |
| CI com PostgreSQL, Keycloak e LocalStack reais | Configurada (M4) |
| IAM real do broker; separação de papéis do banco (D8) | Não implementado (ver limitações em `ARCHITECTURE.md`) |

## Princípios de implementação

O desenvolvimento seguiu **RED → GREEN → REFACTOR** em incrementos pequenos. Garantias financeiras e regras de autorização são verificadas com testes. Mudanças arquiteturais relevantes exigem aprovação.

Este arquivo documenta apenas o que foi efetivamente implementado e verificado.
