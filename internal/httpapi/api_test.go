package httpapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/gofiber/fiber/v3"

	"github.com/tkosudi/jungle-gaming-challenge/internal/auth"
	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
)

// Estes testes cobrem o que o transporte decide sozinho, antes de chegar ao Service:
// autenticação, autorização, validação e contrato de erro. O Service é nil de propósito:
// se algum desses caminhos o alcançasse, o teste entraria em pânico. Os fluxos completos,
// com PostgreSQL e Keycloak reais, estão em tests/.

const (
	testIssuer   = "http://localhost:8180/realms/jungle"
	testAudience = "wagering-api"
)

type testIDP struct {
	key    *rsa.PrivateKey
	server *httptest.Server
}

func newTestIDP(t *testing.T) *testIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &testIDP{key: key}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *testIDP) token(t *testing.T, providerID string, roles ...string) string {
	t.Helper()
	claims := map[string]any{
		"iss": testIssuer, "aud": testAudience, "sub": "service-account", "azp": "client", "typ": "Bearer",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(),
		"realm_access": map[string]any{"roles": roles},
	}
	if providerID != "" {
		claims["provider_id"] = providerID
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func readiness(db pinger) []ReadinessCheck {
	return []ReadinessCheck{{Name: "postgres", Check: db.Ping}, {Name: "sqs", Check: func(context.Context) error { return nil }}}
}

func newTestApp(t *testing.T, p *testIDP) *fiber.App {
	t.Helper()
	jwks := "http://127.0.0.1:1/unreachable"
	if p != nil {
		jwks = p.server.URL
	}
	verifier := auth.NewVerifier(config.OIDC{IssuerURL: testIssuer, JWKSURL: jwks, Audience: testAudience})
	return NewApp(nil, verifier, readiness(pinger{}))
}

type response struct {
	status int
	header http.Header
	body   map[string]any
}

func do(t *testing.T, app *fiber.App, method, path, token, body string, headers ...string) response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode, header: resp.Header}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			t.Fatalf("%s %s: body %q is not JSON: %v", method, path, raw, err)
		}
	}
	return out
}

