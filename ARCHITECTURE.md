# Arquitetura e decisões

Registro das decisões aprovadas, com o status real de implementação. O enunciado (`docs/REQUISITOS.md`) prevalece sobre este documento.

## Status

| Área | Situação |
| --- | --- |
| Schema financeiro PostgreSQL (M1.1) | Implementado e coberto por testes de integração reais |
| `Money`, `Wallet`, abertura de carteira e `BET` com idempotência (M1.2) | Implementado e testado; sem exposição HTTP |
| Idempotência persistente e locking por carteira no caso de uso (M1.2) | Implementado e testado; entrada HTTP/SQS pendente |
| `WIN`, `LOSS`, `REFUND`, `ROLLBACK`, referências pendentes e passo transacional do worker (M1.3) | Implementado e testado; sem HTTP e sem laço automático do worker (M3) |
| Outbox (persistência) | Implementado (M1.2); sem publicador |
| API HTTP, autenticação OIDC (Keycloak), composição Fx do núcleo e reconciliação (M2, M2.1) | Implementado e testado com PostgreSQL e Keycloak reais |
| Inbox, laço do worker de referências com Fx, publicador SQS | Pendente |
| SQS; métricas além da reconciliação | Pendente |

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
- Parsing, escala de duas casas e comparação entre moedas pertencem ao tipo `Money` (`internal/domain`, M1.2).

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

A garantia principal é `SELECT ... FOR UPDATE` na carteira, dentro da transação do caso de uso (M1.2/M1.3). Carteiras diferentes travam linhas diferentes. O banco acrescenta defesas independentes do lock: `version` só avança em 1 com mudança de saldo (um escritor atrasado é recusado — `TestWallets_StaleWriterIsRejected`), `UNIQUE (wallet_id, wallet_version)` e a verificação de commit acima. `UNIQUE (wallet_id, wallet_version)` não é o mecanismo de concorrência.

## Limites conhecidos

- **Privilégios (D8):** os triggers e `CHECK`s não protegem contra o dono das tabelas ou um superusuário, que pode `DISABLE TRIGGER` ou `DROP`. O schema não separa papéis; a aplicação deve conectar com um papel sem ownership e sem `TRIGGER`/`TRUNCATE`, e o papel de migration deve ser distinto. Essa separação ainda não foi implementada nem testada.
- Regras que dependem de contexto de negócio permanecem no domínio: `REFUND` só sobre `BET`; `ROLLBACK` sobre `BET`, `WIN` ou `REFUND`; concordância de jogador, moeda e rodada com a referência; valor igual ao referenciado; transições válidas da máquina de estados.
- As verificações de D5 acontecem no `COMMIT`: o erro (`23514`, constraint `wallet_matches_ledger`, `ledger_entry_consistency` ou `transaction_matches_ledger`) aparece ao confirmar, e o caso de uso deve tratá-lo como falha não retentável de invariante. São verificadas nos testes por transações explícitas, não por um repositório real.
- `result_balance` de `LOSS` e de rejeições não é conferido com o ledger (não há lançamento): o valor informado ao provedor para esses casos é responsabilidade do caso de uso.
- A verificação do encadeamento lê o lançamento anterior no commit; em escritas concorrentes sem lock, `UNIQUE (wallet_id, wallet_version)` e o guard de versão derrubam uma delas, mas a serialização correta continua dependendo de `SELECT ... FOR UPDATE`.
- Os triggers de ledger são `AFTER INSERT`; a imutabilidade do ledger e das transações terminais impede que um histórico confirmado deixe de bater, exceto por papel dono/superusuário (D8).
- Códigos de falha (`failure_code`) são texto livre não vazio; o catálogo estável será definido com os casos de uso.
- A inbox não existe ainda; a outbox existe a partir do M1.2 (D7).

## Núcleo financeiro (M1.2): decisões

Implementado: `Money`, abertura de carteira, `BET` síncrona, idempotência persistente e outbox (somente persistência). Generalizado no M1.3 (seção seguinte): `Operation`/`Service.Process` cobrem os cinco tipos.

