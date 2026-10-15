//go:build integration && distributed

package tests

// Execução com processos reais: o binário de cmd/api é compilado (com -race) e executado como
// processos independentes do sistema operacional, cada um com memória e pool PostgreSQL
// próprios, compartilhando o PostgreSQL, o Keycloak e o LocalStack do Compose (banco e filas
// exclusivos desta execução). Cada processo fala com o SQS por um proxy próprio (faultProxy),
// onde ficam os dois pontos de falha controlados: segurar o DeleteMessage de uma mensagem já
// confirmada no banco e segurar a resposta de um envio de evento que o SQS já aceitou. O
// processo é então morto com SIGKILL, sem shutdown.
//
// Evidências (PIDs, distribuição do trabalho, logs JSON de cada processo e métricas) ficam em
// DISTRIBUTED_EVIDENCE_DIR (ou num diretório temporário informado no log do teste).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// faults é o estado compartilhado pelos proxies: as armadilhas (de disparo único, qualquer
// processo pode cair nela) e o que cada processo recebeu e enviou ao SQS.
type faults struct {
	mu sync.Mutex
	// holdDeleteOf: messageId do envelope cujo DeleteMessage nunca chega ao SQS.
	holdDeleteOf string
	// holdEventGroup: MessageGroupId do evento cujo envio chega ao SQS, mas cuja resposta não
	// volta ao processo.
	holdEventGroup string
	held           chan heldCall

	receipts   map[string]string // receipt handle -> messageId do envelope
	deliveries map[string][]int  // messageId -> processos que o receberam, em ordem
	sentEvents map[string][]int  // eventId -> processos cujo envio o SQS aceitou
	// orphanedReceives conta, por processo, os ReceiveMessage abandonados no meio porque o
	// processo morreu: o broker ainda pode entregar uma mensagem a eles (ver o reinício completo).
	orphanedReceives map[int]int
}

type heldCall struct {
	process int
	id      string // messageId ou eventId
}

func newFaults() *faults {
	return &faults{
		held: make(chan heldCall, 1), receipts: map[string]string{},
		deliveries: map[string][]int{}, sentEvents: map[string][]int{}, orphanedReceives: map[int]int{},
	}
}

func (f *faults) armDelete(messageID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdDeleteOf = messageID
}

func (f *faults) armEvent(group string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdEventGroup = group
}

func (f *faults) recordReceive(process int, response []byte) {
	var out struct {
		Messages []struct{ ReceiptHandle, Body string }
	}
	if json.Unmarshal(response, &out) != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range out.Messages {
		var envelope struct{ MessageID string }
		_ = json.Unmarshal([]byte(m.Body), &envelope)
		f.receipts[m.ReceiptHandle] = envelope.MessageID
		f.deliveries[envelope.MessageID] = append(f.deliveries[envelope.MessageID], process)
	}
}

// takeDelete dispara a armadilha se o DeleteMessage é da mensagem marcada.
func (f *faults) takeDelete(process int, receipt string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.receipts[receipt]
	if f.holdDeleteOf == "" || id != f.holdDeleteOf {
		return false
	}
	f.holdDeleteOf = ""
	f.held <- heldCall{process: process, id: id}
	return true
}

// recordEvent registra um envio aceito pelo SQS e dispara a armadilha do grupo marcado.
func (f *faults) recordEvent(process int, group, eventID string) (hold bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sentEvents[eventID] = append(f.sentEvents[eventID], process)
	if f.holdEventGroup == "" || group != f.holdEventGroup {
		return false
	}
	f.holdEventGroup = ""
	f.held <- heldCall{process: process, id: eventID}
	return true
}

func (f *faults) recordOrphan(process int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orphanedReceives[process]++
}

func (f *faults) orphans() map[int]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]int{}
	for k, v := range f.orphanedReceives {
		out[k] = v
	}
	return out
}

func (f *faults) deliveriesOf(messageID string) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.deliveries[messageID]...)
}

func (f *faults) sendersOf(eventID string) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.sentEvents[eventID]...)
}

// faultProxy repassa as chamadas SQS (protocolo JSON) de um processo para o LocalStack.
type faultProxy struct {
	process     int
	upstream    string
	eventsQueue string
	faults      *faults
	client      http.Client
}

