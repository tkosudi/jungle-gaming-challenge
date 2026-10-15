//go:build integration

package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, r)
	}
	return out
}

func scrape(t *testing.T, base string) map[string]float64 {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]float64{}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if name, value, ok := strings.Cut(scanner.Text(), " "); ok && !strings.HasPrefix(name, "#") {
			out[name], _ = strconv.ParseFloat(value, 64)
		}
	}
	return out
}

func findLog(records []map[string]any, msg string, fields map[string]string) map[string]any {
	for _, r := range records {
		if r["msg"] != msg {
			continue
		}
		match := true
		for k, v := range fields {
			if r[k] != v {
				match = false
			}
		}
		if match {
			return r
		}
	}
	return nil
}

// Seção 12: logs JSON com os identificadores da operação (sem credenciais nem payload
// financeiro) e métricas de latência de processamento e de conflitos de concorrência.
func TestObservability_OperationLogsAndMetrics(t *testing.T) {
	var logs syncBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	env := newEnvironment(t)
	base := startInstance(t, env)
	db := env.pool(t)
	wallet, player := openWallet(t, base, "100.00")
	before := scrape(t, base)

	ok := submit(t, base, "provider-a", newOperation("provider-a", wallet, player, "BET", "bet-1", "10.00", ""), "k-1")
	expect(t, ok, http.StatusOK, "status", "PROCESSED")
	expect(t, submit(t, base, "provider-a", newOperation("provider-a", wallet, player, "BET", "bet-1", "11.00", ""), "k-1"),
		http.StatusConflict, "error.code", "IDEMPOTENCY_CONFLICT")
	release := lockWallet(t, db, wallet) // além do lock_timeout de 5s
	expect(t, submit(t, base, "provider-a", newOperation("provider-a", wallet, player, "BET", "bet-2", "1.00", ""), "k-2"),
		http.StatusServiceUnavailable, "error.code", "WALLET_BUSY")
	release()
	env.queues.Send(t, request(t, "msg-1", newOperation("provider-a", wallet, player, "BET", "bet-3", "1.00", ""), "k-3"), wallet, "msg-1")
	eventually(t, 15*time.Second, "the SQS operation", func() bool { return count(t, db, `SELECT count(*) FROM inbox_messages`) == 1 })

	after := scrape(t, base)
	for series, want := range map[string]float64{
		`wagering_processing_duration_seconds_count{source="http"}`:          3,
		`wagering_processing_duration_seconds_count{source="sqs"}`:           1,
		`wagering_conflicts_total{kind="idempotency",source="http"}`:         1,
		`wagering_conflicts_total{kind="wallet_lock_timeout",source="http"}`: 1,
	} {
		if got := after[series] - before[series]; got != want {
			t.Errorf("%s grew by %v, want %v", series, got, want)
		}
	}
	for _, series := range []string{
		`wagering_conflicts_total{kind="idempotency",source="sqs"}`,
		`wagering_conflicts_total{kind="message",source="sqs"}`,
		`wagering_conflicts_total{kind="wallet_lock_timeout",source="sqs"}`,
	} {
		if _, exists := after[series]; !exists {
			t.Errorf("series %s is not exposed", series)
		}
	}

	records := logs.records(t)
	txID := ok.str("transactionId")
	if findLog(records, "wagering operation completed", map[string]string{
		"transactionId": txID, "correlationId": txID, "walletId": wallet, "providerId": "provider-a", "status": "PROCESSED",
	}) == nil {
		t.Errorf("no HTTP operation log with its identifiers in:\n%s", &logs.buf)
	}
	var sqsTx string
	_ = db.QueryRow(t.Context(), `SELECT transaction_id::text FROM inbox_messages WHERE message_id='msg-1'`).Scan(&sqsTx)
	if findLog(records, "sqs message completed", map[string]string{
		"messageId": "msg-1", "transactionId": sqsTx, "correlationId": sqsTx, "walletId": wallet, "providerId": "provider-a",
	}) == nil {
		t.Errorf("no SQS message log with its identifiers in:\n%s", &logs.buf)
	}
	raw, _ := io.ReadAll(bytes.NewReader(logs.buf.Bytes()))
	for _, secret := range []string{token(t, "provider-a"), clientSecret(t, "provider-a"), `"money"`, `"amount"`} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("logs contain %q", secret[:min(len(secret), 12)])
		}
	}
}