### Organização
- `internal/domain`: `Money`, `Operation` (validação e hash canônico), `Wallet.Debit/Credit`, `Decide` (M1.3). Sem pgx/Fx.
- `internal/usecase`: `Service.OpenWallet` e `Service.Process` (era `PlaceBet`), dono dos limites transacionais. Depende de `Store`/`Tx`, interfaces definidas por quem as consome: a regra do projeto proíbe pgx nos casos de uso, e a implementação vive em `internal/postgres`.
- `internal/postgres`: `Store` (transação `READ COMMITTED`) e `db/sqlc` (código gerado de `db/queries`; regenerar com `docker run --rm -v "$PWD:/src" -w /src sqlc/sqlc:1.30.0 generate`).

### Money
`int64` em centavos, escala fixa de duas casas, sem sinal, espaço, zeros à esquerda ou notação científica; overflow detectado no parsing e na soma. Moedas: subconjunto ISO 4217 ativo com duas casas decimais (lista fixa em `money.go`); JPY, KWD e códigos de teste ficam de fora por não caberem na escala do contrato.

### Identidade do provedor
`Process(ctx, providerID, operation)` recebe o provedor como argumento separado dos dados da operação. Desde o M2 o HTTP o deriva do token (seção M2); o consumidor SQS fará o mesmo a partir das credenciais do broker.

### Idempotência
- Hash canônico (`Operation.PayloadHash`, desde o M1.3): SHA-256 do JSON compacto com chaves em ordem alfabética (`amount`, `currency`, `externalTransactionId`, `gameId`, `kind`, `playerId`, `providerId`, `referenceExternalTransactionId` se houver, `roundId`, `walletId`), UUIDs em minúsculas, valor `d.dd`, sem escape de HTML; chave de idempotência e metadados de transporte fora do hash. Vetor conhecido em `operation_test.go`. Substituiu o formato binário do M1.2 (sem dados persistidos a migrar).
- Replay: existe operação do provedor com a mesma chave **e** o mesmo hash. Qualquer outra colisão (mesma chave com outro conteúdo, mesmo ID externo com outra chave) é `ErrIdempotencyConflict`.
- O `INSERT` usa `ON CONFLICT DO NOTHING`: quem perde a corrida espera o commit do vencedor, não escreve nada e relê o resultado dele. Não há retry nem violação UNIQUE tratada como replay. Vale também para duplicatas em carteiras diferentes (a UNIQUE é por provedor, não por carteira).
- O replay devolve status, `failure_code` e `result_balance` persistidos, não o saldo atual.

### BET
Ordem: consulta de replay sem lock → `SELECT ... FOR UPDATE` da carteira → decisão → `INSERT` da transação → saldo/versão → ledger → outbox, num único commit. Saldo insuficiente, carteira inexistente ou de outro jogador (`WALLET_NOT_FOUND`) e moeda divergente (`CURRENCY_MISMATCH`) são `REJECTED` persistidos (D3), sem ledger e sem alterar a carteira. Rejeições sem carteira válida usam `wallet_id` nulo e `result_balance` nulo.

### Falhas e commit incerto
- `lock_timeout` por transação (parâmetro do `Store`); estouro vira `ErrLockTimeout`, sem efeito persistido, reenvio seguro.
- Erro devolvido pelo servidor no COMMIT (incluindo triggers diferidos) é falha definitiva da transação.
- Falha de conexão ou contexto durante o COMMIT vira `ErrCommitUnknown`. O chamador reenvia a mesma requisição com a mesma chave: se o commit ocorreu, recebe o replay do resultado original; se não, a operação é aplicada uma única vez. Não há retry automático no serviço.

### Outbox (migration 000002)
`outbox_events`: `event_id` (também MessageDeduplicationId), `message_group_id` (carteira, MessageGroupId), `payload` (envelope completo, snapshot imutável por trigger), `sequence` (ordem por carteira, atribuída sob o lock), estado de publicação (`attempts`, `next_attempt_at`, `published_at`) e `UNIQUE (transaction_id, event_type)`. Eventos gravados: `WagerTransactionProcessed` + `WalletBalanceChanged` (BET processada e abertura positiva) e `WagerTransactionRejected`. Sem publicador; `correlationId` é o ID da transação financeira.

