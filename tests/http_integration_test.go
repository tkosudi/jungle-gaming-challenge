//go:build integration

// Package tests exercita a aplicação composta pelo Fx, ponta a ponta: HTTP real, PostgreSQL
// real (banco descartável com as migrations) e tokens emitidos pelo Keycloak do Compose.
package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/fx"

	"github.com/tkosudi/jungle-gaming-challenge/internal/app"
	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
	"github.com/tkosudi/jungle-gaming-challenge/internal/httpapi"
	"github.com/tkosudi/jungle-gaming-challenge/internal/testenv"
)

func env(key, fallback string) string {
	if v := testenv.Lookup(key); v != "" {
		return v
	}
	return fallback
}

var (
	adminDatabaseURL = testenv.DatabaseURL()
	keycloakURL      = env("TEST_KEYCLOAK_URL", "http://localhost:"+env("KEYCLOAK_PORT", "8180"))
	oidc             = config.OIDC{
		IssuerURL: keycloakURL + "/realms/jungle",
		JWKSURL:   keycloakURL + "/realms/jungle/protocol/openid-connect/certs",
		Audience:  "wagering-api",
	}
)

// secretEnv aponta, por cliente, a variável que guarda seu client secret. Os segredos ficam
// no ambiente ou no .env local (o mesmo que o Compose usa para configurar o Keycloak), nunca
// no código.
var secretEnv = map[string]string{
	"provider-a":          "PROVIDER_A_CLIENT_SECRET",
	"provider-b":          "PROVIDER_B_CLIENT_SECRET",
	"internal-service":    "INTERNAL_SERVICE_CLIENT_SECRET",
	"test-other-audience": "TEST_OTHER_AUDIENCE_CLIENT_SECRET",
	"test-short-lived":    "TEST_SHORT_LIVED_CLIENT_SECRET",
}

func clientSecret(t *testing.T, clientID string) string {
	t.Helper()
	secret := testenv.Lookup(secretEnv[clientID])
	if secret == "" {
		t.Fatalf("%s is not set: define it in the environment or in .env (see .env.example)", secretEnv[clientID])
	}
	return secret
}

// newDatabase cria um banco descartável e aplica as migrations; o banco de desenvolvimento
// nunca é alterado.
func newDatabase(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDatabaseURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)
	name := "m2_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminDatabaseURL)
		if err != nil {
			t.Errorf("connect admin for cleanup: %v", err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	u, _ := url.Parse(adminDatabaseURL)
	u.Path = "/" + name
	dbURL := u.String()

	dir, _ := filepath.Abs("../db/migrations")
	mu := *u
	mu.Scheme = "pgx5"
	m, err := migrate.New("file://"+dir, mu.String())
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return dbURL
}

// environment é o que as instâncias de um teste compartilham: banco descartável e filas SQS
// exclusivas no LocalStack.
type environment struct {
	dbURL  string
	queues testenv.Queues
}

func newEnvironment(t *testing.T) environment {
	t.Helper()
	return environment{dbURL: newDatabase(t), queues: testenv.SQSQueues(t, 5)}
}

// instanceConfig é a configuração de uma instância de teste: workers com intervalos curtos.
func (e environment) instanceConfig() config.Config {
	return config.Config{
		HTTPAddr: "127.0.0.1:0", DatabaseURL: e.dbURL, OIDC: oidc, SQS: e.queues.Config,
		Workers: config.Workers{
			ReferenceInitialBackoff: 100 * time.Millisecond, ReferenceMaxBackoff: time.Second, ReferenceMaxAttempts: 10,
			ReferencePollInterval: 100 * time.Millisecond, OutboxPollInterval: 100 * time.Millisecond,
		},
	}
}

// startInstance sobe uma instância completa (Fx: HTTP, consumidor, publicador e worker de
// referências) com pool próprio e devolve sua URL base. Para no fim do teste.
func startInstance(t *testing.T, e environment) string {
	t.Helper()
	base, stop := startConfigured(t, e.instanceConfig())
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	return base
}

// startConfigured sobe a instância e devolve a função que a para (com prazo de shutdown).
func startConfigured(t *testing.T, cfg config.Config) (string, func() error) {
	t.Helper()
	var srv *httpapi.Server
	a := app.New(cfg, fx.NopLogger, fx.Populate(&srv))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	var once sync.Once
	var stopErr error
	stop := func() error {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			stopErr = a.Stop(ctx)
		})
		return stopErr
	}
	return "http://" + srv.Addr().String(), stop
}

