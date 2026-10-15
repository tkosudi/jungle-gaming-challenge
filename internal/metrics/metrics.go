// Package metrics define as métricas Prometheus da aplicação, num registro próprio exposto
// em GET /metrics (somente as métricas daqui, sem coletores do runtime).
package metrics

import "github.com/prometheus/client_golang/prometheus"

var Registry = prometheus.NewRegistry()

// Reconciliations conta as reconciliações executadas por resultado ("consistent" ou
// "divergent"). Uma divergência indica saldo armazenado diferente do reconstruído pelo
// ledger, o que as proteções do banco deveriam impedir: alertar em qualquer incremento.
var Reconciliations = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "wallet_reconciliations_total",
	Help: "Wallet reconciliations by result (consistent or divergent).",
}, []string{"result"})

func init() {
	Registry.MustRegister(Reconciliations)
	// As duas séries existem desde o início (valor 0), para que um alerta sobre "divergent"
	// funcione antes da primeira divergência.
	Reconciliations.WithLabelValues("consistent")
	Reconciliations.WithLabelValues("divergent")
}