func (p *faultProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	action := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AmazonSQS.")
	var in struct{ QueueUrl, ReceiptHandle, MessageGroupId, MessageDeduplicationId string }
	_ = json.Unmarshal(body, &in)

	if action == "DeleteMessage" && p.faults.takeDelete(p.process, in.ReceiptHandle) {
		<-r.Context().Done() // o processo morre esperando; a mensagem continua na fila
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.upstream+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := p.client.Do(req)
	if err != nil {
		if action == "ReceiveMessage" && r.Context().Err() != nil {
			p.faults.recordOrphan(p.process)
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	switch {
	case action == "ReceiveMessage" && resp.StatusCode == http.StatusOK:
		p.faults.recordReceive(p.process, respBody)
	case action == "SendMessage" && resp.StatusCode == http.StatusOK && strings.HasSuffix(in.QueueUrl, "/"+p.eventsQueue):
		if p.faults.recordEvent(p.process, in.MessageGroupId, in.MessageDeduplicationId) {
			<-r.Context().Done() // o SQS aceitou o envio; a confirmação nunca chega ao processo
			return
		}
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// process é um processo do sistema operacional executando o binário da API.
type process struct {
	n       int
	pid     int
	base    string
	logPath string
	cmd     *exec.Cmd
	done    chan struct{}
	proxy   *http.Server
}

func (p *process) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

type cluster struct {
	t        *testing.T
	bin      string
	env      environment
	evidence string
	faults   *faults
	summary  *os.File

	mu    sync.Mutex
	procs []*process

	tokensMu sync.Mutex
	tokens   map[string]cachedToken
}

type cachedToken struct {
	value   string
	fetched time.Time
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	dir := os.Getenv("DISTRIBUTED_EVIDENCE_DIR")
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "jungle-distributed-"); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "jungle-api")
	build := exec.Command("go", "build", "-race", "-o", bin, "./cmd/api")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build -race ./cmd/api: %v\n%s", err, out)
	}
	summary, err := os.Create(filepath.Join(dir, "summary.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := &cluster{
		t: t, bin: bin, env: newEnvironment(t), evidence: dir, faults: newFaults(), summary: summary,
		tokens: map[string]cachedToken{},
	}
	t.Cleanup(c.shutdown)
	c.record("evidence directory: %s", dir)
	c.record("binary: %s (go build -race ./cmd/api), test process pid %d", bin, os.Getpid())
	return c
}

func (c *cluster) record(format string, args ...any) {
	c.t.Helper()
	line := fmt.Sprintf(format, args...)
	c.t.Log(line)
	fmt.Fprintf(c.summary, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), line)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// start sobe mais um processo, com proxy SQS próprio, e espera o readiness real.
func (c *cluster) start() *process {
	t := c.t
	t.Helper()
	c.mu.Lock()
	n := len(c.procs) + 1
	c.mu.Unlock()

	proxy := &faultProxy{process: n, upstream: c.env.queues.Config.EndpointURL, eventsQueue: c.env.queues.Config.EventsQueue, faults: c.faults}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := &http.Server{Handler: proxy}
	go func() { _ = proxyServer.Serve(listener) }()

	port := freePort(t)
	q := c.env.queues.Config
	envVars := []string{
		"HTTP_ADDR=127.0.0.1:" + strconv.Itoa(port),
		"DATABASE_URL=" + c.env.dbURL,
		"OIDC_ISSUER_URL=" + oidc.IssuerURL,
		"OIDC_JWKS_URL=" + oidc.JWKSURL,
		"OIDC_AUDIENCE=" + oidc.Audience,
		"AWS_ENDPOINT_URL=http://" + listener.Addr().String(),
		"AWS_REGION=" + q.Region,
		"AWS_ACCESS_KEY_ID=" + os.Getenv("AWS_ACCESS_KEY_ID"),
		"AWS_SECRET_ACCESS_KEY=" + os.Getenv("AWS_SECRET_ACCESS_KEY"),
		"AWS_EC2_METADATA_DISABLED=true",
		"SQS_REQUESTS_QUEUE=" + q.RequestsQueue,
		"SQS_DLQ_QUEUE=" + q.DeadLetterQueue,
		"SQS_EVENTS_QUEUE=" + q.EventsQueue,
		"SQS_TRUSTED_PROVIDERS=" + strings.Join(q.TrustedProviders, ","),
		"SQS_RETRY_BASE_DELAY=1s",
		"SQS_RETRY_MAX_DELAY=4s",
		"REFERENCE_INITIAL_BACKOFF=300ms",
		"REFERENCE_MAX_BACKOFF=1s",
		// Pendências não expiram durante o teste: sobrevivem ao reinício completo.
		"REFERENCE_MAX_ATTEMPTS=100000",
		"REFERENCE_POLL_INTERVAL=100ms",
		"OUTBOX_POLL_INTERVAL=100ms",
	}
	logPath := filepath.Join(c.evidence, fmt.Sprintf("process-%d.log", n))
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(c.bin)
	cmd.Env = envVars
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start process %d: %v", n, err)
	}
	p := &process{n: n, pid: cmd.Process.Pid, base: "http://127.0.0.1:" + strconv.Itoa(port), logPath: logPath, cmd: cmd, done: make(chan struct{}), proxy: proxyServer}
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(p.done)
	}()
	c.mu.Lock()
	c.procs = append(c.procs, p)
	c.mu.Unlock()

	deadline := time.Now().Add(60 * time.Second)
	for {
		if !p.alive() {
			t.Fatalf("process %d (pid %d) exited during startup: %s\n%s", n, p.pid, cmd.ProcessState, tail(logPath))
		}
		resp, err := http.Get(p.base + "/health/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d (pid %d) not ready: %v\n%s", n, p.pid, err, tail(logPath))
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.record("process %d started: pid %d, http %s, sqs proxy %s, log %s", n, p.pid, p.base, listener.Addr(), logPath)
	return p
}

// kill encerra o processo com SIGKILL (sem shutdown) e confirma que foi o sinal que o matou.
func (c *cluster) kill(p *process) {
	c.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		c.t.Fatalf("kill process %d: %v", p.n, err)
	}
	<-p.done
	ws, _ := p.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		c.t.Fatalf("process %d ended with %s, want SIGKILL", p.n, p.cmd.ProcessState)
	}
	c.record("process %d (pid %d) killed: %s", p.n, p.pid, p.cmd.ProcessState)
}

// live devolve os processos em execução, na ordem de criação.
func (c *cluster) live() []*process {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*process
	for _, p := range c.procs {
		if p.alive() {
			out = append(out, p)
		}
	}
	return out
}