var (
	tokensMu sync.Mutex
	tokens   = map[string]string{}
)

// fetchToken obtém um access token client_credentials no Keycloak real.
func fetchToken(clientID, secret string) (string, int, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {secret}}
	resp, err := http.PostForm(keycloakURL+"/realms/jungle/protocol/openid-connect/token", form)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", resp.StatusCode, err
	}
	return body.AccessToken, resp.StatusCode, nil
}

func token(t *testing.T, clientID string) string {
	t.Helper()
	tokensMu.Lock()
	defer tokensMu.Unlock()
	if tok, ok := tokens[clientID]; ok {
		return tok
	}
	tok, status, err := fetchToken(clientID, clientSecret(t, clientID))
	if err != nil || status != http.StatusOK || tok == "" {
		t.Fatalf("token for %s: status %d, %v", clientID, status, err)
	}
	tokens[clientID] = tok
	return tok
}

type reply struct {
	status int
	header http.Header
	body   map[string]any
}

func call(t *testing.T, method, target, tok string, body any, headers ...string) reply {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, header: resp.Header}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &r.body); err != nil {
			t.Fatalf("%s %s: body %q: %v", method, target, raw, err)
		}
	}
	return r
}

func (r reply) str(path ...string) string {
	var cur any = r.body
	for _, p := range path {
		m, _ := cur.(map[string]any)
		cur = m[p]
	}
	s, _ := cur.(string)
	return s
}

func expect(t *testing.T, r reply, status int, checks ...string) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d: %v", r.status, status, r.body)
	}
	for i := 0; i+1 < len(checks); i += 2 {
		path := strings.Split(checks[i], ".")
		if got := r.str(path...); got != checks[i+1] {
			t.Fatalf("%s = %q, want %q (body %v)", checks[i], got, checks[i+1], r.body)
		}
	}
}

type operation map[string]any

func newOperation(provider, wallet, player, kind, external, amount, reference string) operation {
	op := operation{
		"providerId": provider, "externalTransactionId": external, "playerId": player, "walletId": wallet,
		"roundId": "round-1", "gameId": "game-1", "kind": kind,
		"money": map[string]string{"amount": amount, "currency": "BRL"},
	}
	if reference != "" {
		op["referenceExternalTransactionId"] = reference
	}
	return op
}

// openWallet abre uma carteira pelo endpoint interno e devolve (walletId, playerId).
func openWallet(t *testing.T, base, amount string) (string, string) {
	t.Helper()
	player := uuid.NewString()
	r := call(t, http.MethodPost, base+"/wallets", token(t, "internal-service"),
		map[string]any{"playerId": player, "initialBalance": map[string]string{"amount": amount, "currency": "BRL"}})
	expect(t, r, http.StatusCreated, "playerId", player, "balance.amount", amount)
	return r.str("id"), player
}

func submit(t *testing.T, base, client string, op operation, key string) reply {
	t.Helper()
	return call(t, http.MethodPost, base+"/wagering/transactions", token(t, client), op, "Idempotency-Key", key)
}

