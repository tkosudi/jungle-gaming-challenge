package app

import (
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/tkosudi/jungle-gaming-challenge/internal/config"
)

// O prazo de parada do Fx precisa cobrir a soma dos piores casos dos hooks, que param em
// sequência (ver shutdownTimeout); o padrão do Fx (15s) não cobre.
func TestNew_StopTimeoutCoversTheWorstCaseShutdown(t *testing.T) {
	a := New(config.Config{
		HTTPAddr: "127.0.0.1:0", DatabaseURL: "postgres://user:pass@127.0.0.1:1/db",
		SQS: config.SQS{Region: "us-east-1"}, Workers: config.Workers{ReferenceMaxAttempts: 1},
	}, fx.NopLogger)
	if err := a.Err(); err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := a.StopTimeout(); got != 120*time.Second {
		t.Fatalf("StopTimeout = %v, want 120s", got)
	}
	if worstCaseShutdown >= shutdownTimeout {
		t.Fatalf("worst case %v does not fit in the stop timeout %v", worstCaseShutdown, shutdownTimeout)
	}
}
