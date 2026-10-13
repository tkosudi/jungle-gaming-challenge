# Jungle Gaming Challenge — instruções para o Claude Code

## 1. Fonte de verdade e contexto

- `docs/REQUISITOS.md` contém o enunciado original. Leia as seções relevantes **antes** de implementar cada tarefa. Não modifique esse arquivo.
- `ARCHITECTURE.md` registra as decisões aprovadas. Se houver conflito com os requisitos, reporte-o; o enunciado prevalece.
- `README.md` documenta a solução e deve descrever apenas funcionalidades comprovadamente implementadas.
- Você é **agente de implementação**, não o responsável por redefinir o escopo ou a arquitetura. O candidato e seu Tech Lead aprovam decisões relevantes.

## 2. Escopo e autorização de mudanças

Trabalhe **somente** na tarefa especificada pelo prompt atual.

Antes de editar, apresente sucintamente: objetivo, requisitos relacionados, comportamentos testáveis, arquivos que espera modificar e dependências da tarefa. Em seguida, execute, sem solicitar aprovação para detalhes triviais já contemplados pelo prompt.

- Não antecipe milestones ou implemente funcionalidades "por conveniência".
- Não adicione endpoints, tabelas, workers, filas, funcionalidades ou bibliotecas fora do escopo aprovado.
- Não crie camadas, interfaces ou pacotes especulativos.
- Não faça refatorações não relacionadas à tarefa.
- Não altere contratos financeiros ou políticas de autorização sem aprovação.
- Não modifique `docs/REQUISITOS.md` e não altere arquivos gerados pelo sqlc manualmente.
- Não faça `git commit`, `git push`, merge, alteração de remotes ou exclusão de arquivos sem autorização explícita.
- Não omita requisitos obrigatórios para encurtar a implementação. Reporte qualquer parte não entregue.

Se uma decisão arquitetural relevante não estiver documentada, **pare apenas a parte bloqueada** e apresente: problema, opções, trade-offs e recomendação. Aguarde aprovação antes de decidir. Uma correção local óbvia não exige interromper a tarefa inteira.

## 3. Stack e decisões já aprovadas

- Go 1.26+ (compatível com Fiber v3), mesma versão principal em `go.mod` e Dockerfile.
- Uber Fx para composição e lifecycle; Fiber v3 para HTTP.
- PostgreSQL com pgx/v5; sqlc para código tipado, com SQL explícito.
- Atlas e migrations versionadas, condicionado à prova de reversão reproduzível. Se não for possível, reporte antes de trocar de ferramenta.
- Keycloak com OAuth 2.0/OIDC; AWS SDK para SQS e LocalStack no desenvolvimento.
- Docker Compose para execução local.
- Monólito modular como orientação de organização, com instâncias independentes.
- `Money`: `int64` em unidades mínimas e moeda explícita; sem `float32`/`float64` em qualquer estágio monetário.
- Locking pessimista por carteira, com garantias financeiras verificáveis no PostgreSQL.

Organização convencionada **quando os módulos forem necessários**:

- `cmd/api/`: ponto de entrada.
- `internal/app/`, `internal/config/`: composição e configuração.
- `internal/domain/`: domínio sem dependência de Fx, Fiber, SQS ou pgx.
- `internal/usecase/`: coordenação dos casos de uso e limites transacionais.
- `internal/httpapi/`, `internal/auth/`: transporte e segurança.
- `internal/postgres/`: implementação de repositórios e mapeamento de domínio.
- `internal/sqs/`, `internal/worker/`: integração e trabalhos assíncronos.
- `db/migrations/`, `db/queries/`, `db/sqlc/`: migrations, SQL escrito à mão e Go gerado.
- Testes unitários `_test.go` junto ao código; testes multi-componentes em `tests/` se fizer sentido.

**Não crie a árvore completa agora.** Crie diretórios apenas na tarefa em que forem necessários. A aplicação pode evoluir a organização se isso for aprovado.

## 4. Regras financeiras inegociáveis

- Nunca use ponto flutuante para parsing, cálculo, serialização ou persistência monetária. Valide overflow e incompatibilidade de moedas.
- Saldo nunca negativo; atualização de saldo e ledger no mesmo commit.
- Ledger append-only, com proteções no banco contra update/delete.
- Idempotência persistente compartilhada entre HTTP e SQS; replay não reaplica operação e preserva o resultado original.
- As garantias de unicidade, atomicidade e concorrência precisam ser sustentadas pelo banco, não apenas pela memória do processo ou FIFO do SQS.
- Eventos externos não podem ser publicados antes do commit; outbox recuperável.
- Provedores não podem acessar transações de outros provedores, incluindo replays; ações internas exigem autorização específica.
- DEC-001: uma BET pode ter no máximo **uma compensação direta bem-sucedida**: `REFUND(BET)` ou `ROLLBACK(BET)`. A exclusividade é histórica; `ROLLBACK(REFUND)` não libera `ROLLBACK(BET)`. Preservar todo o histórico.
- Respeite `PENDING_REFERENCE`, terminalidade de `PROCESSED`/`REJECTED`/`FAILED` e a política aprovada para pendências.

## 5. TDD é obrigatório

Para **cada comportamento**, siga RED → GREEN → REFACTOR:

1. **RED:** escreva teste com critério observável; execute e demonstre que falha pelo motivo correto.
2. **GREEN:** implemente o mínimo para passar; execute o teste.
3. **REFACTOR:** melhore somente o necessário; execute os testes novamente.

Não escreva testes apenas depois da implementação. Use testes table-driven quando úteis e infraestrutura real nos testes de integração relevantes. Não simule com mocks as garantias que dependem de PostgreSQL, SQS ou IdP reais.

## 6. Explicação pedagógica obrigatória (em português)

Ao terminar cada tarefa, explique como se estivesse fazendo um code review com um engenheiro backend:

1. **Problema:** qual requisito foi resolvido e qual falha evitamos?
2. **Solução:** mostre o fluxo principal e onde a responsabilidade fica em cada arquivo.
3. **Garantias:** quais invariantes são protegidas em Go e quais são protegidas no banco?
4. **Trade-offs:** por que esta alternativa e qual opção mais simples/complexa foi rejeitada?
5. **TDD e evidências:** identifique testes RED/GREEN/REFACTOR, comandos realmente executados e seus resultados.
6. **Falhas e limites:** explique casos concorrentes, cancelamentos ou retries quando aplicáveis e o que ainda não foi verificado.

Inclua um pequeno exemplo de entrada/saída ou uma sequência de passos quando melhorar a compreensão. Evite aulas genéricas de Go: explique **o código específico implementado**.

## 7. Definição de pronto e condição de parada

- Confira o diff e indique arquivos criados/modificados e justificativa de cada alteração.
- Execute testes pertinentes, `go test ./...`, `go test -race ./...`, `go vet ./...`, `gofmt` e comandos de integração aplicáveis, conforme o estágio do projeto.
- Se um comando não puder ser executado, diga claramente qual, por quê e como reproduzi-lo; jamais declare sucesso presumido.
- Reporte requisitos não atendidos, decisões pendentes e riscos.
- **Pare ao finalizar a tarefa solicitada. Não inicie a próxima automaticamente.**
