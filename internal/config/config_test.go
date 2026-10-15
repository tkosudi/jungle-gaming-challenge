package config

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// baseEnv é o mínimo válido; cada teste sobrescreve o que examina.
func baseEnv(overrides map[string]string) map[string]string {
	env := map[string]string{
		"DATABASE_URL":          "postgres://localhost/db",
		"OIDC_ISSUER_URL":       "http://localhost:8180/realms/jungle",
		"OIDC_AUDIENCE":         "wagering-api",
		"SQS_TRUSTED_PROVIDERS": "provider-a,provider-b",
	}
	for k, v := range overrides {
		env[k] = v
	}
	return env
}

func getenvFrom(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoad_Valid(t *testing.T) {
	cfg, err := Load(getenvFrom(baseEnv(map[string]string{
		"DATABASE_URL": "postgres://u:p@localhost:5432/db?sslmode=disable",
		"HTTP_ADDR":    "127.0.0.1:9090",
	})))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.DatabaseURL != "postgres://u:p@localhost:5432/db?sslmode=disable" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
}

func TestLoad_HTTPAddrDefault(t *testing.T) {
	cfg, err := Load(getenvFrom(baseEnv(map[string]string{
		"DATABASE_URL": "postgres://localhost/db",
	})))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
}

func TestLoad_MissingDatabaseURL(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"absent", map[string]string{"HTTP_ADDR": ":8080", "OIDC_ISSUER_URL": "http://localhost:8180/realms/jungle", "OIDC_AUDIENCE": "wagering-api"}},
		{"empty", baseEnv(map[string]string{"DATABASE_URL": ""})},
		{"blank", baseEnv(map[string]string{"DATABASE_URL": "   "})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(getenvFrom(tt.env))
			if !errors.Is(err, ErrMissingDatabaseURL) {
				t.Fatalf("Load() error = %v, want ErrMissingDatabaseURL", err)
			}
		})
	}
}

func TestLoad_InvalidHTTPAddr(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{"no port", "localhost"},
		{"non numeric port", ":http-alt"},
		{"port out of range", ":70000"},
		{"negative port", ":-1"},
		{"garbage", "not an address"},
		{"empty port", "localhost:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(getenvFrom(baseEnv(map[string]string{
				"DATABASE_URL": "postgres://localhost/db",
				"HTTP_ADDR":    tt.addr,
			})))
			if !errors.Is(err, ErrInvalidHTTPAddr) {
				t.Fatalf("Load() error = %v, want ErrInvalidHTTPAddr", err)
			}
		})
	}
}

func TestLoad_OIDC(t *testing.T) {
	cfg, err := Load(getenvFrom(baseEnv(nil)))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.OIDC.IssuerURL != "http://localhost:8180/realms/jungle" || cfg.OIDC.Audience != "wagering-api" {
		t.Errorf("OIDC = %+v", cfg.OIDC)
	}
	// Sem OIDC_JWKS_URL, o caminho de certificados do Keycloak a partir do issuer.
	if cfg.OIDC.JWKSURL != "http://localhost:8180/realms/jungle/protocol/openid-connect/certs" {
		t.Errorf("JWKSURL default = %q", cfg.OIDC.JWKSURL)
	}

	// Dentro do Docker as chaves vêm do nome interno, mas o issuer esperado continua o público.
	cfg, err = Load(getenvFrom(baseEnv(map[string]string{
		"OIDC_JWKS_URL": "http://keycloak:8080/realms/jungle/protocol/openid-connect/certs",
	})))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.OIDC.JWKSURL != "http://keycloak:8080/realms/jungle/protocol/openid-connect/certs" ||
		cfg.OIDC.IssuerURL != "http://localhost:8180/realms/jungle" {
		t.Errorf("OIDC = %+v", cfg.OIDC)
	}
}

func TestLoad_InvalidOIDC(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want error
	}{
		{"missing issuer", map[string]string{"OIDC_ISSUER_URL": ""}, ErrMissingOIDCIssuer},
		{"missing audience", map[string]string{"OIDC_AUDIENCE": " "}, ErrMissingOIDCAudience},
		{"relative issuer", map[string]string{"OIDC_ISSUER_URL": "/realms/jungle"}, ErrInvalidOIDCURL},
		{"issuer with another scheme", map[string]string{"OIDC_ISSUER_URL": "ftp://idp/realms/jungle"}, ErrInvalidOIDCURL},
		{"invalid jwks url", map[string]string{"OIDC_JWKS_URL": "keycloak:8080/certs"}, ErrInvalidOIDCURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(getenvFrom(baseEnv(tt.env))); !errors.Is(err, tt.want) {
				t.Fatalf("Load() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestLoad_SQSAndWorkerDefaults(t *testing.T) {
	cfg, err := Load(getenvFrom(baseEnv(nil)))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	s := cfg.SQS
	if s.Region != "us-east-1" || s.EndpointURL != "" || s.RequestsQueue != "wager-transactions.fifo" ||
		s.DeadLetterQueue != "wager-transactions-dlq.fifo" || s.EventsQueue != "wager-events.fifo" ||
		s.ConsumerName != "wager-transactions-consumer" || s.RetryBaseDelay != time.Second || s.RetryMaxDelay != time.Minute {
		t.Errorf("SQS = %+v", s)
	}
	if fmt.Sprint(s.TrustedProviders) != "[provider-a provider-b]" {
		t.Errorf("TrustedProviders = %v", s.TrustedProviders)
	}
	w := cfg.Workers
	if w.ReferenceInitialBackoff != time.Second || w.ReferenceMaxBackoff != time.Minute || w.ReferenceMaxAttempts != 10 ||
		w.ReferencePollInterval != time.Second || w.OutboxPollInterval != 500*time.Millisecond {
		t.Errorf("Workers = %+v", w)
	}

	cfg, err = Load(getenvFrom(baseEnv(map[string]string{
		"AWS_ENDPOINT_URL": "http://localstack:4566", "SQS_TRUSTED_PROVIDERS": " provider-a , provider-c ",
		"SQS_RETRY_BASE_DELAY": "200ms", "REFERENCE_MAX_ATTEMPTS": "3", "OUTBOX_POLL_INTERVAL": "50ms",
	})))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SQS.EndpointURL != "http://localstack:4566" || fmt.Sprint(cfg.SQS.TrustedProviders) != "[provider-a provider-c]" ||
		cfg.SQS.RetryBaseDelay != 200*time.Millisecond || cfg.Workers.ReferenceMaxAttempts != 3 || cfg.Workers.OutboxPollInterval != 50*time.Millisecond {
		t.Errorf("config = %+v", cfg)
	}
}

func TestLoad_InvalidSQSAndWorkers(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no trusted providers":       {"SQS_TRUSTED_PROVIDERS": " , "},
		"requests queue is not FIFO": {"SQS_REQUESTS_QUEUE": "wager-transactions"},
		"events queue is not FIFO":   {"SQS_EVENTS_QUEUE": "events"},
		"invalid duration":           {"SQS_RETRY_BASE_DELAY": "soon"},
		"negative duration":          {"OUTBOX_POLL_INTERVAL": "-1s"},
		"max below base":             {"SQS_RETRY_BASE_DELAY": "2m", "SQS_RETRY_MAX_DELAY": "1m"},
		"zero attempts":              {"REFERENCE_MAX_ATTEMPTS": "0"},
		"attempts not a number":      {"REFERENCE_MAX_ATTEMPTS": "many"},
		"invalid endpoint":           {"AWS_ENDPOINT_URL": "localstack:4566"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(getenvFrom(baseEnv(env))); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Load() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
