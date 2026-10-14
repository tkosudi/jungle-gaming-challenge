# Arquitetura e decisões

Registro das decisões aprovadas, com o status real de implementação. O enunciado (`docs/REQUISITOS.md`) prevalece sobre este documento.

## Status

| Área | Situação |
| --- | --- |
| Schema financeiro PostgreSQL (M1.1) | Implementado e coberto por testes de integração reais |
| Tipo `Money`, agregados, repositórios, casos de uso | Pendente |
| Idempotência HTTP/SQS, locking por carteira no caso de uso | Pendente (o schema já sustenta as garantias de unicidade) |
| Inbox, outbox, worker de referências | Pendente (D7) |
| Autenticação/autorização, SQS, observabilidade | Pendente |

## Migrations: golang-migrate (e não Atlas)

**Decisão:** migrations versionadas em `db/migrations/NNNNNN_nome.{up,down}.sql`, aplicadas com golang-migrate.

**Motivo:** o Atlas era a primeira opção, condicionada à prova de reversão reproduzível. Ela não foi obtida:

- O formato `-- atlas:txtar` com seções `migration.sql` e `down.sql` (documentado em `atlasgo.io/versioned/down`) foi testado com `arigaio/atlas:1.3.3`, `1.3.3-extended`, `1.3.3-community` e `latest` (canary `v1.3.4`), usando inclusive o exemplo literal da documentação.
- Em todas as variantes, `migrate apply` (e `--dry-run`) trata o arquivo como SQL puro e executa as duas seções, de modo que a própria migration desfaz o que criou. A variante `community` não reconhece `migrate down --dir`.
- Tentativas de ativar o parser (`?format=atlas`, `atlas.hcl` com `env`) não alteraram o resultado. `atlas whoami` exige login, mas isso não prova que o txtar o exija; a causa não foi identificada.

Logo, reversão manual segura não foi demonstrada. Reavaliar o Atlas depois, com um `atlas login` ou versão que comprovadamente interprete txtar, é possível porque o SQL é padrão e portável.

**Versões:** biblioteca `github.com/golang-migrate/migrate/v4 v4.20.1` (`go.mod`, usada pelos testes com o driver `database/pgx/v5`) e CLI `migrate/migrate:v4.20.1` (`migrate -version` reporta `4.20.1`).

**Consequência:** cada migration tem `.down.sql` escrito à mão. A prova `up → down → up` (sem resíduo de tabelas, funções ou triggers) é um teste (`TestMigrations_UpDownUpLeavesNoResidue`) e foi repetida pela CLI.

## Dinheiro

- `BIGINT` em unidades mínimas (centavos) e moeda ISO 4217 explícita (`CHECK currency ~ '^[A-Z]{3}$'`). Nenhum `FLOAT`/`NUMERIC` no schema.
- Overflow de `BIGINT` aborta com `22003` (testado em `UPDATE ... balance + 1` e na aritmética do ledger); não há "wrap around".
- Parsing, escala de duas casas e comparação entre moedas pertencem ao tipo `Money` (a implementar).

## Schema (migration `000001`)

### `wallets`
Raiz do agregado. Uma por `(player_id, currency)`; `balance >= 0`; `version >= 1`.
- Trigger `BEFORE UPDATE`: identidade imutável; `version` sobe exatamente 1 **se e somente se** o saldo mudar.
- `DELETE` e `TRUNCATE` bloqueados.

### `wager_transactions`
Operações internas (`OPENING`) e externas na mesma tabela (D1), com `CHECK`s por tipo e estado.

| Proteção | Mecanismo |
| --- | --- |
| Operação única por provedor | `UNIQUE (provider_id, external_transaction_id)` |
| Chave de idempotência isolada por provedor (D2) | `UNIQUE (provider_id, idempotency_key)`; a chave de outro provedor pode ser igual |
| Hash do payload | `payload_hash bytea` de 32 bytes (SHA-256); mesma chave com hash diferente é conflito, decidido pelo caso de uso |
| `OPENING` sem metadados externos; externa com todos, não vazios | `wager_tx_origin_fields` |
| `LOSS` com valor zero; demais com valor positivo | `wager_tx_amount_policy` |
| `REFUND`/`ROLLBACK` exigem referência externa | `wager_tx_reference_policy` |
| Campos por estado (`PENDING_REFERENCE` só para `WIN`/`REFUND`/`ROLLBACK` com referência — D6) | `wager_tx_state_fields` |
| Abertura positiva é `PROCESSED`; uma por carteira | `wager_tx_opening_processed` + índice parcial único |
| Referência resolvida do mesmo provedor e da mesma carteira | FK composta `(reference_transaction_id, provider_id, wallet_id)` |
| Jogador e moeda iguais aos da carteira | FK composta `(wallet_id, player_id, currency)` |
| `PROCESSED` que movimenta dinheiro tem lançamento coerente | Trigger diferido (seção D5 abaixo) |
| Terminais (`PROCESSED`/`REJECTED`/`FAILED`) imutáveis; identidade imutável; sem volta a `PENDING`; sem `DELETE`/`TRUNCATE` | Trigger `BEFORE UPDATE` |
| Rejeição persistida para replay (D3) | `REJECTED` com `failure_code` obrigatório; `wallet_id` é nulo apenas em `REJECTED`/`FAILED` |

D4: não há commit de `PENDING` no fluxo síncrono; o valor existe no schema para a retomada e é pré-condição de `PENDING_REFERENCE → ...`. `PENDING_REFERENCE` é durável, com `reference_attempts`, `next_attempt_at` e índice parcial para o worker.