// shutdown para os processos restantes com SIGTERM (shutdown gracioso, código 0) e procura
// relatórios do race detector nos logs de todos os processos.
func (c *cluster) shutdown() {
	for _, p := range c.live() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
			if code := p.cmd.ProcessState.ExitCode(); code != 0 {
				c.t.Errorf("process %d (pid %d) exited with %s after SIGTERM", p.n, p.pid, p.cmd.ProcessState)
			}
			c.record("process %d (pid %d) stopped with SIGTERM: %s", p.n, p.pid, p.cmd.ProcessState)
		case <-time.After(30 * time.Second):
			_ = p.cmd.Process.Kill()
			c.t.Errorf("process %d (pid %d) did not stop within 30s", p.n, p.pid)
		}
	}
	// Os proxies fecham só depois dos processos, para o shutdown não ver o SQS sumir.
	for _, p := range c.procs {
		_ = p.proxy.Close()
		raw, _ := os.ReadFile(p.logPath)
		if bytes.Contains(raw, []byte("WARNING: DATA RACE")) {
			c.t.Errorf("race detector report in process %d (%s)", p.n, p.logPath)
		}
	}
	c.summary.Close()
}

func tail(path string) string {
	raw, _ := os.ReadFile(path)
	if len(raw) > 4000 {
		raw = raw[len(raw)-4000:]
	}
	return string(raw)
}

// token obtém (e renova antes de expirar) um access token client_credentials real.
func (c *cluster) token(clientID string) string {
	c.t.Helper()
	c.tokensMu.Lock()
	defer c.tokensMu.Unlock()
	if tok, ok := c.tokens[clientID]; ok && time.Since(tok.fetched) < 2*time.Minute {
		return tok.value
	}
	value, status, err := fetchToken(clientID, clientSecret(c.t, clientID))
	if err != nil || status != http.StatusOK || value == "" {
		c.t.Fatalf("token for %s: status %d, %v", clientID, status, err)
	}
	c.tokens[clientID] = cachedToken{value: value, fetched: time.Now()}
	return value
}

func (c *cluster) openWallet(p *process, amount string) (string, string) {
	c.t.Helper()
	player := uuid.NewString()
	r := call(c.t, http.MethodPost, p.base+"/wallets", c.token("internal-service"),
		map[string]any{"playerId": player, "initialBalance": map[string]string{"amount": amount, "currency": "BRL"}})
	expect(c.t, r, http.StatusCreated, "balance.amount", amount)
	return r.str("id"), player
}

func (c *cluster) submit(p *process, op operation, key string) reply {
	c.t.Helper()
	return call(c.t, http.MethodPost, p.base+"/wagering/transactions", c.token("provider-a"), op, "Idempotency-Key", key)
}

