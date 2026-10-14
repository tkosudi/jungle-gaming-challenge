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

## 8. Estilo obrigatório: Go idiomático, legível e simples

**Prioridade:** correção financeira e segurança > clareza do fluxo > simplicidade > extensibilidade comprovadamente necessária. Código curto não é necessariamente código simples. Preserve todas as garantias de produção.

- Escreva Go que um engenheiro Go consiga acompanhar de cima para baixo, sem conhecer padrões ou bibliotecas além das efetivamente usadas. Prefira nomes específicos, funções diretas, retornos antecipados de erro e tratamento explícito de recursos.
- Comece pela implementação **mais simples que satisfaz os requisitos e os testes**. Para introduzir interface, factory, wrapper, helper, struct, goroutine, canal, pacote ou camada, explique qual problema real ela resolve **agora**. Se não houver justificativa concreta, não introduza.
- Siga a convenção de Go: interfaces pequenas, preferencialmente definidas por quem as consome; composição em vez de hierarquias; `context.Context` quando a operação suporta cancelamento; erros com contexto (`fmt.Errorf(... %w ...)`) quando útil. Evite abstrações e padrões de Java/C# transplantados para Go.
- Prefira construtores previsíveis (`NewServer`, `NewStore`, `NewService`) que revelem dependências e retornem uma estrutura utilizável. Centralize registro de rotas no transporte e mantenha handlers finos, sem SQL ou regras financeiras.
- Mantenha **Uber Fx apenas na composição/inicialização e nos adaptadores de lifecycle**. Código de domínio e casos de uso não podem importar Fx/Fiber/pgx. Os detalhes de `OnStart`, `OnStop`, listener, goroutine, shutdown, timeout e cleanup devem continuar corretos e fáceis de rastrear.
- Não use `fx.Invoke` vazio por hábito. Se uma construção precisa ser forçada para registrar hooks, explique isso, escolha a ligação mais explícita e preserve a ordem de startup/shutdown. Não substitua garantias reais por uma API mais bonita.
- Configuração: carregamento simples, validação explícita de entradas obrigatórias e inválidas, defaults documentados e testes determinísticos. Não adicione Viper ou outro framework só para reduzir linhas. Uma pequena função de injeção de ambiente é permitida se facilitar concretamente os testes.
- SQL permanece explícito e gerado via sqlc quando aplicável. **Nunca** simplifique removendo transações, constraints, locks, idempotência persistente, validação de overflow, rollback ou fechamento de recursos.
- Prefira comentários que expliquem o **porquê**, invariantes ou armadilhas, não comentários que repitam o código. Evite siglas crípticas, funções triviais desnecessariamente fragmentadas e testes com mocks que escondam garantias exigidas do PostgreSQL/SQS/IdP.
- Não faça grandes refatorações enquanto implementa novos comportamentos. Escolha mudanças pequenas e verificáveis. Sem necessidade concreta, mantenha os arquivos simples já existentes como estão.

### Revisão de legibilidade antes de declarar pronto

Confira e responda objetivamente:

1. Um leitor consegue identificar em até alguns minutos onde a requisição entra, onde fica a regra de negócio e onde ocorre a persistência?
2. Cada abstração nova existe para resolver uma necessidade atual? Qual? Uma função direta seria mais clara?
3. O fluxo de erros e cancelamentos é óbvio? Toda aquisição de recurso tem cleanup nas rotas de sucesso **e** falha?
4. Há complexidade acidental que pode ser removida **sem perder** concorrência correta, idempotência, atomicidade, segurança ou testes?
5. Você consegue explicar a solução em português, com um exemplo concreto, sem recorrer a jargões como "SOLID" ou "clean architecture" como justificativa suficiente?

**Para funcionalidades novas:** mantenha RED → GREEN → REFACTOR com evidências reais. **Para refatoração pura:** execute a suíte existente antes e depois, prove que o comportamento foi preservado e não invente uma etapa RED artificial. Se alterar contratos, erros, rotas ou semântica, trate como mudança funcional e exija testes apropriados.

No relatório final, separe: (a) complexidade essencial imposta pelos requisitos; (b) complexidade evitada/removida; (c) trade-offs e garantias preservadas. Se a opção mais curta piorar a segurança ou a clareza, mantenha a opção mais explícita.

### Política de comentários e documentação

O código destinado ao GitHub deve ser profissional, legível e autoexplicativo sempre que possível. Comentários não devem substituir nomes claros, funções pequenas ou responsabilidades bem definidas.

**Escreva comentários somente quando agregarem informação que não esteja evidente no código:**

- Explique o motivo de decisões técnicas não óbvias, limitações e trade-offs relevantes.
- Documente invariantes financeiras, garantias de concorrência, idempotência, atomicidade, locking, compensações e condições de falha.
- Explique comportamentos delicados de lifecycle, shutdown, retries e recuperação de recursos quando necessário.
- Preserve diretivas essenciais como `//go:build integration`.
- Utilize comentários GoDoc em APIs públicas quando ajudarem a compreender o contrato.

**Evite:**

- Comentários que apenas repetem o que a função ou instrução já faz.
- Tutoriais, explicações linha por linha ou comentários excessivamente didáticos.
- Comentários decorativos, cabeçalhos redundantes e código comentado sem justificativa.
- Referências ao processo de geração por IA, prompts ou conversas de desenvolvimento.
- Comentários extensos para explicar decisões que pertencem ao README ou ao ARCHITECTURE.md.

**Critério obrigatório:** se o comentário for removido, o leitor perderá alguma informação importante para compreender, utilizar ou manter corretamente o código?

- Se sim, preserve-o de forma clara e concisa.
- Se não, prefira removê-lo.

Explicações detalhadas para estudo e preparação de entrevistas devem ficar em documentos privados, separados do código versionado.

Não remova comentários técnicos úteis apenas para reduzir o número de linhas. Não simplifique código sacrificando segurança, observabilidade, tratamento de erros ou requisitos financeiros.

Antes de concluir cada milestone, revise também a qualidade e a necessidade dos comentários adicionados.