### `wallet_ledger_entries`
Append-only: `UPDATE`, `DELETE` e `TRUNCATE` bloqueados por trigger.
- `CHECK` aritmético: `CREDIT: after = before + amount`; `DEBIT: after = before - amount`; `amount > 0`; saldos `>= 0`.
- `UNIQUE (wallet_id, transaction_id)` e `UNIQUE (wallet_id, wallet_version)`.

### Correspondência transação ↔ lançamento ↔ carteira (D5)

Três triggers `DEFERRABLE INITIALLY DEFERRED`, verificados no `COMMIT`. A ordem dos comandos dentro da transação não importa, e vários movimentos legítimos podem coexistir na mesma transação SQL (`TestLedger_TwoMovementsInOneCommit`).

| Vínculo | Trigger | Regra |
| --- | --- | --- |
| Lançamento → transação e encadeamento | `ledger_entries_consistency` (INSERT no ledger) | Transação existente, `PROCESSED`, não `LOSS`, mesma carteira e mesmo valor; direção do tipo (`BET` débito; `OPENING`/`WIN`/`REFUND` crédito; `ROLLBACK` o contrário do movimento referenciado); `balance_before` igual ao `balance_after` anterior; versão seguinte. A **versão 1 é exclusivamente a `OPENING`** (e `OPENING` só na versão 1); carteira aberta com saldo zero não tem `OPENING` e seu primeiro movimento é a versão 2 |
| Carteira ↔ ledger, nos dois sentidos | `wallets_match_ledger` (INSERT/UPDATE em `wallets`) e `ledger_entries_match_wallet` (INSERT no ledger), mesma função | Saldo e versão da carteira são exatamente os do último lançamento (zero/versão 1 sem lançamentos). Alterar saldo sem lançamento, ou inserir só o lançamento, falha no commit |
| Transação → lançamento | `wager_transactions_match_ledger` (INSERT/UPDATE com `status = 'PROCESSED'` e `kind <> 'LOSS'`) | Existe exatamente um lançamento (a `UNIQUE (wallet_id, transaction_id)` limita a um) e `result_balance = balance_after` dele. `LOSS` não gera lançamento |

Pela imutabilidade da transação terminal e do ledger, `result_balance` não diverge do ledger depois do commit, o que mantém o replay histórico fiel.

Concordância de identidade: a FK composta `(wallet_id, player_id, currency) → wallets (id, player_id, currency)` obriga jogador e moeda da transação a serem os da carteira. `wallet_id` nulo (rejeição por carteira inexistente) não é verificado (`MATCH SIMPLE`) e preserva o jogador informado.

### DEC-001 — compensação direta única
Índice parcial único sobre `reference_transaction_id` onde `status = 'PROCESSED' AND kind IN ('REFUND','ROLLBACK')`.
- `REFUND(BET)` e `ROLLBACK(BET)` competem pela mesma vaga: o segundo falha com `23505`.
- A exclusividade é histórica: `ROLLBACK(REFUND)` referencia o `REFUND` (não a `BET`), e o `REFUND` permanece `PROCESSED`; logo `ROLLBACK(BET)` continua bloqueado. Nada é apagado ou reaberto.
- Compensações `REJECTED` ou pendentes não ocupam a vaga.

## Concorrência

A garantia principal será `SELECT ... FOR UPDATE` na carteira, dentro da transação do caso de uso (a implementar). Carteiras diferentes travam linhas diferentes. O banco acrescenta defesas independentes do lock: `version` só avança em 1 com mudança de saldo (um escritor atrasado é recusado — `TestWallets_StaleWriterIsRejected`), `UNIQUE (wallet_id, wallet_version)` e a verificação de commit acima. `UNIQUE (wallet_id, wallet_version)` não é o mecanismo de concorrência.

## Limites conhecidos

- **Privilégios (D8):** os triggers e `CHECK`s não protegem contra o dono das tabelas ou um superusuário, que pode `DISABLE TRIGGER` ou `DROP`. O schema não separa papéis; a aplicação deve conectar com um papel sem ownership e sem `TRIGGER`/`TRUNCATE`, e o papel de migration deve ser distinto. Essa separação ainda não foi implementada nem testada.
- Regras que dependem de contexto de negócio permanecem no domínio: `REFUND` só sobre `BET`; `ROLLBACK` sobre `BET`, `WIN` ou `REFUND`; concordância de jogador, moeda e rodada com a referência; valor igual ao referenciado; transições válidas da máquina de estados.
- As verificações de D5 acontecem no `COMMIT`: o erro (`23514`, constraint `wallet_matches_ledger`, `ledger_entry_consistency` ou `transaction_matches_ledger`) aparece ao confirmar, e o caso de uso deve tratá-lo como falha não retentável de invariante. São verificadas nos testes por transações explícitas, não por um repositório real.
- `result_balance` de `LOSS` e de rejeições não é conferido com o ledger (não há lançamento): o valor informado ao provedor para esses casos é responsabilidade do caso de uso.
- A verificação do encadeamento lê o lançamento anterior no commit; em escritas concorrentes sem lock, `UNIQUE (wallet_id, wallet_version)` e o guard de versão derrubam uma delas, mas a serialização correta continua dependendo de `SELECT ... FOR UPDATE`.
- Os triggers de ledger são `AFTER INSERT`; a imutabilidade do ledger e das transações terminais impede que um histórico confirmado deixe de bater, exceto por papel dono/superusuário (D8).
- Códigos de falha (`failure_code`) são texto livre não vazio; o catálogo estável será definido com os casos de uso.
- Inbox e outbox não existem ainda; a outbox é pré-requisito das operações que emitem eventos (D7).
