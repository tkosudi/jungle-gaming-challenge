package config

import (
	"errors"
	"testing"
)

func getenvFrom(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoad_Valid(t *testing.T) {
	cfg, err := Load(getenvFrom(map[string]string{
		"DATABASE_URL": "postgres://u:p@localhost:5432/db?sslmode=disable",
		"HTTP_ADDR":    "127.0.0.1:9090",
	}))
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
	cfg, err := Load(getenvFrom(map[string]string{
		"DATABASE_URL": "postgres://localhost/db",
	}))
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
		{"absent", map[string]string{"HTTP_ADDR": ":8080"}},
		{"empty", map[string]string{"DATABASE_URL": "", "HTTP_ADDR": ":8080"}},
		{"blank", map[string]string{"DATABASE_URL": "   ", "HTTP_ADDR": ":8080"}},
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
			_, err := Load(getenvFrom(map[string]string{
				"DATABASE_URL": "postgres://localhost/db",
				"HTTP_ADDR":    tt.addr,
			}))
			if !errors.Is(err, ErrInvalidHTTPAddr) {
				t.Fatalf("Load() error = %v, want ErrInvalidHTTPAddr", err)
			}
		})
	}
}