### Limites do M1.2
- Os testes de concorrência usam goroutines com pools e conexões independentes no mesmo processo, **não** três processos. O teste com três processos reais continua pendente.
- O publicador da outbox, retries de entrega e DLQ não existem; as linhas apenas acumulam.
- `Service` ainda não é composto no Fx e não há endpoint: só os testes o exercitam.

## Operações e compensações (M1.3)

Implementado: `WIN`, `LOSS`, `REFUND`, `ROLLBACK`, `PENDING_REFERENCE` com replay, e o **passo** transacional do worker (`Service.ResolveNextPendingReference`). Não implementado: HTTP, OIDC, SQS, inbox, laço automático do worker com Fx (M3). Migration `000003` adiciona só um trigger.

### Regras (`domain.Decide`, pura)
Ordem das verificações, depois do lock da carteira; ela define o código recebido e impede pendências que nunca poderiam ser válidas:

1. Carteira inexistente ou de outro jogador → `WALLET_NOT_FOUND`; moeda diferente → `CURRENCY_MISMATCH` (ambos sem `wallet_id`, como na BET).
2. Tipo da referência (WIN/REFUND → BET; ROLLBACK → BET, WIN ou REFUND; ROLLBACK de ROLLBACK é inválido) → `REFERENCE_KIND_INVALID`; jogador/carteira/moeda/rodada → `REFERENCE_MISMATCH`; valor integral em REFUND/ROLLBACK → `AMOUNT_MISMATCH`.
3. Estado da referência: pendente → a operação continua `PENDING_REFERENCE`; `REJECTED`/`FAILED` → `REFERENCE_NOT_PROCESSED`.
4. DEC-001: referência já compensada por REFUND/ROLLBACK processado → `REFERENCE_ALREADY_COMPENSATED`.
5. Saldo: débito sem saldo → `INSUFFICIENT_FUNDS` (BET) ou `REVERSAL_INSUFFICIENT_FUNDS` (ROLLBACK de WIN/REFUND); crédito que estoura `int64` → `BALANCE_OVERFLOW`.

| Operação | Movimento | Ledger / versão | Eventos (PROCESSED) |
| --- | --- | --- | --- |
| `BET` | débito | 1 DEBIT, +1 | Processed + BalanceChanged |
| `WIN` (com ou sem referência a uma BET) | crédito | 1 CREDIT, +1 | idem |
| `LOSS` (`0.00`) | nenhum; `result_balance` = saldo atual | nenhum, versão inalterada | só Processed |
| `REFUND` (BET) | crédito | 1 CREDIT, +1 | idem |
| `ROLLBACK` (BET) / (WIN ou REFUND) | crédito / débito | 1 CREDIT / 1 DEBIT, +1 | idem |

Rejeições geram só `WagerTransactionRejected`; pendência, só `WagerTransactionPendingReference` (sem saldo nem ledger). Códigos corrigíveis (novo ID externo após corrigir o dado): `WALLET_NOT_FOUND`, `CURRENCY_MISMATCH`, `REFERENCE_KIND_INVALID`, `REFERENCE_MISMATCH`, `AMOUNT_MISMATCH`. Definitivos: os demais.

### Interpretações adotadas (não explícitas no enunciado)
- WIN referencia uma BET processada da mesma rodada, mas tem valor próprio, pode haver várias por BET e não ocupa a vaga do DEC-001.
- Entrada inválida (LOSS com valor, REFUND/ROLLBACK sem referência, referência a si mesmo, `OPENING`) é recusada pelo domínio e **não** é persistida: é erro de validação do transporte, não rejeição financeira.
- Referência de outro provedor equivale a inexistente (nada é revelado); fica pendente e expira com `REFERENCE_NOT_FOUND`.
- `FAILED` existe no schema, mas nenhum caminho o produz: erro SQL inesperado, inclusive de integridade, faz rollback e sobe como erro observável. Converter em `FAILED` exigiria decisão explícita.

