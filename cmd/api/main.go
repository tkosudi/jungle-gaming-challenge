// Command api inicia o serviço HTTP de processamento de apostas.
package main

import (
	"fmt"
	"os"

	"github.com/tkosudi/jungle-gaming-challenge/internal/app"
	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
)

func main() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Run bloqueia até SIGINT/SIGTERM, executa o shutdown ordenado e
	// termina com código 1 se o startup falhar.
	app.New(cfg).Run()
}