func (r response) code() string {
	e, _ := r.body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func requireError(t *testing.T, r response, status int, code string) {
	t.Helper()
	if r.status != status || r.code() != code {
		t.Fatalf("response = %d %v, want %d %s", r.status, r.body, status, code)
	}
}

const validOperation = `{"providerId":"provider-a","externalTransactionId":"tx-1","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
	"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","roundId":"r-1","gameId":"g-1","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}`

func TestHealthReady(t *testing.T) {
	ok := do(t, NewApp(nil, nil, readiness(pinger{})), http.MethodGet, "/health/ready", "", "")
	if ok.status != http.StatusOK || ok.body["status"] != "ok" {
		t.Fatalf("ready = %d %v", ok.status, ok.body)
	}
	down := do(t, NewApp(nil, nil, readiness(pinger{err: errors.New("down")})), http.MethodGet, "/health/ready", "", "")
	checks, _ := down.body["checks"].(map[string]any)
	if down.status != http.StatusServiceUnavailable || down.body["status"] != "unavailable" || checks["postgres"] != "unavailable" || checks["sqs"] != "ok" {
		t.Fatalf("ready with database down = %d %v", down.status, down.body)
	}
	if checks, _ := ok.body["checks"].(map[string]any); checks["postgres"] != "ok" || checks["sqs"] != "ok" {
		t.Fatalf("ready checks = %v", ok.body)
	}
}

func TestAuthentication(t *testing.T) {
	p := newTestIDP(t)
	app := newTestApp(t, p)
	routes := [][2]string{
		{http.MethodPost, "/wagering/transactions"},
		{http.MethodGet, "/wagering/transactions/0192f298-345e-7e38-af88-e43f851a819d"},
		{http.MethodGet, "/providers/provider-a/wagering/transactions/tx-1"},
		{http.MethodPost, "/wallets"},
		{http.MethodGet, "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37"},
		{http.MethodGet, "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37/ledger"},
		{http.MethodPost, "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37/reconciliation"},
	}
	for _, route := range routes {
		t.Run(route[0]+" "+route[1], func(t *testing.T) {
			missing := do(t, app, route[0], route[1], "", "")
			requireError(t, missing, http.StatusUnauthorized, "UNAUTHENTICATED")
			if !strings.HasPrefix(missing.header.Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("WWW-Authenticate = %q", missing.header.Get("WWW-Authenticate"))
			}
			requireError(t, do(t, app, route[0], route[1], "", "", "Authorization", "Basic dXNlcjpwYXNz"), http.StatusUnauthorized, "UNAUTHENTICATED")
			requireError(t, do(t, app, route[0], route[1], "garbage", ""), http.StatusUnauthorized, "UNAUTHENTICATED")
		})
	}

	t.Run("signing keys unavailable is a 503, not a 401", func(t *testing.T) {
		r := do(t, newTestApp(t, nil), http.MethodGet, "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37", p.token(t, "", auth.RoleWalletOperator), "")
		requireError(t, r, http.StatusServiceUnavailable, "IDP_UNAVAILABLE")
	})
}

func TestAuthorization(t *testing.T) {
	p := newTestIDP(t)
	app := newTestApp(t, p)
	provider := p.token(t, "provider-a", auth.RoleWageringProvider)
	internal := p.token(t, "", auth.RoleWalletOperator)
	roleWithoutProvider := p.token(t, "", auth.RoleWageringProvider)
	providerWithoutRole := p.token(t, "provider-a")

	cases := []struct {
		name, method, path, token, body string
	}{
		{"provider cannot open wallets", http.MethodPost, "/wallets", provider, `{}`},
		{"provider cannot read wallets", http.MethodGet, "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37", provider, ""},
		{"provider cannot read ledgers", http.MethodGet, "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37/ledger", provider, ""},
		{"provider cannot reconcile wallets", http.MethodPost, "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37/reconciliation", provider, ""},
		{"internal service cannot send operations", http.MethodPost, "/wagering/transactions", internal, validOperation},
		{"internal service cannot read provider transactions", http.MethodGet, "/wagering/transactions/0192f298-345e-7e38-af88-e43f851a819d", internal, ""},
		{"provider role without provider identity", http.MethodPost, "/wagering/transactions", roleWithoutProvider, validOperation},
		{"provider identity without the role", http.MethodPost, "/wagering/transactions", providerWithoutRole, validOperation},
		{"another provider's namespace", http.MethodGet, "/providers/provider-b/wagering/transactions/tx-1", provider, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireError(t, do(t, app, tc.method, tc.path, tc.token, tc.body, "Idempotency-Key", "k-1"), http.StatusForbidden, "FORBIDDEN")
		})
	}

	t.Run("body cannot choose another provider", func(t *testing.T) {
		body := strings.Replace(validOperation, `"providerId":"provider-a"`, `"providerId":"provider-b"`, 1)
		requireError(t, do(t, app, http.MethodPost, "/wagering/transactions", provider, body, "Idempotency-Key", "k-1"), http.StatusForbidden, "PROVIDER_MISMATCH")
	})
}

