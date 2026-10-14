// Package config carrega e valida a configuração da aplicação a partir do ambiente.
package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const defaultHTTPAddr = ":8080"

var (
	ErrMissingDatabaseURL = errors.New("config: DATABASE_URL is required")
	ErrInvalidHTTPAddr    = errors.New("config: HTTP_ADDR is invalid")
)

// Config reúne os parâmetros de execução validados.
type Config struct {
	HTTPAddr    string
	DatabaseURL string
}

// Load constrói a Config lendo variáveis com getenv (os.Getenv em produção).
// Valor vazio equivale a variável ausente. HTTP_ADDR tem default ":8080".
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

	return Config{HTTPAddr: httpAddr, DatabaseURL: databaseURL}, nil
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
