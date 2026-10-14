// Package auth valida os access tokens OAuth 2.0/OIDC emitidos pelo Keycloak e expõe a
// identidade autenticada. A autorização por rota fica no transporte (httpapi).
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
)

var (
	// ErrUnauthenticated: token ausente, malformado, com assinatura, algoritmo, issuer,
	// audience, tipo ou validade inválidos. Resposta 401.
	ErrUnauthenticated = errors.New("auth: invalid credentials")
	// ErrKeysUnavailable: não foi possível obter as chaves públicas do IdP. É falha
	// transitória da infraestrutura (503), não do cliente: o token pode ser válido.
	ErrKeysUnavailable = errors.New("auth: signing keys unavailable")
)

// Papéis de realm atribuídos às contas de serviço no Keycloak.
const (
	// RoleWageringProvider permite enviar e consultar operações do próprio provedor.
	RoleWageringProvider = "wagering-provider"
	// RoleWalletOperator permite as operações internas de carteira.
	RoleWalletOperator = "wallet-operator"
)

const (
	// Keycloak marca access tokens com typ "Bearer"; ID tokens usam "ID" e não são aceitos.
	accessTokenType = "Bearer"
	jwksTimeout     = 5 * time.Second
	// jwksMinRefreshInterval limita as buscas ao JWKS. O go-oidc busca de novo a cada kid
	// desconhecido; sem o limite, tokens forjados com kids aleatórios virariam uma requisição
	// ao IdP cada. Kids já em cache não dependem do limite.
	jwksMinRefreshInterval = 5 * time.Second
)

// Principal é a identidade extraída de um token válido.
type Principal struct {
	Subject  string
	ClientID string // azp: o cliente OAuth que obteve o token
	// ProviderID vem do claim provider_id, configurado no IdP por cliente. Vazio para
	// identidades que não são provedores; nunca é lido do payload ou de headers.
	ProviderID string
	Roles      []string
}

func (p Principal) HasRole(role string) bool { return slices.Contains(p.Roles, role) }

// Verifier valida assinatura (somente RS256, chaves do JWKS), issuer exato, audience,
// expiração e nbf. As chaves ficam em cache e são buscadas de novo ao surgir um kid novo.
type Verifier struct {
	verifier *oidc.IDTokenVerifier
}

func NewVerifier(cfg config.OIDC) *Verifier {
	return newVerifier(cfg, jwksMinRefreshInterval)
}

func newVerifier(cfg config.OIDC, minRefreshInterval time.Duration) *Verifier {
	transport := &keysTransport{base: http.DefaultTransport, minInterval: minRefreshInterval}
	client := &http.Client{Timeout: jwksTimeout, Transport: transport}
	remote := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), client), cfg.JWKSURL)
	return &Verifier{verifier: oidc.NewVerifier(cfg.IssuerURL, &keySet{remote: remote, known: map[string]bool{}}, &oidc.Config{
		ClientID:             cfg.Audience,
		SupportedSigningAlgs: []string{oidc.RS256},
	})}
}

type claims struct {
	Type        string `json:"typ"`
	AuthParty   string `json:"azp"`
	ProviderID  string `json:"provider_id"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func (v *Verifier) Verify(ctx context.Context, rawToken string) (Principal, error) {
	var keysUnavailable bool
	token, err := v.verifier.Verify(context.WithValue(ctx, keysUnavailableKey{}, &keysUnavailable), rawToken)
	switch {
	case err != nil && keysUnavailable:
		return Principal{}, fmt.Errorf("%w: %v", ErrKeysUnavailable, err)
	case err != nil:
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	var c claims
	if err := token.Claims(&c); err != nil {
		return Principal{}, fmt.Errorf("%w: claims: %v", ErrUnauthenticated, err)
	}
	if c.Type != accessTokenType {
		return Principal{}, fmt.Errorf("%w: token type %q is not an access token", ErrUnauthenticated, c.Type)
	}
	return Principal{Subject: token.Subject, ClientID: c.AuthParty, ProviderID: c.ProviderID, Roles: c.RealmAccess.Roles}, nil
}

// keySet sinaliza ao Verify que a assinatura não pôde ser verificada por falta das chaves.
// O go-oidc formata o erro do KeySet com %v, perdendo a cadeia de errors.Is; por isso o
// sinal volta por um ponteiro no contexto da própria chamada (mesma goroutine, sem disputa).
//
// O go-oidc também busca o JWKS de novo quando a assinatura não confere com a chave em
// cache. Para um kid já verificado com sucesso a chave é conhecida: a falha é do token (401),
// ainda que essa nova busca tenha sido recusada ou falhado.
type keySet struct {
	remote *oidc.RemoteKeySet

	mu    sync.Mutex
	known map[string]bool // kids que já verificaram uma assinatura; limitado às chaves reais
}

type keysUnavailableKey struct{}

func (k *keySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	kid := keyID(jwt)
	payload, err := k.remote.VerifySignature(ctx, jwt)

	k.mu.Lock()
	defer k.mu.Unlock()
	if err == nil {
		k.known[kid] = true
		return payload, nil
	}
	unavailable := errors.Is(err, ErrKeysUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	if unavailable && !k.known[kid] {
		if flag, ok := ctx.Value(keysUnavailableKey{}).(*bool); ok {
			*flag = true
		}
	}
	return nil, err
}

// keyID lê o kid do cabeçalho sem validar nada; vazio se o token for malformado (o go-oidc o
// recusa em seguida).
func keyID(jwt string) string {
	sig, err := jose.ParseSigned(jwt, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(sig.Signatures) != 1 {
		return ""
	}
	return sig.Signatures[0].Header.KeyID
}

// keysTransport faz as buscas ao JWKS. Marca falhas de rede e respostas 5xx como
// ErrKeysUnavailable, para que uma indisponibilidade do IdP não seja confundida com token
// inválido, e recusa buscar de novo antes de minInterval: um kid desconhecido nesse intervalo
// (token forjado ou, raramente, chave recém-rotacionada) responde 503 sem tocar o IdP. Buscas
// simultâneas já são agrupadas pelo go-oidc numa só.
type keysTransport struct {
	base        http.RoundTripper
	minInterval time.Duration

	mu   sync.Mutex
	last time.Time // início da última busca, com ou sem sucesso
}

func (t *keysTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if !t.last.IsZero() && time.Since(t.last) < t.minInterval {
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: JWKS was fetched less than %s ago", ErrKeysUnavailable, t.minInterval)
	}
	t.last = time.Now()
	t.mu.Unlock()

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeysUnavailable, err)
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: JWKS answered %d", ErrKeysUnavailable, resp.StatusCode)
	}
	return resp, nil
}