### Locks
Cada transação toma um único lock explícito: a **carteira** (`FOR UPDATE`). A FK composta obriga a referência a estar na mesma carteira, logo toda escrita que afeta a compensação de uma referência passa por esse lock; a referência é lida depois dele (READ COMMITTED enxerga todo commit anterior). REFUND e ROLLBACK concorrentes da mesma BET serializam: o segundo é gravado como `REJECTED / REFERENCE_ALREADY_COMPENSATED`. O índice do DEC-001 é só o backstop; como o `INSERT` usa `ON CONFLICT DO NOTHING`, uma violação dele apareceria como "conflito sem operação visível" (erro e rollback, sem dinheiro movido), mutação confirmada em teste.

### Worker de referências (passo)
1. `SELECT ... FOR UPDATE OF w SKIP LOCKED` escolhe a pendência vencida **travando a carteira**, nunca a linha da transação: ordem carteira → transação, sem esperar carteira ocupada. Travar a transação antes da carteira produz deadlock com uma requisição que já tem a carteira e insere uma transação referenciando a pendência (FK `FOR KEY SHARE`).
2. Em novo statement relê e revalida a pendência (o snapshot da seleção pode ser anterior ao commit de outra instância).
3. Procura a referência **antes** de qualquer expiração e aplica `Decide`. Referência encontrada resolve mesmo na última tentativa; referência existente porém pendente nunca expira (a cadeia é finita: ROLLBACK → WIN/REFUND pendente → BET ausente, que expira).
4. Sem referência: reagenda (`reference_attempts + 1`, `next_attempt_at = now() + min(1s·2^n, 60s)` pelo relógio do banco); na tentativa `MaxAttempts` (10) rejeita com `REFERENCE_NOT_FOUND`. Valores configuráveis (`ReferencePolicy`); expiração por tentativas, não por relógio, para que o tempo sem worker não consuma o prazo.
5. Estado, saldo, ledger e eventos no mesmo commit. A unicidade `(transaction_id, event_type)` do outbox, a imutabilidade do estado terminal e o `UPDATE ... WHERE status = 'PENDING_REFERENCE'` impedem processamento e eventos duplicados. Falha transitória desfaz o passo sem consumir tentativa. Uma referência que chega *depois* da expiração não reabre nada.

### Migration 000003
Trigger `AFTER INSERT OR UPDATE` em `wager_transactions` para linhas `PROCESSED` com referência: referência `PROCESSED`, tipo permitido, mesmo jogador/moeda/rodada e, em REFUND/ROLLBACK, valor igual. Disparar *depois* das FKs mantém o erro de provedor/carteira (M1.1) como está. Reversão `up → down → up` testada.

### Evidências e limites do M1.3
- Testes PostgreSQL reais: REFUND × ROLLBACK concorrentes (10 rodadas, 3 instâncias), DEC-001 histórica, ROLLBACK(WIN) sem saldo, LOSS sem ledger, pendência/replay/resolução/expiração, workers concorrentes (1 pendência × 8 chamadas; 12 carteiras × 3 workers), worker × REFUND síncrono, recuperação por outra instância, falhas injetadas sem `FAILED`.
- Mutações verificadas: remover o `FOR UPDATE` da carteira, o `SKIP LOCKED`, a consulta antes da expiração e a proteção de referência pendente fazem testes falharem.
- Pendente: teste com três **processos**; o passo resolve a pendência no próximo ciclo (a chegada da referência não a resolve na hora); `Money` sem negativos/negação e `Wallet` com campos exportados (requisitos do §6, para o milestone de reconciliação); `provider_id` ainda é argumento confiável do chamador.

## API HTTP, autenticação e composição (M2)

Implementado: rotas de carteira, envio e consulta de operações, health checks, validação de JWT do Keycloak e composição Fx de `Store`, `Service`, verificador e servidor. A reconciliação veio no M2.1 (seção seguinte). Não implementado: readiness do SQS e SQS (M3).

