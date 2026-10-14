package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
)

// Os testes assinam tokens com chaves RSA reais e servem o JWKS por HTTP: a verificação
// criptográfica é a de produção. O Keycloak real é exercitado nos testes de integração.

const (
	testIssuer   = "http://localhost:8180/realms/jungle"
	testAudience = "wagering-api"
)

type idp struct {
	t      *testing.T
	key    *rsa.PrivateKey
	kid    string
	server *httptest.Server
	status int // resposta forçada do JWKS; 0 = 200 com as chaves
	hits   atomic.Int64
	mu     sync.Mutex // protege key e kid, lidos pelo servidor JWKS em outra goroutine
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{t: t, key: key, kid: "test-key"}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		if p.status != 0 {
			w.WriteHeader(p.status)
			return
		}
		p.mu.Lock()
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: p.kid, Algorithm: "RS256", Use: "sig"}}}
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *idp) verifier() *Verifier {
	return NewVerifier(config.OIDC{IssuerURL: testIssuer, JWKSURL: p.server.URL, Audience: testAudience})
}

// providerClaims imita um access token de client_credentials do Keycloak para o provider-a.
func providerClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": testIssuer, "aud": []string{testAudience, "account"}, "sub": "service-account-provider-a",
		"azp": "provider-a", "typ": "Bearer", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"provider_id":  "provider-a",
		"realm_access": map[string]any{"roles": []string{"default-roles-jungle", RoleWageringProvider}},
	}
}

func (p *idp) sign(claims map[string]any) string {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	return signWith(p.t, jose.RS256, p.key, p.kid, claims)
}

func signWith(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func with(claims map[string]any, key string, value any) map[string]any {
	claims[key] = value
	return claims
}

func TestVerify_AcceptsAProviderToken(t *testing.T) {
	p := newIDP(t)
	got, err := p.verifier().Verify(context.Background(), p.sign(providerClaims()))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.ProviderID != "provider-a" || got.ClientID != "provider-a" || got.Subject != "service-account-provider-a" {
		t.Errorf("principal = %+v", got)
	}
	if !got.HasRole(RoleWageringProvider) || got.HasRole(RoleWalletOperator) {
		t.Errorf("roles = %v", got.Roles)
	}
}

func TestVerify_AcceptsAudienceAsAString(t *testing.T) {
	p := newIDP(t)
	if _, err := p.verifier().Verify(context.Background(), p.sign(with(providerClaims(), "aud", testAudience))); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
}

func TestVerify_RejectsInvalidTokens(t *testing.T) {
	p := newIDP(t)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	valid := p.sign(providerClaims())
	parts := strings.Split(valid, ".")
	tamperedClaims, _ := json.Marshal(with(providerClaims(), "provider_id", "provider-b"))
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(tamperedClaims) + "." + parts[2]
	unsignedHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))

	tests := map[string]string{
		"expired":                    p.sign(with(providerClaims(), "exp", now.Add(-time.Minute).Unix())),
		"not yet valid":              p.sign(with(providerClaims(), "nbf", now.Add(10*time.Minute).Unix())),
		"issuer is the internal URL": p.sign(with(providerClaims(), "iss", "http://keycloak:8080/realms/jungle")),
		"another issuer":             p.sign(with(providerClaims(), "iss", "http://evil.example/realms/jungle")),
		"another audience":           p.sign(with(providerClaims(), "aud", []string{"account"})),
		"no audience":                p.sign(func() map[string]any { c := providerClaims(); delete(c, "aud"); return c }()),
		"ID token type":              p.sign(with(providerClaims(), "typ", "ID")),
		"signed by another key":      signWith(t, jose.RS256, otherKey, p.kid, providerClaims()),
		"HS256 with the public key as secret": signWith(t, jose.HS256,
			[]byte(base64.StdEncoding.EncodeToString(p.key.PublicKey.N.Bytes())), p.kid, providerClaims()),
		"alg none":  unsignedHeader + "." + parts[1] + ".",
		"tampered":  tampered,
		"malformed": "not-a-jwt",
		"empty":     "",
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := p.verifier().Verify(context.Background(), token)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("Verify() error = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestVerify_UnavailableKeysAreNotAnAuthenticationFailure(t *testing.T) {
	t.Run("IdP answers 503", func(t *testing.T) {
		p := newIDP(t)
		p.status = http.StatusServiceUnavailable
		if _, err := p.verifier().Verify(context.Background(), p.sign(providerClaims())); !errors.Is(err, ErrKeysUnavailable) {
			t.Fatalf("Verify() error = %v, want ErrKeysUnavailable", err)
		}
	})
	t.Run("IdP unreachable", func(t *testing.T) {
		p := newIDP(t)
		token := p.sign(providerClaims())
		v := p.verifier()
		p.server.Close()
		if _, err := v.Verify(context.Background(), token); !errors.Is(err, ErrKeysUnavailable) {
			t.Fatalf("Verify() error = %v, want ErrKeysUnavailable", err)
		}
	})
}

func TestPrincipal_InternalServiceHasNoProvider(t *testing.T) {
	p := newIDP(t)
	claims := providerClaims()
	delete(claims, "provider_id")
	claims["azp"] = "internal-service"
	claims["realm_access"] = map[string]any{"roles": []string{RoleWalletOperator}}
	got, err := p.verifier().Verify(context.Background(), p.sign(claims))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.ProviderID != "" || !got.HasRole(RoleWalletOperator) || got.HasRole(RoleWageringProvider) {
		t.Errorf("principal = %+v", got)
	}
}

// Um kid desconhecido faria o go-oidc buscar o JWKS a cada token forjado. As buscas ficam
// limitadas a uma por intervalo; chaves já em cache continuam verificando sem rede.
func TestVerify_UnknownKeyIDsDoNotHammerTheIdP(t *testing.T) {
	p := newIDP(t)
	v := newVerifier(config.OIDC{IssuerURL: testIssuer, JWKSURL: p.server.URL, Audience: testAudience}, time.Hour)
	if _, err := v.Verify(context.Background(), p.sign(providerClaims())); err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		forged := signWith(t, jose.RS256, other, fmt.Sprintf("unknown-%d", i), providerClaims())
		if _, err := v.Verify(context.Background(), forged); err == nil {
			t.Fatal("forged token accepted")
		}
	}
	if _, err := v.Verify(context.Background(), p.sign(providerClaims())); err != nil {
		t.Fatalf("known key after the forged burst: %v", err)
	}
	if hits := p.hits.Load(); hits != 1 {
		t.Fatalf("JWKS requests = %d, want 1", hits)
	}
}