// submitRetrying é o cliente que reenvia a mesma operação, com a mesma chave, a outro processo
// quando a conexão cai (processo morto) ou a resposta é 503. Devolve a resposta final e quantas
// tentativas falharam por transporte. Seguro de chamar fora da goroutine do teste.
func (c *cluster) submitRetrying(first int, op operation, key string, tok string) (reply, int, error) {
	raw, err := json.Marshal(op)
	if err != nil {
		return reply{}, 0, err
	}
	transportFailures := 0
	deadline := time.Now().Add(60 * time.Second)
	for attempt := first; time.Now().Before(deadline); attempt++ {
		c.mu.Lock()
		p := c.procs[attempt%len(c.procs)]
		c.mu.Unlock()
		if !p.alive() {
			continue
		}
		req, _ := http.NewRequest(http.MethodPost, p.base+"/wagering/transactions", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Idempotency-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			transportFailures++
			time.Sleep(100 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		r := reply{status: resp.StatusCode, header: resp.Header}
		if err := json.Unmarshal(body, &r.body); err != nil {
			return reply{}, transportFailures, fmt.Errorf("body %q: %w", body, err)
		}
		return r, transportFailures, nil
	}
	return reply{}, transportFailures, errors.New("no process answered within 60s")
}

// metric lê um valor de GET /metrics de um processo (0 se a série não existe).
func metric(t *testing.T, p *process, series string) float64 {
	t.Helper()
	resp, err := http.Get(p.base + "/metrics")
	if err != nil {
		t.Fatalf("metrics of process %d: %v", p.n, err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), " ")
		if ok && name == series {
			v, _ := strconv.ParseFloat(value, 64)
			return v
		}
	}
	return 0
}

// perProcess lê a mesma série em todos os processos vivos.
func (c *cluster) perProcess(series string) map[int]float64 {
	c.t.Helper()
	out := map[int]float64{}
	for _, p := range c.live() {
		out[p.n] = metric(c.t, p, series)
	}
	return out
}

func delta(after, before map[int]float64) map[int]float64 {
	out := map[int]float64{}
	for n, v := range after {
		out[n] = v - before[n]
	}
	return out
}

func contributors(m map[int]float64) int {
	n := 0
	for _, v := range m {
		if v > 0 {
			n++
		}
	}
	return n
}

// waitLockWaiters espera até n sessões do banco desta execução estarem bloqueadas em lock.
func waitLockWaiters(t *testing.T, db *pgxpool.Pool, n int) {
	t.Helper()
	eventually(t, 4*time.Second, fmt.Sprintf("%d sessions waiting for a lock", n), func() bool {
		return count(t, db, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) >= n
	})
}

func debits(t *testing.T, db *pgxpool.Pool, wallet string) int {
	t.Helper()
	return count(t, db, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`, wallet)
}

func txStatus(t *testing.T, db *pgxpool.Pool, external string) string {
	t.Helper()
	var s string
	_ = db.QueryRow(context.Background(), `SELECT status FROM wager_transactions WHERE external_transaction_id=$1`, external).Scan(&s)
	return s
}

// afterKillRedelivery limita a espera por uma mensagem SQS enviada logo depois de um SIGKILL. O
// long polling do processo morto continua aberto no broker por até 5s, e o LocalStack entrega a
// mensagem a ele (verificado): ela fica invisível até o visibility timeout da fila (60s) e só
// então volta. É atraso de at-least-once, não perda.
const afterKillRedelivery = 60*time.Second + 5*time.Second + 25*time.Second

func runAll(n int, fn func(i int)) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(i)
		}()
	}
	wg.Wait()
}

func TestDistributed_ThreeProcesses(t *testing.T) {
	c := newCluster(t)
	db := c.env.pool(t)
	run := "r" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10] // IDs externos exclusivos desta execução
	id := func(name string) string { return run + "-" + name }

	// Fases em sequência sobre o mesmo cluster: uma falha interrompe o teste, porque as fases
	// seguintes dependem dos processos e dos dados das anteriores.
	phase := func(name string, fn func()) {
		t.Helper()
		c.record("== %s", name)
		fn()
	}

	first := []*process{c.start(), c.start(), c.start()}
	pids := map[int]bool{os.Getpid(): true}
	for _, p := range first {
		if pids[p.pid] {
			t.Fatalf("pid %d is not unique", p.pid)
		}
		pids[p.pid] = true
	}

	phase("two concurrent bets of 80 on 100 across processes", func() {
		wallet, player := c.openWallet(first[0], "100.00")
		release := lockWallet(t, db, wallet)
		replies := make([]reply, 2)
		tok := c.token("provider-a")
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				op := newOperation("provider-a", wallet, player, "BET", id(fmt.Sprintf("bet80-%d", i)), "80.00", "")
				replies[i], _, _ = c.submitRetrying(i+1, op, id(fmt.Sprintf("bet80-%d", i)), tok)
			}()
		}
		// As duas requisições, em processos diferentes, esperam a mesma carteira ao mesmo tempo.
		waitLockWaiters(t, db, 2)
		release()
		wg.Wait()
		statuses := map[string]int{}
		for _, r := range replies {
			statuses[r.str("status")]++
		}
		if statuses["PROCESSED"] != 1 || statuses["REJECTED"] != 1 {
			t.Fatalf("statuses = %v, want one PROCESSED and one REJECTED", statuses)
		}
		if cents := walletCents(t, db, wallet); cents != 2000 || debits(t, db, wallet) != 1 {
			t.Fatalf("wallet = %d, debits = %d; want 2000 and 1", cents, debits(t, db, wallet))
		}
		c.record("80/80: processes 2 and 3 waited on the same wallet lock; result %v, balance 20.00, 1 debit", statuses)
	})

	var replayOp operation
	var replayKey, replayTx string
	phase("fifty equivalent requests spread over three processes", func() {
		wallet, player := c.openWallet(first[1], "100.00")
		replayOp = newOperation("provider-a", wallet, player, "BET", id("bet-50x"), "25.00", "")
		replayKey = id("bet-50x")
		release := lockWallet(t, db, wallet)
		replies := make([]reply, 50)
		tok := c.token("provider-a")
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				replies[i], _, _ = c.submitRetrying(i, replayOp, replayKey, tok)
			}()
		}
		waitLockWaiters(t, db, 3)
		release()
		wg.Wait()
		originals := 0
		for i, r := range replies {
			if r.status != http.StatusOK || r.str("status") != "PROCESSED" || r.str("balance", "amount") != "75.00" {
				t.Fatalf("reply %d = %d %v", i, r.status, r.body)
			}
			if replies[0].str("transactionId") != r.str("transactionId") {
				t.Fatalf("reply %d has transaction %s, want %s", i, r.str("transactionId"), replies[0].str("transactionId"))
			}
			if r.body["idempotentReplay"] == false {
				originals++
			}
		}
		if originals != 1 || debits(t, db, wallet) != 1 || walletCents(t, db, wallet) != 7500 {
			t.Fatalf("originals = %d, debits = %d, wallet = %d; want 1, 1, 7500", originals, debits(t, db, wallet), walletCents(t, db, wallet))
		}
		replayTx = replies[0].str("transactionId")
		c.record("50 equivalent requests (17/17/16 per process): 1 original + 49 replays of transaction %s, 1 debit", replayTx)
	})

	var sqsDuplicateMsg, sqsDuplicateBody, sqsDuplicateWallet string
	phase("duplicates across HTTP and SQS keep the original result", func() {
		wallet, player := c.openWallet(first[2], "100.00")
		// HTTP primeiro (processo 1); depois o mesmo pedido chega duas vezes pelo SQS.
		httpOp := newOperation("provider-a", wallet, player, "BET", id("cross-1"), "10.00", "")
		viaHTTP := c.submit(first[0], httpOp, id("cross-1"))
		expect(t, viaHTTP, http.StatusOK, "status", "PROCESSED", "balance.amount", "90.00")
		body := request(t, id("cross-1-msg"), httpOp, id("cross-1"))
		c.env.queues.Send(t, body, wallet, id("cross-1-msg"))
		c.env.queues.Send(t, body, wallet, id("cross-1-resend")) // reenvio do produtor, mesmo messageId
		eventually(t, 20*time.Second, "both deliveries", func() bool { return c.env.queues.Depth(t, c.env.queues.RequestsURL) == 0 })
		var inboxTx string
		if err := db.QueryRow(context.Background(), `SELECT transaction_id::text FROM inbox_messages WHERE message_id=$1`, id("cross-1-msg")).Scan(&inboxTx); err != nil {
			t.Fatal(err)
		}
		if inboxTx != viaHTTP.str("transactionId") {
			t.Fatalf("inbox transaction = %s, want the HTTP one %s", inboxTx, viaHTTP.str("transactionId"))
		}

		// SQS primeiro; depois a carteira muda e o mesmo pedido chega por HTTP (processo 3):
		// replay com o resultado original (saldo da época), não o saldo atual.
		sqsOp := newOperation("provider-a", wallet, player, "BET", id("cross-2"), "20.00", "")
		sqsDuplicateMsg, sqsDuplicateWallet = id("cross-2-msg"), wallet
		sqsDuplicateBody = request(t, sqsDuplicateMsg, sqsOp, id("cross-2"))
		c.env.queues.Send(t, sqsDuplicateBody, wallet, sqsDuplicateMsg)
		eventually(t, 20*time.Second, "the SQS operation", func() bool { return txStatus(t, db, id("cross-2")) == "PROCESSED" })
		expect(t, c.submit(first[1], newOperation("provider-a", wallet, player, "BET", id("cross-3"), "5.00", ""), id("cross-3")),
			http.StatusOK, "balance.amount", "65.00")
		replay := c.submit(first[2], sqsOp, id("cross-2"))
		expect(t, replay, http.StatusOK, "status", "PROCESSED", "balance.amount", "70.00")
		if replay.body["idempotentReplay"] != true {
			t.Fatalf("HTTP after SQS = %v, want replay", replay.body)
		}
		if debits(t, db, wallet) != 3 || walletCents(t, db, wallet) != 6500 {
			t.Fatalf("debits = %d, wallet = %d; want 3 and 6500", debits(t, db, wallet), walletCents(t, db, wallet))
		}
		c.record("HTTP->SQS x2 and SQS->HTTP: one effect each; replay kept the original balance 70.00 (current 65.00); deliveries of %s: processes %v",
			id("cross-1-msg"), c.faults.deliveriesOf(id("cross-1-msg")))
	})

	phase("independent wallets advance while one wallet is locked", func() {
		locked, lockedPlayer := c.openWallet(first[0], "100.00")
		others := make([][2]string, 9)
		for i := range others {
			others[i][0], others[i][1] = c.openWallet(first[i%3], "100.00")
		}
		tok := c.token("provider-a")
		release := lockWallet(t, db, locked)
		blocked := make(chan reply, 1)
		go func() {
			r, _, _ := c.submitRetrying(0, newOperation("provider-a", locked, lockedPlayer, "BET", id("locked-bet"), "1.00", ""), id("locked-bet"), tok)
			blocked <- r
		}()
		waitLockWaiters(t, db, 1)
		begin := time.Now()
		runAll(len(others), func(i int) {
			r, _, err := c.submitRetrying(i, newOperation("provider-a", others[i][0], others[i][1], "BET", id(fmt.Sprintf("free-%d", i)), "1.00", ""), id(fmt.Sprintf("free-%d", i)), tok)
			if err != nil || r.str("status") != "PROCESSED" {
				t.Errorf("wallet %d: %v %v", i, err, r.body)
			}
		})
		elapsed := time.Since(begin)
		select {
		case r := <-blocked:
			t.Fatalf("the locked wallet was processed while locked: %v", r.body)
		default:
		}
		release()
		if r := <-blocked; r.str("status") != "PROCESSED" {
			t.Fatalf("locked wallet after release = %v", r.body)
		}
		c.record("9 operations on other wallets (3 per process) finished in %v while one wallet stayed locked; it ran after the release", elapsed.Round(time.Millisecond))
	})

	phase("instances compete for SQS, outbox and pending references", func() {
		consumedBefore := c.perProcess(`sqs_messages_total{outcome="completed"}`)
		publishedBefore := c.perProcess(`outbox_events_published_total`)
		resolvedBefore := c.perProcess(`pending_reference_steps_total{status="PROCESSED"}`)

		const n = 30
		wallets := make([][2]string, n)
		for i := range wallets {
			wallets[i][0], wallets[i][1] = c.openWallet(first[i%3], "100.00")
		}
		// Uma mensagem por carteira (grupos FIFO distintos) e um ROLLBACK que chega antes da sua BET.
		for i, w := range wallets {
			op := newOperation("provider-a", w[0], w[1], "BET", id(fmt.Sprintf("sqs-%d", i)), "10.00", "")
			c.env.queues.Send(t, request(t, id(fmt.Sprintf("sqs-%d-msg", i)), op, id(fmt.Sprintf("sqs-%d", i))), w[0], id(fmt.Sprintf("sqs-%d-msg", i)))
			rb := newOperation("provider-a", w[0], w[1], "ROLLBACK", id(fmt.Sprintf("rb-%d", i)), "5.00", id(fmt.Sprintf("late-%d", i)))
			expect(t, c.submit(first[i%3], rb, id(fmt.Sprintf("rb-%d", i))), http.StatusAccepted, "status", "PENDING_REFERENCE")
		}
		tok := c.token("provider-a")
		runAll(n, func(i int) {
			bet := newOperation("provider-a", wallets[i][0], wallets[i][1], "BET", id(fmt.Sprintf("late-%d", i)), "5.00", "")
			if r, _, err := c.submitRetrying(i, bet, id(fmt.Sprintf("late-%d", i)), tok); err != nil || r.str("status") != "PROCESSED" {
				t.Errorf("late bet %d: %v %v", i, err, r.body)
			}
		})
		eventually(t, 60*time.Second, "SQS, pending references and outbox to drain", func() bool {
			return c.env.queues.Depth(t, c.env.queues.RequestsURL) == 0 &&
				count(t, db, `SELECT count(*) FROM wager_transactions WHERE status='PENDING_REFERENCE'`) == 0 &&
				count(t, db, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) == 0
		})
		for i, w := range wallets {
			if txStatus(t, db, id(fmt.Sprintf("rb-%d", i))) != "PROCESSED" || walletCents(t, db, w[0]) != 9000 {
				t.Fatalf("wallet %d: rollback %s, balance %d; want PROCESSED and 9000", i, txStatus(t, db, id(fmt.Sprintf("rb-%d", i))), walletCents(t, db, w[0]))
			}
		}
		consumed := delta(c.perProcess(`sqs_messages_total{outcome="completed"}`), consumedBefore)
		published := delta(c.perProcess(`outbox_events_published_total`), publishedBefore)
		resolved := delta(c.perProcess(`pending_reference_steps_total{status="PROCESSED"}`), resolvedBefore)
		c.record("competition per process: SQS messages completed %v, outbox events published %v, pending references resolved %v", consumed, published, resolved)
		total := 0.0
		for _, v := range resolved {
			total += v
		}
		if total != n {
			t.Fatalf("pending references resolved %v times, want %d", total, n)
		}
		if contributors(consumed) < 2 || contributors(published) < 2 || contributors(resolved) < 2 {
			t.Fatalf("work was not shared: consumed %v, published %v, resolved %v", consumed, published, resolved)
		}
	})

	phase("SIGKILL after commit and before the ACK, during HTTP and SQS work", func() {
		target, targetPlayer := c.openWallet(first[0], "100.00")
		busy, busyPlayer := c.openWallet(first[1], "100.00")
		burst := make([][2]string, 15)
		for i := range burst {
			burst[i][0], burst[i][1] = c.openWallet(first[i%3], "100.00")
		}

		msg := id("crash-ack-msg")
		c.faults.armDelete(msg)
		c.env.queues.Send(t, request(t, msg, newOperation("provider-a", target, targetPlayer, "BET", id("crash-ack"), "30.00", ""), id("crash-ack")), target, msg)
		for i, w := range burst {
			op := newOperation("provider-a", w[0], w[1], "BET", id(fmt.Sprintf("burst-%d", i)), "10.00", "")
			c.env.queues.Send(t, request(t, id(fmt.Sprintf("burst-%d-msg", i)), op, id(fmt.Sprintf("burst-%d", i))), w[0], id(fmt.Sprintf("burst-%d-msg", i)))
		}
		var held heldCall
		select {
		case held = <-c.faults.held:
		case <-time.After(30 * time.Second):
			t.Fatal("no process reached the DeleteMessage of the target message")
		}
		heldAt := time.Now()
		victim := c.procs[held.process-1]
		// Commit feito: a inbox registra a mensagem, mas ela continua na fila.
		if n := count(t, db, `SELECT count(*) FROM inbox_messages WHERE message_id=$1`, msg); n != 1 || walletCents(t, db, target) != 7000 {
			t.Fatalf("before the kill: inbox = %d, wallet = %d; want 1 and 7000", n, walletCents(t, db, target))
		}

		// Requisições HTTP em andamento em todos os processos, inclusive na vítima: esperam o
		// lock de uma carteira dentro da transação quando a vítima morre.
		release := lockWallet(t, db, busy)
		results := make([]reply, 6)
		failures := make([]int, 6)
		var wg sync.WaitGroup
		tok := c.token("provider-a")
		for i := range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				op := newOperation("provider-a", busy, busyPlayer, "BET", id(fmt.Sprintf("inflight-%d", i)), "5.00", "")
				var err error
				// Duas requisições por processo; a primeira tentativa vai para o processo i%3+1.
				results[i], failures[i], err = c.submitRetrying(i, op, id(fmt.Sprintf("inflight-%d", i)), tok)
				if err != nil {
					t.Errorf("in-flight request %d: %v", i, err)
				}
			}()
		}
		waitLockWaiters(t, db, 6)
		c.kill(victim)
		c.record("process %d (pid %d) was holding the DeleteMessage of %s (committed, not acknowledged) for %v when killed, with 2 HTTP requests waiting on a wallet lock",
			victim.n, victim.pid, msg, time.Since(heldAt).Round(time.Millisecond))
		release()
		wg.Wait()
		transport := 0
		for i, r := range results {
			transport += failures[i]
			if r.str("status") != "PROCESSED" {
				t.Fatalf("in-flight request %d = %v", i, r.body)
			}
		}
		if transport < 2 {
			t.Fatalf("transport failures = %d, want at least the 2 requests of the killed process", transport)
		}

		replacement := c.start()
		eventually(t, afterKillRedelivery, "the redelivery of the unacknowledged message (visibility timeout 60s)", func() bool {
			return c.env.queues.Depth(t, c.env.queues.RequestsURL) == 0
		})
		deliveries := c.faults.deliveriesOf(msg)
		if len(deliveries) < 2 || deliveries[len(deliveries)-1] == victim.n {
			t.Fatalf("deliveries of %s = %v, want a redelivery to another process", msg, deliveries)
		}
		if n := count(t, db, `SELECT count(*) FROM inbox_messages WHERE message_id=$1`, msg); n != 1 || walletCents(t, db, target) != 7000 || debits(t, db, target) != 1 {
			t.Fatalf("after the redelivery: inbox = %d, wallet = %d, debits = %d; want 1, 7000, 1", n, walletCents(t, db, target), debits(t, db, target))
		}
		if walletCents(t, db, busy) != 7000 || debits(t, db, busy) != 6 {
			t.Fatalf("busy wallet = %d with %d debits; want 7000 and 6", walletCents(t, db, busy), debits(t, db, busy))
		}
		for i, w := range burst {
			if walletCents(t, db, w[0]) != 9000 || debits(t, db, w[0]) != 1 {
				t.Fatalf("burst wallet %d = %d with %d debits", i, walletCents(t, db, w[0]), debits(t, db, w[0]))
			}
		}
		c.record("recovery: %s delivered to processes %v (the last one found the inbox row and only acknowledged); %d HTTP transport failures retried with the same key; replacement process %d (pid %d)",
			msg, deliveries, transport, replacement.n, replacement.pid)
	})

	phase("SIGKILL after the SQS accepted an event and before the outbox confirmation", func() {
		live := c.live()
		// A carteira é aberta antes de armar: a armadilha mira o primeiro evento da BET seguinte.
		w, player := c.openWallet(live[0], "100.00")
		eventually(t, 20*time.Second, "the opening events", func() bool {
			return count(t, db, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) == 0
		})
		c.faults.armEvent(w)
		expect(t, c.submit(live[1], newOperation("provider-a", w, player, "BET", id("outbox-crash"), "10.00", ""), id("outbox-crash")), http.StatusOK, "status", "PROCESSED")
		var held heldCall
		select {
		case held = <-c.faults.held:
		case <-time.After(30 * time.Second):
			t.Fatal("no publisher sent the event of the target wallet")
		}
		victim := c.procs[held.process-1]
		c.kill(victim)
		var published bool
		if err := db.QueryRow(context.Background(), `SELECT published_at IS NOT NULL FROM outbox_events WHERE event_id=$1`, held.id).Scan(&published); err != nil {
			t.Fatal(err)
		}
		if published {
			t.Fatal("the event was marked published although the confirmation never arrived")
		}

		replacement := c.start()
		eventually(t, 30*time.Second, "republication", func() bool {
			return count(t, db, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) == 0
		})
		senders := c.faults.sendersOf(held.id)
		if len(senders) < 2 || senders[0] != victim.n || senders[len(senders)-1] == victim.n {
			t.Fatalf("senders of event %s = %v, want the killed process and then another one", held.id, senders)
		}
		c.record("outbox: event %s accepted by SQS from process %d (pid %d), killed before the confirmation; republished by processes %v with the same eventId; replacement process %d (pid %d)",
			held.id, victim.n, victim.pid, senders[1:], replacement.n, replacement.pid)
	})

	phase("full restart preserves idempotency and pending references", func() {
		live := c.live()
		wallet, player := c.openWallet(live[0], "100.00")
		pendingHTTP := newOperation("provider-a", wallet, player, "ROLLBACK", id("restart-rb"), "40.00", id("restart-bet-1"))
		pending := c.submit(live[1], pendingHTTP, id("restart-rb"))
		expect(t, pending, http.StatusAccepted, "status", "PENDING_REFERENCE")
		pendingSQS := newOperation("provider-a", wallet, player, "REFUND", id("restart-refund"), "20.00", id("restart-bet-2"))
		c.env.queues.Send(t, request(t, id("restart-refund-msg"), pendingSQS, id("restart-refund")), wallet, id("restart-refund-msg"))
		sentAt := time.Now()
		eventually(t, afterKillRedelivery, "the SQS pending reference", func() bool { return txStatus(t, db, id("restart-refund")) == "PENDING_REFERENCE" })
		c.record("SQS pending reference sent right after the previous SIGKILL was consumed after %v", time.Since(sentAt).Round(100*time.Millisecond))
		inboxBefore := count(t, db, `SELECT count(*) FROM inbox_messages`)

		for _, p := range live {
			c.kill(p)
		}
		if txStatus(t, db, id("restart-rb")) != "PENDING_REFERENCE" || txStatus(t, db, id("restart-refund")) != "PENDING_REFERENCE" {
			t.Fatal("pending references did not survive the processes")
		}
		restarted := []*process{c.start(), c.start(), c.start()}
		for _, p := range restarted {
			if pids[p.pid] {
				t.Fatalf("pid %d reused", p.pid)
			}
			pids[p.pid] = true
		}

		// Idempotência depois do reinício: replays por HTTP em cada processo novo e reenvio SQS.
		for _, p := range restarted {
			r := c.submit(p, replayOp, replayKey)
			expect(t, r, http.StatusOK, "status", "PROCESSED", "transactionId", replayTx, "balance.amount", "75.00")
			if r.body["idempotentReplay"] != true {
				t.Fatalf("replay on process %d = %v", p.n, r.body)
			}
			expect(t, c.submit(p, pendingHTTP, id("restart-rb")), http.StatusAccepted, "status", "PENDING_REFERENCE", "transactionId", pending.str("transactionId"))
		}
		deliveredBefore := len(c.faults.deliveriesOf(sqsDuplicateMsg))
		c.env.queues.Send(t, sqsDuplicateBody, sqsDuplicateWallet, id("cross-2-after-restart"))
		resentAt := time.Now()
		eventually(t, afterKillRedelivery, "the resent message", func() bool { return c.env.queues.Depth(t, c.env.queues.RequestsURL) == 0 })
		c.record("message resent after the restart was acknowledged after %v by process %v; long polls abandoned by killed processes so far: %v",
			time.Since(resentAt).Round(100*time.Millisecond), c.faults.deliveriesOf(sqsDuplicateMsg)[deliveredBefore:], c.faults.orphans())
		if n := count(t, db, `SELECT count(*) FROM inbox_messages`); n != inboxBefore {
			t.Fatalf("inbox rows = %d, want %d (the resent message is a duplicate)", n, inboxBefore)
		}
		if walletCents(t, db, sqsDuplicateWallet) != 6500 {
			t.Fatalf("wallet = %d, want 6500", walletCents(t, db, sqsDuplicateWallet))
		}

		// As referências chegam agora; os workers dos processos novos resolvem as pendências.
		expect(t, c.submit(restarted[0], newOperation("provider-a", wallet, player, "BET", id("restart-bet-1"), "40.00", ""), id("restart-bet-1")), http.StatusOK, "balance.amount", "60.00")
		eventually(t, 30*time.Second, "the HTTP pending reference resolved after the restart", func() bool {
			return txStatus(t, db, id("restart-rb")) == "PROCESSED"
		})
		expect(t, c.submit(restarted[1], newOperation("provider-a", wallet, player, "BET", id("restart-bet-2"), "20.00", ""), id("restart-bet-2")), http.StatusOK, "balance.amount", "80.00")
		eventually(t, 30*time.Second, "the SQS pending reference resolved after the restart", func() bool {
			return txStatus(t, db, id("restart-refund")) == "PROCESSED"
		})
		if walletCents(t, db, wallet) != 10000 {
			t.Fatalf("wallet = %d, want 10000", walletCents(t, db, wallet))
		}
		c.record("full restart: all processes SIGKILLed; new pids %d, %d, %d replayed the original results and resolved the 2 pending references",
			restarted[0].pid, restarted[1].pid, restarted[2].pid)
	})

	phase("final consistency of balances, ledger and versions", func() {
		eventually(t, 30*time.Second, "outbox drained", func() bool {
			return count(t, db, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) == 0
		})
		checks := map[string]string{
			"wallets whose balance differs from credits minus debits": `
				SELECT count(*) FROM wallets w WHERE w.balance <> (
					SELECT COALESCE(sum(CASE direction WHEN 'CREDIT' THEN amount ELSE -amount END), 0)
					FROM wallet_ledger_entries e WHERE e.wallet_id = w.id)`,
			"wallets whose version is not the last ledger version": `
				SELECT count(*) FROM wallets w WHERE w.version <> COALESCE(
					(SELECT max(wallet_version) FROM wallet_ledger_entries e WHERE e.wallet_id = w.id), 1)`,
			"wallets with gaps or repeats in the ledger versions": `
				SELECT count(*) FROM (SELECT wallet_id FROM wallet_ledger_entries GROUP BY wallet_id
					HAVING count(*) <> count(DISTINCT wallet_version) OR max(wallet_version) - min(wallet_version) + 1 <> count(*)) g`,
			"transactions with more than one ledger entry": `
				SELECT count(*) FROM (SELECT transaction_id FROM wallet_ledger_entries GROUP BY 1 HAVING count(*) > 1) d`,
			"external operations recorded more than once": `
				SELECT count(*) FROM (SELECT provider_id, external_transaction_id FROM wager_transactions
					WHERE external_transaction_id IS NOT NULL GROUP BY 1, 2 HAVING count(*) > 1) d`,
			"negative balances":       `SELECT count(*) FROM wallets WHERE balance < 0`,
			"pending references left": `SELECT count(*) FROM wager_transactions WHERE status = 'PENDING_REFERENCE'`,
		}
		names := make([]string, 0, len(checks))
		for name := range checks {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if n := count(t, db, checks[name]); n != 0 {
				t.Errorf("%s: %d", name, n)
			}
		}
		live := c.live()
		rows, err := db.Query(context.Background(), `SELECT id::text FROM wallets ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		var wallets []string
		for rows.Next() {
			var w string
			_ = rows.Scan(&w)
			wallets = append(wallets, w)
		}
		rows.Close()
		for i, w := range wallets {
			r := call(t, http.MethodPost, live[i%len(live)].base+"/wallets/"+w+"/reconciliation", c.token("internal-service"), nil)
			if r.status != http.StatusOK || r.body["consistent"] != true {
				t.Fatalf("reconciliation of %s = %d %v", w, r.status, r.body)
			}
		}
		if n := c.env.queues.Depth(t, c.env.queues.DeadLetterURL); n != 0 {
			t.Errorf("DLQ has %d messages, want 0", n)
		}
		events := map[string]int{}
		for _, m := range c.env.queues.Drain(t, c.env.queues.EventsURL, 5*time.Second) {
			events[aws.ToString(m.MessageAttributes["eventId"].StringValue)]++
		}
		outboxEvents := count(t, db, `SELECT count(*) FROM outbox_events`)
		duplicated := 0
		for _, n := range events {
			if n > 1 {
				duplicated++
			}
		}
		if len(events) != outboxEvents {
			t.Errorf("events queue has %d distinct event ids, outbox has %d events", len(events), outboxEvents)
		}
		var ledgerEntries, transactions int
		_ = db.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM wallet_ledger_entries), (SELECT count(*) FROM wager_transactions)`).Scan(&ledgerEntries, &transactions)
		c.record("final: %d wallets reconciled through HTTP (consistent), %d transactions, %d ledger entries, %d outbox events all delivered (%d event ids delivered more than once), DLQ empty",
			len(wallets), transactions, ledgerEntries, outboxEvents, duplicated)
		for _, p := range live {
			c.record("process %d (pid %d) metrics: sqs completed %v, duplicates sqs_inbox %v, http replays %v, outbox published %v, worker errors consumer %v publisher %v references %v",
				p.n, p.pid, metric(t, p, `sqs_messages_total{outcome="completed"}`), metric(t, p, `wagering_duplicates_total{kind="sqs_inbox"}`),
				metric(t, p, `wagering_duplicates_total{kind="http"}`), metric(t, p, `outbox_events_published_total`),
				metric(t, p, `worker_errors_total{worker="consumer"}`), metric(t, p, `worker_errors_total{worker="publisher"}`), metric(t, p, `worker_errors_total{worker="references"}`))
		}
	})
}