### Validação de tokens
- Biblioteca `github.com/coreos/go-oidc/v3` v3.21.0, sem descoberta: `RemoteKeySet` sobre `OIDC_JWKS_URL` (cache em memória, nova busca quando aparece um `kid` desconhecido) e `IDTokenVerifier` com **issuer exato** (`OIDC_ISSUER_URL`), **audience** `wagering-api` (`OIDC_AUDIENCE`), **somente RS256**, `exp` sem tolerância e `nbf`. Além disso, `typ` precisa ser `Bearer`: ID tokens do Keycloak são recusados.
- Keycloak no Docker: o `KC_HOSTNAME` fixa o issuer público (`http://localhost:8180/realms/jungle`) em todo token, inclusive nos obtidos pela rede interna. A API valida esse issuer e busca as chaves por `http://keycloak:8080/...` (`OIDC_JWKS_URL`). Separar "de onde vêm as chaves" de "quem é o emissor esperado" evita afrouxar o `iss`. Evidência: token pedido a `keycloak:8080` de dentro da rede do Compose tem `iss` público e foi aceito pela API em contêiner.
- Falha ao obter o JWKS (rede, 5xx, prazo) é `503 IDP_UNAVAILABLE`, não `401`: o go-oidc formata o erro do `KeySet` com `%v`, então o sinal volta por um ponteiro no contexto da chamada.

### Identidade e permissões
| Identidade (client_credentials) | Papel de realm | `provider_id` (claim fixo por cliente) | Pode |
| --- | --- | --- | --- |
| `provider-a`, `provider-b` | `wagering-provider` | o próprio | `POST /wagering/transactions`, `GET /wagering/transactions/:id`, `GET /providers/:providerId/wagering/transactions/:externalId` |
| `internal-service` | `wallet-operator` | — | `POST /wallets`, `GET /wallets/:id`, `GET /wallets/:id/ledger` |

- O provedor da operação vem **só** do claim `provider_id` do token validado. O `providerId` do corpo precisa ser igual (senão `403 PROVIDER_MISMATCH`, sem persistir nada); headers não influenciam.
- Isolamento no SQL: as consultas de transação filtram por `provider_id`, e a de outro provedor responde `404`, igual a inexistente. `providerId` do caminho diferente do token é `403`. Replays já eram isolados pelas UNIQUEs por provedor.
- Rotas sem papel adequado: `403 FORBIDDEN` (`WWW-Authenticate: Bearer error="insufficient_scope"`). Token ausente ou inválido: `401 UNAUTHENTICATED`.
- Identidades de teste do realm local: `test-other-audience` (provedor sem a audience `wagering-api`) e `test-short-lived` (token de 2s). Existem para provar com o Keycloak real a recusa de audience e de expiração.

### Contrato HTTP
Erros: `{"error":{"code":"...","message":"..."}}`. `POST /wagering/transactions` (header `Idempotency-Key` obrigatório, usado como recebido):

| Situação | Status | Corpo |
| --- | --- | --- |
| `PROCESSED` (novo ou replay) | 200 | `transactionId`, `status`, `balance`, `idempotentReplay` |
| `PENDING_REFERENCE` | 202 + `Location` | idem, sem `balance` |
| `REJECTED` (rejeição de negócio persistida) | 422 | idem, com `failureCode` |
| Entrada inválida (JSON, campos desconhecidos, valor numérico em vez de string, escala, moeda, tipo, chave ausente) | 400 `VALIDATION_ERROR` | erro |
| Conflito de idempotência | 409 `IDEMPOTENCY_CONFLICT` | erro |
| Lock da carteira esgotado, banco indisponível, prazo esgotado, commit incerto | 503 (`WALLET_BUSY`, `SERVICE_UNAVAILABLE`, `OUTCOME_UNKNOWN`) + `Retry-After` | erro; reenviar com a mesma chave é seguro |

