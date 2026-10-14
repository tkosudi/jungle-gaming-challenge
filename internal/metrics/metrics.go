// Package metrics define as métricas Prometheus da aplicação, num registro próprio exposto
// em GET /metrics (somente as métricas daqui, sem coletores do runtime).
package metrics

import "github.com/prometheus/client_golang/prometheus"

var Registry = prometheus.NewRegistry()

var (
	// Reconciliations conta as reconciliações executadas por resultado ("consistent" ou
	// "divergent"). Uma divergência indica saldo armazenado diferente do reconstruído pelo
	// ledger, o que as proteções do banco deveriam impedir: alertar em qualquer incremento.
	Reconciliations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wallet_reconciliations_total",
		Help: "Wallet reconciliations by result (consistent or divergent).",
	}, []string{"result"})

	// Operations conta operações financeiras novas por origem (http, sqs) e estado final.
	Operations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wagering_operations_total",
		Help: "Wagering operations applied, by source and resulting status.",
	}, []string{"source", "status"})

	// Duplicates conta recebimentos repetidos reconhecidos: replay por idempotência (http, sqs)
	// e mensagem já concluída na inbox (sqs_inbox).
	Duplicates = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wagering_duplicates_total",
		Help: "Repeated deliveries recognized without new effects, by kind.",
	}, []string{"kind"})

	// SQSMessages conta o destino de cada mensagem recebida: completed, retried (falha
	// transitória, volta à fila com atraso), dead_lettered (recusa permanente, enviada à DLQ)
	// e released (recebida durante o shutdown e devolvida sem tratar).
	SQSMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sqs_messages_total",
		Help: "Received SQS messages by outcome.",
	}, []string{"outcome"})

	// DeadLettered conta as mensagens enviadas à DLQ pelo consumidor, por motivo. As que
	// chegam lá pelo redrive (tentativas esgotadas) aparecem em SQSMessages{outcome="retried"}
	// e na própria DLQ.
	DeadLettered = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sqs_dead_lettered_total",
		Help: "Messages moved to the dead-letter queue by the consumer, by reason.",
	}, []string{"reason"})

	OutboxPublished = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "outbox_events_published_total",
		Help: "Outbox events confirmed by the broker and marked as published.",
	})
	OutboxPublishFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "outbox_publish_failures_total",
		Help: "Outbox sends rejected by the broker or failed batches, retried later.",
	})
	// OutboxOldestPendingAge é o atraso da outbox: idade do evento pendente mais antigo.
	OutboxOldestPendingAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_oldest_pending_age_seconds",
		Help: "Age of the oldest unpublished outbox event.",
	})
	OutboxPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_pending_events",
		Help: "Unpublished outbox events.",
	})

	// ReferenceSteps conta os passos do worker de referências por estado resultante.
	ReferenceSteps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pending_reference_steps_total",
		Help: "Pending reference worker steps by resulting status.",
	}, []string{"status"})

	// WorkerErrors conta falhas inesperadas dos laços de fundo (consumer, publisher, references).
	WorkerErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "worker_errors_total",
		Help: "Unexpected errors in background workers, by worker.",
	}, []string{"worker"})
)

func init() {
	Registry.MustRegister(Reconciliations, Operations, Duplicates, SQSMessages, DeadLettered,
		OutboxPublished, OutboxPublishFailures, OutboxOldestPendingAge, OutboxPending, ReferenceSteps, WorkerErrors)
	// Séries que existem desde a partida (valor 0), para alertas antes do primeiro incremento.
	Reconciliations.WithLabelValues("consistent")
	Reconciliations.WithLabelValues("divergent")
	for _, outcome := range []string{"completed", "retried", "dead_lettered", "released"} {
		SQSMessages.WithLabelValues(outcome)
	}
}
