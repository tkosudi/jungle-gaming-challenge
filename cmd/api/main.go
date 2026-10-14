// Command api inicia o serviço HTTP de processamento de apostas.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/tkosudi/jungle-gaming-challenge/internal/app"
	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
)

func main() {
	// Logs estruturados em JSON (stdout) para todo slog da aplicação.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Run bloqueia até SIGINT/SIGTERM, executa o shutdown ordenado e
	// termina com código 1 se o startup falhar.
	app.New(cfg).Run()
}