`POST /wallets`: 201, ou 409 `WALLET_ALREADY_EXISTS`. Ledger: ordem pela versão da carteira; cursor opaco (base64url de `{carteira, última versão}`, recusado em outra carteira); `limit` de 1 a 100, padrão 50. Corpo limitado a 64 KiB (413), `Content-Type` JSON obrigatório (415). `GET /health/ready` verifica o PostgreSQL.

Falhas de conexão do banco (recusa, perda antes do COMMIT, SQLSTATE `08*`, `57P0*`, `53300`) viram `usecase.ErrUnavailable`: nada foi confirmado.

### Composição Fx
`postgres.Module` fornece pool e `usecase.Store` (`lock_timeout` 5s); `app` fornece `Service` (`DefaultReferencePolicy`), verificador e `Pinger`; `httpapi.Module` fornece app Fiber e servidor. Ordem de hooks preservada: PostgreSQL inicia antes do HTTP e fecha depois dele. Service e verificador não têm hooks, porque não abrem recursos.

### Evidências e limites do M2
- Unitários (sem banco; tokens RSA reais e JWKS local): assinatura de outra chave, alg `none`, HS256 com a chave pública, issuer interno/estranho, audience errada/ausente, expirado, `nbf` futuro, `typ` ID, adulterado, JWKS 503/inacessível; autorização por rota; validação; limite de corpo.
- Ponta a ponta (`tests/`, Fx completo, PostgreSQL e Keycloak reais): fluxo de carteira e operações, replays, conflito, 422, 202 com consulta, isolamento entre provedores, `providerId` forjado no corpo ou em header, assinatura e payload adulterados, audience incorreta, token expirado, segredo errado recusado pelo Keycloak; 100/80/80 e 30 requisições idênticas distribuídas por 3 instâncias Fx com pools próprios.
- Mutações verificadas: aceitar o `providerId` do corpo, pular a verificação de audience e a de issuer fazem testes falharem.
- Smoke test na API do Compose: 401/403/201/200 (novo e replay)/404 entre provedores.
- Limites: três processos reais continuam pendentes (as instâncias são do mesmo processo de teste); atualizar papéis no `realm.json` exige recriar o contêiner do Keycloak (`docker compose up -d --force-recreate keycloak`), que só importa realms inexistentes.

## Reconciliação, Money com sinal e endurecimento do OIDC (M2.1)

### Money com sinal
`Money` passou a admitir valores negativos **só como resultado de aritmética interna** (`Sub`, `Neg`, `Add` com negativos), com overflow detectado nos dois sentidos (inclusive `Neg(MinInt64)`) e `Amount()` formatando `-d.dd`. A fronteira não mudou: `NewMoney` (entrada externa) e `MoneyFromCents` (reidratação) recusam negativos, `Wallet.Debit/Credit` exigem valor positivo, `OpenWallet` recusa saldo inicial negativo e o banco mantém `balance >= 0`. Mudança de contrato do M1.2: `Sub` abaixo de zero devolvia `ErrNegativeAmount` e agora devolve o valor negativo (o único uso em produção, `Wallet.Debit`, compara antes de subtrair).

### Reconciliação (`POST /wallets/:walletId/reconciliation`, papel `wallet-operator`)
- Um único `SELECT` lê `wallets.balance` e o saldo líquido do ledger (créditos − débitos, inclusive a `OPENING`) no **mesmo snapshot**, sem `FOR UPDATE`: não espera nem bloqueia escritores. Duas leituras separadas em READ COMMITTED poderiam ver commits diferentes e acusar divergência falsa.
- A transação é `READ ONLY` (`Store.InReadOnlyTx`): o banco recusa qualquer escrita (`25006`, testado), além de o código não escrever.
- O líquido é somado em `numeric` e só ele vira `bigint`: os totais históricos de cada direção podem passar de `int64` com o saldo válido (regressão testada: abertura de `MaxInt64` centavos, BET e WIN de 0.01). Em Go, `difference = armazenado − calculado` com o `Money` assinado (`MoneyFromSignedCents` reidrata o líquido, que só é negativo num ledger corrompido); `consistent = difference == 0`; `checkedEntries` = lançamentos lidos.
- Divergência: `slog.Warn` em JSON (`walletId`, `currency`, `storedBalance`, `calculatedBalance`, `difference`, `checkedEntries`) e o contador Prometheus `wallet_reconciliations_total{result="divergent"}`; as séries `consistent`/`divergent` existem desde a partida (valor 0). Exposto em `GET /metrics` (registro próprio, só métricas da aplicação, público como os health checks). Logs da aplicação passaram a ser JSON (`cmd/api`).
- Dependência nova: `github.com/prometheus/client_golang` v1.24.1 (a v1.25.0 tinha menos de duas semanas).

