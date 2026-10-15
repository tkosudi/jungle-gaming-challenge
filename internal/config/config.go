// Package config carrega e valida a configuração da aplicação a partir do ambiente.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
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
	// ErrInvalidConfig cobre os parâmetros de SQS e dos workers.
	ErrInvalidConfig = errors.New("config: invalid configuration")
)

// Config reúne os parâmetros de execução validados.
type Config struct {
	HTTPAddr    string
	DatabaseURL string
	OIDC        OIDC
	SQS         SQS
	Workers     Workers
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
	sqs, err := loadSQS(getenv)
	if err != nil {
		return Config{}, err
	}
	workers, err := loadWorkers(getenv)
	if err != nil {
		return Config{}, err
	}
	return Config{HTTPAddr: httpAddr, DatabaseURL: databaseURL, OIDC: oidc, SQS: sqs, Workers: workers}, nil
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

// SQS configura as filas. Credenciais AWS não passam por aqui: o SDK as lê do ambiente
// (AWS_ACCESS_KEY_ID e AWS_SECRET_ACCESS_KEY, vindos do .env local), sem valores no código.
type SQS struct {
	Region      string // AWS_REGION, padrão us-east-1
	EndpointURL string // AWS_ENDPOINT_URL, opcional (LocalStack)
	// Filas FIFO de entrada, de mensagens mortas (destino do redrive e das recusas
	// permanentes) e de eventos publicados pela outbox.
	RequestsQueue   string // SQS_REQUESTS_QUEUE, padrão wager-transactions.fifo
	DeadLetterQueue string // SQS_DLQ_QUEUE, padrão wager-transactions-dlq.fifo
	EventsQueue     string // SQS_EVENTS_QUEUE, padrão wager-events.fifo
	ConsumerName    string // SQS_CONSUMER_NAME, padrão wager-transactions-consumer (chave da inbox)
	// TrustedProviders é a fronteira de confiança da fila: só mensagens destes provedores são
	// aceitas (SQS_TRUSTED_PROVIDERS, obrigatória, separada por vírgulas). Ver ARCHITECTURE.md.
	TrustedProviders []string
	// Espera antes de reentregar uma mensagem com falha transitória: base * 2^(recebimentos-1),
	// limitada a RetryMaxDelay.
	RetryBaseDelay time.Duration // SQS_RETRY_BASE_DELAY, padrão 1s
	RetryMaxDelay  time.Duration // SQS_RETRY_MAX_DELAY, padrão 60s
}

// Workers configura os laços de fundo.
type Workers struct {
	ReferenceInitialBackoff time.Duration // REFERENCE_INITIAL_BACKOFF, padrão 1s
	ReferenceMaxBackoff     time.Duration // REFERENCE_MAX_BACKOFF, padrão 60s
	ReferenceMaxAttempts    int           // REFERENCE_MAX_ATTEMPTS, padrão 10
	ReferencePollInterval   time.Duration // REFERENCE_POLL_INTERVAL, padrão 1s: espera quando não há pendência vencida
	OutboxPollInterval      time.Duration // OUTBOX_POLL_INTERVAL, padrão 500ms: espera quando a outbox está vazia
}

func loadSQS(getenv func(string) string) (SQS, error) {
	s := SQS{
		Region:          valueOr(getenv, "AWS_REGION", "us-east-1"),
		EndpointURL:     strings.TrimSpace(getenv("AWS_ENDPOINT_URL")),
		RequestsQueue:   valueOr(getenv, "SQS_REQUESTS_QUEUE", "wager-transactions.fifo"),
		DeadLetterQueue: valueOr(getenv, "SQS_DLQ_QUEUE", "wager-transactions-dlq.fifo"),
		EventsQueue:     valueOr(getenv, "SQS_EVENTS_QUEUE", "wager-events.fifo"),
		ConsumerName:    valueOr(getenv, "SQS_CONSUMER_NAME", "wager-transactions-consumer"),
	}
	for _, p := range strings.Split(getenv("SQS_TRUSTED_PROVIDERS"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			s.TrustedProviders = append(s.TrustedProviders, p)
		}
	}
	if len(s.TrustedProviders) == 0 {
		return SQS{}, fmt.Errorf("%w: SQS_TRUSTED_PROVIDERS is required", ErrInvalidConfig)
	}
	for name, q := range map[string]string{"SQS_REQUESTS_QUEUE": s.RequestsQueue, "SQS_DLQ_QUEUE": s.DeadLetterQueue, "SQS_EVENTS_QUEUE": s.EventsQueue} {
		if !strings.HasSuffix(q, ".fifo") {
			return SQS{}, fmt.Errorf("%w: %s %q must be a FIFO queue (.fifo)", ErrInvalidConfig, name, q)
		}
	}
	if s.EndpointURL != "" {
		if err := validateURL("AWS_ENDPOINT_URL", s.EndpointURL); err != nil {
			return SQS{}, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
		}
	}
	var err error
	if s.RetryBaseDelay, err = duration(getenv, "SQS_RETRY_BASE_DELAY", time.Second); err != nil {
		return SQS{}, err
	}
	if s.RetryMaxDelay, err = duration(getenv, "SQS_RETRY_MAX_DELAY", time.Minute); err != nil {
		return SQS{}, err
	}
	if s.RetryMaxDelay < s.RetryBaseDelay {
		return SQS{}, fmt.Errorf("%w: SQS_RETRY_MAX_DELAY is below SQS_RETRY_BASE_DELAY", ErrInvalidConfig)
	}
	return s, nil
}

func loadWorkers(getenv func(string) string) (Workers, error) {
	var w Workers
	var err error
	for _, d := range []struct {
		dst      *time.Duration
		name     string
		fallback time.Duration
	}{
		{&w.ReferenceInitialBackoff, "REFERENCE_INITIAL_BACKOFF", time.Second},
		{&w.ReferenceMaxBackoff, "REFERENCE_MAX_BACKOFF", time.Minute},
		{&w.ReferencePollInterval, "REFERENCE_POLL_INTERVAL", time.Second},
		{&w.OutboxPollInterval, "OUTBOX_POLL_INTERVAL", 500 * time.Millisecond},
	} {
		if *d.dst, err = duration(getenv, d.name, d.fallback); err != nil {
			return Workers{}, err
		}
	}
	w.ReferenceMaxAttempts = 10
	if raw := strings.TrimSpace(getenv("REFERENCE_MAX_ATTEMPTS")); raw != "" {
		if w.ReferenceMaxAttempts, err = strconv.Atoi(raw); err != nil || w.ReferenceMaxAttempts < 1 {
			return Workers{}, fmt.Errorf("%w: REFERENCE_MAX_ATTEMPTS must be a positive integer", ErrInvalidConfig)
		}
	}
	return w, nil
}

func valueOr(getenv func(string) string, key, fallback string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return fallback
}

func duration(getenv func(string) string, key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%w: %s %q must be a non-negative duration (e.g. 500ms, 2s)", ErrInvalidConfig, key, raw)
	}
	return d, nil
}
