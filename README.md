# Jungle Gaming Challenge — Backend de apostas em Go

Implementação do desafio técnico de processamento financeiro distribuído de apostas com API HTTP e mensagens SQS.

> **Status:** M0.1 (bootstrap), M1.1 (schema financeiro) e M1.2 (Money, carteira, BET, idempotência e outbox; sem HTTP) implementados e testados; WIN/LOSS/REFUND/ROLLBACK, autenticação, SQS e publicador da outbox ainda não existem. Detalhes em [`ARCHITECTURE.md`](ARCHITECTURE.md).

## Documentos

- [`docs/REQUISITOS.md`](docs/REQUISITOS.md): enunciado original (não editar).
- [`ARCHITECTURE.md`](ARCHITECTURE.md): decisões aprovadas, interpretações e limitações.
- [`CLAUDE.md`](CLAUDE.md): regras de execução das tarefas pelo Claude Code.

## Stack definida

Go 1.26+, Uber Fx, Fiber v3, PostgreSQL, pgx/v5, sqlc, golang-migrate v4.20.1 (biblioteca e CLI; migrations `.up.sql`/`.down.sql`), Keycloak, AWS SQS com LocalStack e Docker Compose.

## Como executar

Disponível hoje: a API com um único endpoint, `GET /health/live`, conectada ao PostgreSQL, e as migrations do schema financeiro. Não existem ainda endpoints financeiros, autenticação, consumidor SQS nem outbox (ver "Estado da entrega").

### Pré-requisitos

- Go 1.26+ (`go.mod` declara `go 1.26.0`; o Dockerfile usa `golang:1.26-alpine`).
- Docker e Docker Compose v2.

### Configuração e serviços

Suba os serviços (o Compose usa os valores locais padrão de `.env.example`; copiar o arquivo para `.env` só é necessário para alterá-los):

```sh
docker compose up -d --build
curl -i http://127.0.0.1:8080/health/live   # 200 {"status":"ok"}
```

- `postgres` (PostgreSQL 17) e `api` são os serviços efetivamente exercitados pelos testes e pela API atual.
- `keycloak` e `localstack` (SQS) são iniciados pelo Compose (o `api` depende deles), mas a aplicação ainda não os utiliza; seu provisionamento não foi validado por este incremento.
- A API lê `DATABASE_URL` (obrigatória) e `HTTP_ADDR` (padrão `:8080`). No Compose, ambas são definidas em `compose.yaml`.
- As migrations **não** são aplicadas automaticamente: use a seção a seguir.

### Migrations

Arquivos em `db/migrations/`, aplicados com golang-migrate. Com o PostgreSQL do Compose em execução (`docker compose up -d postgres`), contra um banco de sua escolha:

```sh
export DATABASE_URL="postgres://jungle:localdev-only@127.0.0.1:5432/jungle?sslmode=disable"
migrate() { docker run --rm --network host -v "$PWD/db/migrations:/migrations:ro" migrate/migrate:v4.20.1 -path=/migrations -database "$DATABASE_URL" "$@"; }

migrate up          # aplica
migrate down -all   # reverte tudo (destrutivo: apaga os dados financeiros)
migrate version
```

A reversão `up → down → up` foi executada com esta CLI em um PostgreSQL 17 descartável e deixou 0 tabelas, funções e triggers após o `down`. Não aplique `down` em um banco com dados que precisem ser preservados.

### API e autenticação

A documentar após implementar e testar as rotas e a emissão/validação de credenciais de teste.

### Testes

Comandos do enunciado:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Testes unitários e de integração do schema, já executados e aprovados:

- `go test ./...`, `go test -race ./...` e `go vet ./...` (unitários, sem dependências externas);
- `go test -tags=integration -race ./...` (exige o PostgreSQL do Compose em `127.0.0.1:5432`, ou `TEST_DATABASE_URL`; cada teste cria e remove um banco próprio, sem tocar no banco de desenvolvimento).

Ainda pendentes e não documentados como executáveis: testes com autenticação, mensageria SQS, três instâncias independentes e simulações de falha.

## Estado da entrega

| Área | Situação |
| --- | --- |
| Composição Fx + HTTP (`GET /health/live`) + pool PostgreSQL, com início e encerramento | Implementado e testado (M0.1) |
| Domínio `Money`, `Wallet`, `Bet`; abertura de carteira e BET com idempotência (sem HTTP) | Implementado e testado (M1.2) |
| Schema financeiro PostgreSQL (migrations, constraints, triggers) | Implementado e testado (M1.1) |
| WIN, LOSS, REFUND, ROLLBACK | Pendente |
| Autenticação/autorização com IdP real | Pendente |
| Consumer SQS, inbox e DLQ | Pendente |
| Outbox: persistência | Implementado (M1.2); publicador pendente |
| Referências pendentes, reversões e reconciliação | Pendente |
| Testes distribuídos e recuperação | Pendente |

## Princípios de implementação

O desenvolvimento seguirá **RED → GREEN → REFACTOR** em incrementos pequenos. Garantias financeiras e regras de autorização devem ser verificadas com testes. Mudanças arquiteturais relevantes exigem aprovação.

Este arquivo documentará apenas o que foi efetivamente implementado e verificado.