### Limite de buscas ao JWKS
O go-oidc busca o JWKS de novo a cada `kid` desconhecido **e** a cada assinatura que não confere. O transporte do JWKS permite uma busca a cada 5s (`jwksMinRefreshInterval`); buscas simultâneas já são agrupadas pelo go-oidc. Consequências:
- Token forjado com `kid` desconhecido: `503 IDP_UNAVAILABLE` sem tocar o IdP; assinatura inválida com `kid` já verificado: `401` (a chave está em cache).
- Chave legitimamente rotacionada no Keycloak dentro de 5s após outra busca: até 5s de `503` com `Retry-After` para tokens com o `kid` novo. Trade-off aceito.

### Vínculo entre cliente e provedor (responsabilidade do IdP)
A API confia no claim `provider_id` porque ele é fixado pelo Keycloak por cliente (mapper hardcoded no `realm.json`), não pelo cliente. Logo, o vínculo `azp` → `provider_id` é **configuração confiável do IdP**: quem administra o realm decide qual cliente fala por qual provedor, e um cliente mal configurado agiria por outro provedor. A API não confere `azp`. Os clientes `test-other-audience` e `test-short-lived` emitem `provider_id = provider-a` e existem só no realm local de testes: não podem ser provisionados nem tratados como clientes de produção, e um realm de produção deve ser gerado sem eles.

### Evidências do M2.1
- Unitários: aritmética assinada e overflow; fronteira sem negativos; autorização e validação da rota; `/metrics`; 20 tokens com `kid` desconhecido geram 1 só busca ao JWKS; rotação depois do intervalo; 10 primeiras requisições simultâneas geram 1 busca; assinatura inválida com `kid` conhecido é 401.
- PostgreSQL real:
  - carteira coerente com abertura e uma BET (`checkedEntries = 2`);
  - abertura zero (0 lançamentos);
  - todos os tipos de movimento;
  - divergências positiva (`5.00`) e negativa (`-25.50`), criadas desligando os triggers como dono das tabelas (limite D8), com log JSON e métrica conferidos e saldo/ledger inalterados;
  - cerca de 900 reconciliações durante 300 escritas concorrentes de 2 instâncias, todas `consistent`, repetido 5 vezes;
  - resposta com a carteira travada por outro escritor e `lock_timeout` de 200 ms;
  - transação somente leitura recusa escrita.
- Ponta a ponta: contrato de 6 campos com `checkedEntries = 2` após abertura e BET; 403 para provedor, 401 sem token, 404 para carteira inexistente, contador visível em `/metrics`.

### Segredos locais
Nenhuma senha ou client secret fica em arquivo versionado. O `realm.json` usa placeholders `${PROVIDER_A_CLIENT_SECRET}` etc., que o Keycloak 26.8.0 resolve do ambiente ao importar o realm (validado: o secret do `.env` obtém token, o literal antigo e o placeholder cru recebem 401). O Compose passa esses valores ao Keycloak e exige (`${VAR:?}`) as senhas do PostgreSQL e do administrador e os client secrets. Os valores reais ficam no `.env` local, ignorado pelo Git; o `.env.example` só tem marcadores `change-me-*`. Os testes de integração leem as mesmas variáveis do ambiente ou, na ausência delas, do `.env` (`internal/testenv`, usado só por testes). Sem secret manager: para o desafio, `.env` local basta.
