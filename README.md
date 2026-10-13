# Jungle Gaming Challenge — Backend de apostas em Go

Implementação do desafio técnico de processamento financeiro distribuído de apostas com API HTTP e mensagens SQS.

> **Status:** projeto em preparação. Nenhuma funcionalidade financeira ou integração foi validada ainda. Atualizar esta seção somente com evidências de execução e testes.

## Documentos

- [`docs/REQUISITOS.md`](docs/REQUISITOS.md): enunciado original (não editar).
- [`ARCHITECTURE.md`](ARCHITECTURE.md): decisões aprovadas, interpretações e limitações.
- [`CLAUDE.md`](CLAUDE.md): regras de execução das tarefas pelo Claude Code.

## Stack definida

Go 1.26+, Uber Fx, Fiber v3, PostgreSQL, pgx/v5, sqlc, Atlas (verificação de rollback pendente), Keycloak, AWS SQS com LocalStack e Docker Compose.

## Como executar

**Ainda não disponível.** Esta seção será preenchida após validar o primeiro incremento de infraestrutura. Não execute comandos sugeridos como se o ambiente já estivesse pronto.

### Pré-requisitos

A documentar após o bootstrap: versão de Go, Docker/Compose e eventuais CLIs necessárias.

### Configuração e serviços

A documentar após a integração: `.env.example`, variáveis de ambiente, provisionamento de PostgreSQL, IdP, filas e DLQ, migrations e execução da aplicação.

### API e autenticação

A documentar após implementar e testar as rotas e a emissão/validação de credenciais de teste.

### Testes

Os comandos obrigatórios previstos no enunciado são:

```sh
go test ./...
go test -race ./...
go vet ./...
```

**Ainda não executados neste repositório por este kit.** Serão documentados os comandos separados para testes com dependências reais, três instâncias e simulações de falha.

## Estado da entrega

| Área | Situação |
| --- | --- |
| Composição Fx + HTTP + PostgreSQL | Pendente |
| Domínio Money, Wallet, Transaction, Ledger | Pendente |
| Persistência financeira, idempotência e concorrência | Pendente |
| Autenticação/autorização com IdP real | Pendente |
| Consumer SQS, inbox e DLQ | Pendente |
| Outbox e eventos | Pendente |
| Referências pendentes, reversões e reconciliação | Pendente |
| Testes distribuídos e recuperação | Pendente |

## Princípios de implementação

O desenvolvimento seguirá **RED → GREEN → REFACTOR** em incrementos pequenos. Garantias financeiras e regras de autorização devem ser verificadas com testes. Mudanças arquiteturais relevantes exigem aprovação.

Este arquivo documentará apenas o que foi efetivamente implementado e verificado.