func TestOperationValidation(t *testing.T) {
	p := newTestIDP(t)
	app := newTestApp(t, p)
	provider := p.token(t, "provider-a", auth.RoleWageringProvider)
	replace := func(old, new string) string { return strings.Replace(validOperation, old, new, 1) }

	cases := []struct {
		name, body, key string
		status          int
		code            string
	}{
		{"missing Idempotency-Key", validOperation, "", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"blank Idempotency-Key", validOperation, " ", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"amount as a JSON number", replace(`"amount":"25.00"`, `"amount":25.00`), "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"amount with three decimals", replace(`"amount":"25.00"`, `"amount":"25.000"`), "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"negative amount", replace(`"amount":"25.00"`, `"amount":"-25.00"`), "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"scientific notation", replace(`"amount":"25.00"`, `"amount":"2.5e1"`), "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"unknown currency", replace(`"currency":"BRL"`, `"currency":"ZZZ"`), "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"missing money", replace(`,"money":{"amount":"25.00","currency":"BRL"}`, ``), "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"OPENING is internal", replace(`"kind":"BET"`, `"kind":"OPENING"`), "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"REFUND without reference", replace(`"kind":"BET"`, `"kind":"REFUND"`), "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"missing providerId", replace(`"providerId":"provider-a",`, ``), "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"unknown field", replace(`"kind":"BET"`, `"kind":"BET","extra":1`), "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"trailing data", validOperation + `{}`, "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
		{"not JSON", `kind=BET`, "k-1", http.StatusBadRequest, "VALIDATION_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var headers []string
			if tc.key != "" {
				headers = []string{"Idempotency-Key", tc.key}
			}
			requireError(t, do(t, app, http.MethodPost, "/wagering/transactions", provider, tc.body, headers...), tc.status, tc.code)
		})
	}

	t.Run("content type must be JSON", func(t *testing.T) {
		r := do(t, app, http.MethodPost, "/wagering/transactions", provider, validOperation, "Idempotency-Key", "k-1", "Content-Type", "text/plain")
		requireError(t, r, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
	})
	t.Run("body size is limited", func(t *testing.T) {
		// O limite é aplicado pelo servidor fasthttp ao ler a requisição; app.Test não o reproduz,
		// então a verificação usa um listener real.
		big := replace(`"gameId":"g-1"`, `"gameId":"`+strings.Repeat("x", maxBodyBytes)+`"`)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
		t.Cleanup(func() { _ = app.Shutdown() })
		// Só os headers são enviados: o servidor recusa pelo Content-Length, antes de ler o corpo.
		// Enviar o corpo inteiro tornaria o teste instável (o servidor fecha a conexão no meio da escrita).
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "POST /wagering/transactions HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\n"+
			"Authorization: Bearer %s\r\nIdempotency-Key: k-1\r\nContent-Length: %d\r\n\r\n", provider, len(big))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		requireError(t, response{status: resp.StatusCode, body: body}, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE")
	})
	t.Run("transaction id must be a UUID", func(t *testing.T) {
		requireError(t, do(t, app, http.MethodGet, "/wagering/transactions/not-a-uuid", provider, ""), http.StatusBadRequest, "VALIDATION_ERROR")
	})
}

func TestWalletValidation(t *testing.T) {
	p := newTestIDP(t)
	app := newTestApp(t, p)
	internal := p.token(t, "", auth.RoleWalletOperator)
	wallet := "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37"

	cases := []struct {
		name, method, path, body string
	}{
		{"invalid player", http.MethodPost, "/wallets", `{"playerId":"p-1","initialBalance":{"amount":"1.00","currency":"BRL"}}`},
		{"negative initial balance", http.MethodPost, "/wallets", `{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"-1.00","currency":"BRL"}}`},
		{"initial balance as number", http.MethodPost, "/wallets", `{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":1,"currency":"BRL"}}`},
		{"missing initial balance", http.MethodPost, "/wallets", `{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"}`},
		{"wallet id not a UUID", http.MethodGet, "/wallets/abc", ""},
		{"reconciliation of a non-UUID wallet", http.MethodPost, "/wallets/abc/reconciliation", ""},
		{"limit too large", http.MethodGet, wallet + "/ledger?limit=101", ""},
		{"limit zero", http.MethodGet, wallet + "/ledger?limit=0", ""},
		{"limit not a number", http.MethodGet, wallet + "/ledger?limit=ten", ""},
		{"cursor not decodable", http.MethodGet, wallet + "/ledger?cursor=%%%", ""},
		{"cursor of another wallet", http.MethodGet, wallet + "/ledger?cursor=" + encodeCursor(ledgerCursor{WalletID: "0192f291-27dd-7d3f-8071-000000000000", Version: 3}), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireError(t, do(t, app, tc.method, tc.path, internal, tc.body), http.StatusBadRequest, "VALIDATION_ERROR")
		})
	}
}

func TestUnknownRouteIsJSON404(t *testing.T) {
	requireError(t, do(t, newTestApp(t, nil), http.MethodGet, "/nope", "", ""), http.StatusNotFound, "NOT_FOUND")
}

func TestMetricsEndpointIsExposed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	resp, err := newTestApp(t, nil).Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "wallet_reconciliations_total") {
		t.Fatalf("GET /metrics = %d %s", resp.StatusCode, raw)
	}
}