func TestHTTP_WalletAndWageringFlow(t *testing.T) {
	base := startInstance(t, newEnvironment(t))
	internal := token(t, "internal-service")
	wallet, player := openWallet(t, base, "100.00")

	again := call(t, http.MethodPost, base+"/wallets", internal,
		map[string]any{"playerId": player, "initialBalance": map[string]string{"amount": "5.00", "currency": "BRL"}})
	expect(t, again, http.StatusConflict, "error.code", "WALLET_ALREADY_EXISTS")

	bet := newOperation("provider-a", wallet, player, "BET", "bet-1", "30.00", "")
	first := submit(t, base, "provider-a", bet, "provider-a:bet-1")
	expect(t, first, http.StatusOK, "status", "PROCESSED", "balance.amount", "70.00", "balance.currency", "BRL")
	if first.body["idempotentReplay"] != false {
		t.Fatalf("first response = %v", first.body)
	}
	replay := submit(t, base, "provider-a", bet, "provider-a:bet-1")
	expect(t, replay, http.StatusOK, "status", "PROCESSED", "balance.amount", "70.00", "transactionId", first.str("transactionId"))
	if replay.body["idempotentReplay"] != true {
		t.Fatalf("replay = %v", replay.body)
	}
	changed := newOperation("provider-a", wallet, player, "BET", "bet-1", "31.00", "")
	expect(t, submit(t, base, "provider-a", changed, "provider-a:bet-1"), http.StatusConflict, "error.code", "IDEMPOTENCY_CONFLICT")

	rejected := submit(t, base, "provider-a", newOperation("provider-a", wallet, player, "BET", "bet-big", "500.00", ""), "provider-a:bet-big")
	expect(t, rejected, http.StatusUnprocessableEntity, "status", "REJECTED", "failureCode", "INSUFFICIENT_FUNDS", "balance.amount", "70.00")

	pending := submit(t, base, "provider-a", newOperation("provider-a", wallet, player, "ROLLBACK", "rb-1", "5.00", "bet-not-yet"), "provider-a:rb-1")
	expect(t, pending, http.StatusAccepted, "status", "PENDING_REFERENCE")
	location := pending.header.Get("Location")
	tracked := call(t, http.MethodGet, base+location, token(t, "provider-a"), nil)
	expect(t, tracked, http.StatusOK, "status", "PENDING_REFERENCE", "referenceExternalTransactionId", "bet-not-yet")
	if tracked.str("nextAttemptAt") == "" {
		t.Fatalf("pending transaction without nextAttemptAt: %v", tracked.body)
	}

	// Isolamento: o provider-b não enxerga nem consulta transações do provider-a.
	expect(t, call(t, http.MethodGet, base+location, token(t, "provider-b"), nil), http.StatusNotFound, "error.code", "TRANSACTION_NOT_FOUND")
	expect(t, call(t, http.MethodGet, base+"/providers/provider-a/wagering/transactions/bet-1", token(t, "provider-b"), nil), http.StatusForbidden)
	expect(t, call(t, http.MethodGet, base+"/providers/provider-a/wagering/transactions/bet-1", token(t, "provider-a"), nil),
		http.StatusOK, "status", "PROCESSED", "balance.amount", "70.00", "providerId", "provider-a")

	// O mesmo ID externo no namespace do provider-b é outra operação, não um replay.
	other := submit(t, base, "provider-b", newOperation("provider-b", wallet, player, "BET", "bet-1", "10.00", ""), "provider-a:bet-1")
	expect(t, other, http.StatusOK, "status", "PROCESSED", "balance.amount", "60.00")
	if other.body["idempotentReplay"] != false || other.str("transactionId") == first.str("transactionId") {
		t.Fatalf("provider-b operation = %v", other.body)
	}

	expect(t, submit(t, base, "provider-a", newOperation("provider-a", wallet, player, "REFUND", "refund-1", "30.00", "bet-1"), "provider-a:refund-1"),
		http.StatusOK, "status", "PROCESSED", "balance.amount", "90.00")
	expect(t, submit(t, base, "provider-a", newOperation("provider-a", wallet, player, "LOSS", "loss-1", "0.00", ""), "provider-a:loss-1"),
		http.StatusOK, "status", "PROCESSED", "balance.amount", "90.00")

	got := call(t, http.MethodGet, base+"/wallets/"+wallet, internal, nil)
	expect(t, got, http.StatusOK, "balance.amount", "90.00", "id", wallet)
	if got.body["version"] != float64(4) {
		t.Fatalf("wallet = %v, want version 4", got.body)
	}

	var versions []float64
	next := base + "/wallets/" + wallet + "/ledger?limit=3"
	for page := 0; next != "" && page < 5; page++ {
		r := call(t, http.MethodGet, next, internal, nil)
		expect(t, r, http.StatusOK, "walletId", wallet)
		for _, e := range r.body["entries"].([]any) {
			versions = append(versions, e.(map[string]any)["walletVersion"].(float64))
		}
		next = ""
		if c, ok := r.body["nextCursor"].(string); ok {
			next = base + "/wallets/" + wallet + "/ledger?limit=3&cursor=" + url.QueryEscape(c)
		}
	}
	if fmt.Sprint(versions) != "[1 2 3 4]" {
		t.Fatalf("ledger versions = %v, want [1 2 3 4]", versions)
	}
}

