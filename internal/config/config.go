// Package config carrega e valida a configuração da aplicação a partir do ambiente.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const (
	defaultHTTPAddr = ":8080"
	// keycloakCertsPath é onde o Keycloak publica o JWKS de um realm.
	keycloakCertsPath = "/protocol/openid-connect/certs"
)

var (
	ErrMissingDatabaseURL  = errors.New("config: DATABASE_URL is required")
	ErrInvalidHTTPAddr     = errors.New("config: HTTP_ADDR is invalid")
	ErrMissingOIDCIssuer   = errors.New("config: OIDC_ISSUER_URL is required")
	ErrMissingOIDCAudience = errors.New("config: OIDC_AUDIENCE is required")
	ErrInvalidOIDCURL      = errors.New("config: OIDC URL is invalid")
)

// Config reúne os parâmetros de execução validados.
type Config struct {
	HTTPAddr    string
	DatabaseURL string
	OIDC        OIDC
}

// OIDC descreve como validar os access tokens do IdP.
type OIDC struct {
	// IssuerURL é comparado exatamente com o claim iss: é a URL pública do realm, a mesma
	// que o Keycloak grava nos tokens (KC_HOSTNAME), não importa de onde a API o acesse.
	IssuerURL string
	// JWKSURL é de onde as chaves públicas são buscadas; dentro do Docker aponta para o
	// nome interno do serviço. Separá-lo do issuer evita afrouxar a verificação do iss.
	JWKSURL  string
	Audience string
}

// Load constrói a Config lendo variáveis com getenv (os.Getenv em produção).
// Valor vazio equivale a variável ausente. Defaults: HTTP_ADDR ":8080"; OIDC_JWKS_URL é o
// caminho de certificados do Keycloak sob OIDC_ISSUER_URL.
func Load(getenv func(string) string) (Config, error) {
	databaseURL := strings.TrimSpace(getenv("DATABASE_URL"))
	if databaseURL == "" {
		return Config{}, ErrMissingDatabaseURL
	}

	httpAddr := strings.TrimSpace(getenv("HTTP_ADDR"))
	if httpAddr == "" {
		httpAddr = defaultHTTPAddr
	}
	if err := validateAddr(httpAddr); err != nil {
		return Config{}, err
	}

	oidc, err := loadOIDC(getenv)
	if err != nil {
		return Config{}, err
	}
	return Config{HTTPAddr: httpAddr, DatabaseURL: databaseURL, OIDC: oidc}, nil
}

func loadOIDC(getenv func(string) string) (OIDC, error) {
	issuer := strings.TrimSpace(getenv("OIDC_ISSUER_URL"))
	if issuer == "" {
		return OIDC{}, ErrMissingOIDCIssuer
	}
	if err := validateURL("OIDC_ISSUER_URL", issuer); err != nil {
		return OIDC{}, err
	}
	audience := strings.TrimSpace(getenv("OIDC_AUDIENCE"))
	if audience == "" {
		return OIDC{}, ErrMissingOIDCAudience
	}
	jwks := strings.TrimSpace(getenv("OIDC_JWKS_URL"))
	if jwks == "" {
		jwks = strings.TrimSuffix(issuer, "/") + keycloakCertsPath
	}
	if err := validateURL("OIDC_JWKS_URL", jwks); err != nil {
		return OIDC{}, err
	}
	return OIDC{IssuerURL: issuer, JWKSURL: jwks, Audience: audience}, nil
}

func validateURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: %s %q must be an absolute http(s) URL", ErrInvalidOIDCURL, name, raw)
	}
	return nil
}

func validateAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", ErrInvalidHTTPAddr, addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("%w: %q: port must be 0-65535", ErrInvalidHTTPAddr, addr)
	}
	return nil
}