func TestVerify_RotatedKeyIsFetchedAfterTheInterval(t *testing.T) {
	p := newIDP(t)
	v := newVerifier(config.OIDC{IssuerURL: testIssuer, JWKSURL: p.server.URL, Audience: testAudience}, 50*time.Millisecond)
	if _, err := v.Verify(context.Background(), p.sign(providerClaims())); err != nil {
		t.Fatal(err)
	}
	rotated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.key, p.kid = rotated, "rotated-key"
	p.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	if _, err := v.Verify(context.Background(), p.sign(providerClaims())); err != nil {
		t.Fatalf("token signed by the rotated key: %v", err)
	}
	if hits := p.hits.Load(); hits != 2 {
		t.Fatalf("JWKS requests = %d, want 2", hits)
	}
}

// Na partida, requisições simultâneas com o mesmo kid compartilham a primeira busca.
func TestVerify_ConcurrentFirstRequestsShareOneFetch(t *testing.T) {
	p := newIDP(t)
	v := p.verifier()
	token := p.sign(providerClaims())
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = v.Verify(context.Background(), token)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if hits := p.hits.Load(); hits != 1 {
		t.Fatalf("JWKS requests = %d, want 1", hits)
	}
}

// Com o kid já verificado, a chave está em cache: assinatura que não confere é token inválido
// (401), mesmo que a nova busca ao JWKS esteja limitada.
func TestVerify_BadSignatureOnAKnownKeyIsUnauthenticatedWhileRefreshIsLimited(t *testing.T) {
	p := newIDP(t)
	v := newVerifier(config.OIDC{IssuerURL: testIssuer, JWKSURL: p.server.URL, Audience: testAudience}, time.Hour)
	if _, err := v.Verify(context.Background(), p.sign(providerClaims())); err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	forged := signWith(t, jose.RS256, other, p.kid, providerClaims())
	if _, err := v.Verify(context.Background(), forged); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Verify() error = %v, want ErrUnauthenticated", err)
	}
}