func TestHTTP_ProviderIdentityComesOnlyFromTheToken(t *testing.T) {
	base := startInstance(t, newEnvironment(t))
	wallet, player := openWallet(t, base, "100.00")

	spoofed := newOperation("provider-b", wallet, player, "BET", "bet-1", "10.00", "")
	expect(t, submit(t, base, "provider-a", spoofed, "k-1"), http.StatusForbidden, "error.code", "PROVIDER_MISMATCH")
	expect(t, call(t, http.MethodGet, base+"/providers/provider-b/wagering/transactions/bet-1", token(t, "provider-b"), nil), http.StatusNotFound)

	// Um header arbitrário não muda o provedor: a operação é do provider-a, dono do token.
	op := newOperation("provider-a", wallet, player, "BET", "bet-2", "10.00", "")
	expect(t, call(t, http.MethodPost, base+"/wagering/transactions", token(t, "provider-a"), op,
		"Idempotency-Key", "k-2", "X-Provider-Id", "provider-b"), http.StatusOK, "status", "PROCESSED")
	expect(t, call(t, http.MethodGet, base+"/providers/provider-a/wagering/transactions/bet-2", token(t, "provider-a"), nil), http.StatusOK)
	expect(t, call(t, http.MethodGet, base+"/providers/provider-b/wagering/transactions/bet-2", token(t, "provider-b"), nil), http.StatusNotFound)
}

func TestHTTP_AuthenticationWithRealKeycloak(t *testing.T) {
	base := startInstance(t, newEnvironment(t))
	probe := base + "/wagering/transactions/" + uuid.NewString() // 404 prova que a autenticação passou

	expect(t, call(t, http.MethodGet, probe, token(t, "provider-a"), nil), http.StatusNotFound, "error.code", "TRANSACTION_NOT_FOUND")
	expect(t, call(t, http.MethodGet, probe, "", nil), http.StatusUnauthorized, "error.code", "UNAUTHENTICATED")
	expect(t, call(t, http.MethodGet, probe, "not-a-token", nil), http.StatusUnauthorized, "error.code", "UNAUTHENTICATED")

	real := token(t, "provider-a")
	parts := strings.Split(real, ".")
	sig := []byte(parts[2])
	sig[len(sig)/2] ^= 0x01
	badSignature := parts[0] + "." + parts[1] + "." + string(sig)
	expect(t, call(t, http.MethodGet, probe, badSignature, nil), http.StatusUnauthorized, "error.code", "UNAUTHENTICATED")
	// Trocar o provider_id do payload de um token real invalida a assinatura.
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.Replace(string(payload), `"provider_id":"provider-a"`, `"provider_id":"provider-b"`, 1)
	if forged == string(payload) {
		t.Fatal("token has no provider_id claim to forge")
	}
	forgedToken := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(forged)) + "." + parts[2]
	expect(t, call(t, http.MethodGet, probe, forgedToken, nil), http.StatusUnauthorized, "error.code", "UNAUTHENTICATED")

	expect(t, call(t, http.MethodGet, probe, token(t, "test-other-audience"), nil), http.StatusUnauthorized, "error.code", "UNAUTHENTICATED")

	if _, status, _ := fetchToken("provider-a", "wrong-secret"); status != http.StatusUnauthorized {
		t.Fatalf("Keycloak accepted a wrong client secret: status %d", status)
	}

	// Cliente com access token de 2s: válido agora, rejeitado depois de expirar.
	short, status, err := fetchToken("test-short-lived", clientSecret(t, "test-short-lived"))
	if err != nil || status != http.StatusOK {
		t.Fatalf("short-lived token: %d %v", status, err)
	}
	expect(t, call(t, http.MethodGet, probe, short, nil), http.StatusNotFound)
	time.Sleep(3 * time.Second)
	expect(t, call(t, http.MethodGet, probe, short, nil), http.StatusUnauthorized, "error.code", "UNAUTHENTICATED")

	wallet, player := openWallet(t, base, "10.00")
	expect(t, call(t, http.MethodPost, base+"/wallets", token(t, "provider-a"),
		map[string]any{"playerId": uuid.NewString(), "initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"}}),
		http.StatusForbidden, "error.code", "FORBIDDEN")
	expect(t, call(t, http.MethodGet, base+"/wallets/"+wallet, token(t, "provider-a"), nil), http.StatusForbidden)
	expect(t, submit(t, base, "internal-service", newOperation("provider-a", wallet, player, "BET", "b-1", "1.00", ""), "k-1"),
		http.StatusForbidden, "error.code", "FORBIDDEN")
}

