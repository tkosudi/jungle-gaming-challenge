# Jungle Gaming Challenge — Backend de apostas em Go

Implementação do desafio técnico de processamento financeiro distribuído de apostas com API HTTP e mensagens SQS.

> **Status:** M0.1 a M1.3 (núcleo financeiro), M2 e M2.1 (API HTTP com autenticação OIDC via Keycloak e reconciliação) implementados e testados; SQS, laço automático do worker e publicador da outbox ainda não existem. Detalhes em [`ARCHITECTURE.md`](ARCHITECTURE.md).

## Documentos

- [`docs/REQUISITOS.md`](docs/REQUISITOS.md): enunciado original (não editar).
- [`ARCHITECTURE.md`](ARCHITECTURE.md): decisões aprovadas, interpretações e limitações.
- [`CLAUDE.md`](CLAUDE.md): regras de execução das tarefas pelo Claude Code.

## Stack definida

Go 1.26+, Uber Fx, Fiber v3, PostgreSQL, pgx/v5, sqlc, golang-migrate v4.20.1 (biblioteca e CLI; migrations `.up.sql`/`.down.sql`), Keycloak, AWS SQS com LocalStack e Docker Compose.

## Como executar

Disponível hoje: a API HTTP autenticada (carteiras, operações BET/WIN/LOSS/REFUND/ROLLBACK, consultas e reconciliação), health checks, `GET /metrics` (Prometheus) e as migrations. Não existem ainda consumidor SQS nem publicador da outbox (ver "Estado da entrega").

### Pré-requisitos

- Go 1.26+ (`go.mod` declara `go 1.26.0`; o Dockerfile usa `golang:1.26-alpine`).
- Docker e Docker Compose v2.

### Configuração e serviços

Crie o `.env` local (ignorado pelo Git) a partir do exemplo e troque os valores `change-me-*` (por exemplo, com `openssl rand -hex 24`). O Compose recusa subir sem as senhas e os client secrets, e o Keycloak lê os secrets do ambiente na importação do realm (placeholders `${...}` em `infra/keycloak/realm.json`). Ao trocar um secret, recrie o Keycloak (`docker compose up -d --force-recreate keycloak`).

```sh
cp .env.example .env   # e edite os valores
docker compose up -d --build
curl -i http://127.0.0.1:8080/health/live   # 200 {"status":"ok"}
```

- `postgres` (PostgreSQL 17) e `api` são os serviços efetivamente exercitados pelos testes e pela API atual.
- `keycloak` importa o realm `jungle` de `infra/keycloak/realm.json` (clientes, papéis e identidades de teste). `localstack` (SQS) é iniciado, mas a aplicação ainda não o utiliza.
- A API lê `DATABASE_URL`, `OIDC_ISSUER_URL` e `OIDC_AUDIENCE` (obrigatórias), `OIDC_JWKS_URL` (padrão: certificados do realm sob o issuer) e `HTTP_ADDR` (padrão `:8080`). No Compose, todas são definidas em `compose.yaml`.
- As migrations **não** são aplicadas automaticamente: use a seção a seguir.

### Migrations

Arquivos em `db/migrations/`, aplicados com golang-migrate. Com o PostgreSQL do Compose em execução (`docker compose up -d postgres`), contra um banco de sua escolha:

```sh
set -a; . ./.env; set +a   # credenciais do .env local
export DATABASE_URL="postgres://$POSTGRES_USER:$POSTGRES_PASSWORD@127.0.0.1:$POSTGRES_PORT/$POSTGRES_DB?sslmode=disable"
migrate() { docker run --rm --network host -v "$PWD/db/migrations:/migrations:ro" migrate/migrate:v4.20.1 -path=/migrations -database "$DATABASE_URL" "$@"; }

migrate up          # aplica
migrate down -all   # reverte tudo (destrutivo: apaga os dados financeiros)
migrate version
```

A reversão `up → down → up` foi executada com esta CLI em um PostgreSQL 17 descartável e deixou 0 tabelas, funções e triggers após o `down`. Não aplique `down` em um banco com dados que precisem ser preservados.

### API e autenticação

Tokens `client_credentials` do Keycloak local, com os client secrets do `.env`: `provider-a`, `provider-b` (operações de apostas) e `internal-service` (carteiras). Rotas, códigos e regras em `ARCHITECTURE.md` (seção M2). As migrations precisam estar aplicadas.

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
```

### Testes

Comandos do enunciado:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Testes unitários e de integração do schema, já executados e aprovados:

- `go test ./...`, `go test -race ./...` e `go vet ./...` (unitários, sem dependências externas);
- `go test -tags=integration -race ./...` (exige o PostgreSQL do Compose em `127.0.0.1:5432`, ou `TEST_DATABASE_URL`, e o Keycloak em `localhost:8180`, ou `TEST_KEYCLOAK_URL`; credenciais do banco e client secrets vêm do ambiente ou, na falta, do `.env` local, que os testes leem por conta própria (o `go test` não carrega `.env`); cada teste cria e remove um banco próprio, sem tocar no banco de desenvolvimento; os testes ponta a ponta HTTP ficam em `tests/`).

Ainda pendentes e não documentados como executáveis: mensageria SQS, três processos independentes e simulações de falha de processo.

## Estado da entrega

| Área | Situação |
| --- | --- |
| Composição Fx + HTTP (`GET /health/live`) + pool PostgreSQL, com início e encerramento | Implementado e testado (M0.1) |
| Domínio `Money`, `Wallet`, `Operation`; abertura de carteira e BET com idempotência (sem HTTP) | Implementado e testado (M1.2) |
| Schema financeiro PostgreSQL (migrations, constraints, triggers) | Implementado e testado (M1.1) |
| WIN, LOSS, REFUND, ROLLBACK (sem HTTP) | Implementado e testado (M1.3) |
| API HTTP com autenticação/autorização OIDC (Keycloak real) | Implementado e testado (M2) |
| Consumer SQS, inbox e DLQ | Pendente |
| Outbox: persistência | Implementado (M1.2); publicador pendente |
| Referências pendentes: persistência, replay e passo transacional do worker | Implementado e testado (M1.3); laço automático pendente |
| Reconciliação (`POST /wallets/:walletId/reconciliation`), log JSON e métrica de divergência | Implementado e testado (M2.1) |
| Testes distribuídos e recuperação | Pendente |

## Princípios de implementação

O desenvolvimento seguirá **RED → GREEN → REFACTOR** em incrementos pequenos. Garantias financeiras e regras de autorização devem ser verificadas com testes. Mudanças arquiteturais relevantes exigem aprovação.

Este arquivo documentará apenas o que foi efetivamente implementado e verificado.