// Package testenv fornece a configuração dos testes de integração sem segredos no código:
// cada valor vem da variável de ambiente ou, na ausência dela, do .env local da raiz do
// repositório (ignorado pelo Git). O go test não carrega .env sozinho; este pacote o lê.
// Usado apenas por testes.
package testenv

import (
	"bufio"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	loadOnce sync.Once
	dotenv   map[string]string
)

// Lookup devolve a variável de ambiente key ou, se vazia, o valor do .env da raiz do módulo.
func Lookup(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	loadOnce.Do(func() { dotenv = readDotenv() })
	return dotenv[key]
}

// DatabaseURL é TEST_DATABASE_URL ou o PostgreSQL local do Compose montado a partir de
// POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DB e POSTGRES_PORT.
func DatabaseURL() string {
	if v := Lookup("TEST_DATABASE_URL"); v != "" {
		return v
	}
	port := Lookup("POSTGRES_PORT")
	if port == "" {
		port = "5432"
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(Lookup("POSTGRES_USER"), Lookup("POSTGRES_PASSWORD")),
		Host:     "127.0.0.1:" + port,
		Path:     "/" + Lookup("POSTGRES_DB"),
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// readDotenv lê linhas KEY=VALUE do .env ao lado do go.mod; comentários e linhas vazias são
// ignorados e aspas externas são removidas. Sem .env, devolve um mapa vazio.
func readDotenv() map[string]string {
	values := map[string]string{}
	dir, err := os.Getwd()
	if err != nil {
		return values
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return values
		}
		dir = parent
	}
	f, err := os.Open(filepath.Join(dir, ".env"))
	if err != nil {
		return values
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, ok := strings.Cut(line, "=")
		if line == "" || strings.HasPrefix(line, "#") || !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values
}