func TestHTTP_ConcurrencyAcrossIndependentInstances(t *testing.T) {
	env := newEnvironment(t)
	instances := []string{startInstance(t, env), startInstance(t, env), startInstance(t, env)}
	wallet, player := openWallet(t, instances[0], "100.00")
	token(t, "provider-a")

	results := make([]reply, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op := newOperation("provider-a", wallet, player, "BET", fmt.Sprintf("bet-80-%d", i), "80.00", "")
			results[i] = submit(t, instances[i], "provider-a", op, fmt.Sprintf("k-80-%d", i))
		}()
	}
	wg.Wait()
	statuses := map[int]int{}
	for _, r := range results {
		statuses[r.status]++
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusUnprocessableEntity] != 1 {
		t.Fatalf("statuses = %v, want one 200 and one 422 (%v)", statuses, results)
	}

	const requests = 30
	same := newOperation("provider-a", wallet, player, "BET", "bet-same", "10.00", "")
	replies := make([]reply, requests)
	for i := range replies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replies[i] = submit(t, instances[i%len(instances)], "provider-a", same, "k-same")
		}()
	}
	wg.Wait()
	originals := 0
	for _, r := range replies {
		expect(t, r, http.StatusOK, "status", "PROCESSED", "balance.amount", "10.00", "transactionId", replies[0].str("transactionId"))
		if r.body["idempotentReplay"] == false {
			originals++
		}
	}
	if originals != 1 {
		t.Fatalf("non-replay responses = %d, want 1", originals)
	}

	ledger := call(t, http.MethodGet, instances[2]+"/wallets/"+wallet+"/ledger", token(t, "internal-service"), nil)
	expect(t, ledger, http.StatusOK)
	if n := len(ledger.body["entries"].([]any)); n != 3 {
		t.Fatalf("ledger entries = %d, want opening + one 80.00 debit + one 10.00 debit", n)
	}
	expect(t, call(t, http.MethodGet, instances[1]+"/wallets/"+wallet, token(t, "internal-service"), nil), http.StatusOK, "balance.amount", "10.00")
}

func TestHTTP_Reconciliation(t *testing.T) {
	base := startInstance(t, newEnvironment(t))
	internal := token(t, "internal-service")
	wallet, player := openWallet(t, base, "1000.00")
	submit(t, base, "provider-a", newOperation("provider-a", wallet, player, "BET", "bet-1", "25.00", ""), "provider-a:bet-1")

	r := call(t, http.MethodPost, base+"/wallets/"+wallet+"/reconciliation", internal, nil)
	expect(t, r, http.StatusOK, "walletId", wallet,
		"storedBalance.amount", "975.00", "storedBalance.currency", "BRL",
		"calculatedBalance.amount", "975.00", "difference.amount", "0.00", "difference.currency", "BRL")
	if r.body["consistent"] != true || r.body["checkedEntries"] != float64(2) || len(r.body) != 6 {
		t.Fatalf("reconciliation = %v", r.body)
	}

	expect(t, call(t, http.MethodPost, base+"/wallets/"+wallet+"/reconciliation", token(t, "provider-a"), nil), http.StatusForbidden, "error.code", "FORBIDDEN")
	expect(t, call(t, http.MethodPost, base+"/wallets/"+wallet+"/reconciliation", "", nil), http.StatusUnauthorized)
	expect(t, call(t, http.MethodPost, base+"/wallets/"+uuid.NewString()+"/reconciliation", internal, nil), http.StatusNotFound, "error.code", "WALLET_NOT_FOUND")

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `wallet_reconciliations_total{result="consistent"}`) {
		t.Fatalf("metrics without the reconciliation counter:\n%s", raw)
	}
}
