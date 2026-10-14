//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tkosudi/jungle-gaming-challenge/internal/postgres"
	"github.com/tkosudi/jungle-gaming-challenge/internal/sqs"
	"github.com/tkosudi/jungle-gaming-challenge/internal/testenv"
	"github.com/tkosudi/jungle-gaming-challenge/internal/usecase"
)

func (e environment) pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), e.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// request monta o envelope WagerTransactionRequested do enunciado.
func request(t *testing.T, messageID string, op operation, key string) string {
	t.Helper()
	data := map[string]any{"idempotencyKey": key}
	for k, v := range op {
		data[k] = v
	}
	raw, err := json.Marshal(map[string]any{
		"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": time.Now().UTC().Format(time.RFC3339Nano), "data": data,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func walletCents(t *testing.T, pool *pgxpool.Pool, wallet string) int64 {
	t.Helper()
	var cents int64
	if err := pool.QueryRow(context.Background(), `SELECT balance FROM wallets WHERE id=$1`, wallet).Scan(&cents); err != nil {
		t.Fatal(err)
	}
	return cents
}

func TestSQS_RepeatedDeliveriesAndHTTPApplyOneEffect(t *testing.T) {
	env := newEnvironment(t)
	instances := []string{startInstance(t, env), startInstance(t, env), startInstance(t, env)}
	db := env.pool(t)
	wallet, player := openWallet(t, instances[0], "100.00")
	op := newOperation("provider-a", wallet, player, "BET", "bet-1", "30.00", "")

	// O mesmo messageId 50 vezes (deduplicação do SQS contornada de propósito: reenvio do produtor).
	body := request(t, "msg-1", op, "provider-a:bet-1")
	for i := 0; i < 50; i++ {
		env.queues.Send(t, body, wallet, fmt.Sprintf("resend-%d", i))
	}
	eventually(t, 30*time.Second, "50 deliveries to be consumed", func() bool { return env.queues.Depth(t, env.queues.RequestsURL) == 0 })
	if n := count(t, db, `SELECT count(*) FROM inbox_messages`); n != 1 {
		t.Fatalf("inbox rows = %d, want 1", n)
	}
	if cents := walletCents(t, db, wallet); cents != 7000 {
		t.Fatalf("wallet = %d, want 7000", cents)
	}

	// A mesma operação por HTTP é replay; por outra mensagem, também.
	viaHTTP := submit(t, instances[1], "provider-a", op, "provider-a:bet-1")
	expect(t, viaHTTP, http.StatusOK, "status", "PROCESSED", "balance.amount", "70.00")
	if viaHTTP.body["idempotentReplay"] != true {
		t.Fatalf("HTTP after SQS = %v, want replay", viaHTTP.body)
	}
	env.queues.Send(t, request(t, "msg-2", op, "provider-a:bet-1"), wallet, "msg-2")
	eventually(t, 15*time.Second, "msg-2", func() bool { return count(t, db, `SELECT count(*) FROM inbox_messages`) == 2 })

	// 50 mensagens distintas, em grupos distintos (consumo em paralelo pelas 3 instâncias).
	op2 := newOperation("provider-a", wallet, player, "BET", "bet-2", "10.00", "")
	for i := 0; i < 50; i++ {
		env.queues.Send(t, request(t, fmt.Sprintf("multi-%d", i), op2, "provider-a:bet-2"), fmt.Sprintf("group-%d", i), fmt.Sprintf("multi-%d", i))
	}
	eventually(t, 30*time.Second, "50 distinct messages", func() bool { return count(t, db, `SELECT count(*) FROM inbox_messages`) == 52 })
	if n := count(t, db, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`, wallet); n != 2 {
		t.Fatalf("debits = %d, want one per operation (2)", n)
	}
	if cents := walletCents(t, db, wallet); cents != 6000 {
		t.Fatalf("wallet = %d, want 6000", cents)
	}
}

func TestSQS_TwoBetsOf80OnBalance100(t *testing.T) {
	env := newEnvironment(t)
	instances := []string{startInstance(t, env), startInstance(t, env), startInstance(t, env)}
	db := env.pool(t)
	for round := 0; round < 3; round++ {
		wallet, player := openWallet(t, instances[0], "100.00")
		for i := 0; i < 2; i++ {
			id := fmt.Sprintf("bet-80-%d-%d", round, i)
			op := newOperation("provider-a", wallet, player, "BET", id, "80.00", "")
			// Grupos distintos: o FIFO não serializa as duas; a garantia vem do PostgreSQL.
			env.queues.Send(t, request(t, id, op, "k-"+id), id, id)
		}
		eventually(t, 20*time.Second, "both bets", func() bool {
			return count(t, db, `SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND kind='BET'`, wallet) == 2
		})
		processed := count(t, db, `SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND kind='BET' AND status='PROCESSED'`, wallet)
		rejected := count(t, db, `SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND kind='BET' AND status='REJECTED' AND failure_code='INSUFFICIENT_FUNDS'`, wallet)
		if processed != 1 || rejected != 1 || walletCents(t, db, wallet) != 2000 {
			t.Fatalf("round %d: processed=%d rejected=%d balance=%d", round, processed, rejected, walletCents(t, db, wallet))
		}
	}
}

func TestSQS_InjectedFailureLeavesNothingAndTheRedeliveryAppliesOnce(t *testing.T) {
	env := newEnvironment(t)
	base := startInstance(t, env)
	db := env.pool(t)
	wallet, player := openWallet(t, base, "100.00")
	outbox := count(t, db, `SELECT count(*) FROM outbox_events`)

	if _, err := db.Exec(context.Background(), `
		CREATE FUNCTION test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced failure'; END $$;
		CREATE TRIGGER test_fail BEFORE INSERT ON inbox_messages FOR EACH ROW EXECUTE FUNCTION test_fail();`); err != nil {
		t.Fatal(err)
	}
	op := newOperation("provider-a", wallet, player, "BET", "bet-1", "30.00", "")
	env.queues.Send(t, request(t, "msg-1", op, "k-1"), wallet, "msg-1")
	time.Sleep(3 * time.Second) // ao menos duas tentativas (atraso inicial de 1s)

	if env.queues.Depth(t, env.queues.RequestsURL) != 1 {
		t.Fatal("the failed message left the queue")
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM inbox_messages`:                      0,
		`SELECT count(*) FROM wager_transactions WHERE kind='BET'`: 0,
		`SELECT count(*) FROM outbox_events`:                       outbox,
	} {
		if n := count(t, db, query); n != want {
			t.Fatalf("%s = %d, want %d", query, n, want)
		}
	}
	if walletCents(t, db, wallet) != 10000 {
		t.Fatal("wallet changed although the transaction failed")
	}

	if _, err := db.Exec(context.Background(), `DROP TRIGGER test_fail ON inbox_messages; DROP FUNCTION test_fail()`); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, "the redelivery", func() bool { return env.queues.Depth(t, env.queues.RequestsURL) == 0 })
	if n := count(t, db, `SELECT count(*) FROM inbox_messages`); n != 1 || walletCents(t, db, wallet) != 7000 {
		t.Fatalf("inbox = %d, wallet = %d; want 1 and 7000", n, walletCents(t, db, wallet))
	}
}

// Queda depois do commit e antes do DeleteMessage: a mensagem volta e a inbox a reconhece.
func TestSQS_CrashAfterCommitBeforeAck(t *testing.T) {
	env := newEnvironment(t)
	base, stop := startConfigured(t, env.instanceConfig())
	wallet, player := openWallet(t, base, "100.00")
	if err := stop(); err != nil { // nenhuma instância consumindo
		t.Fatal(err)
	}
	db := env.pool(t)
	op := newOperation("provider-a", wallet, player, "BET", "bet-1", "30.00", "")
	env.queues.Send(t, request(t, "msg-1", op, "k-1"), wallet, "msg-1")

	// "Processo A": recebe, confirma no banco e cai antes de apagar a mensagem.
	out, err := env.queues.Client.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
		QueueUrl: aws.String(env.queues.RequestsURL), WaitTimeSeconds: 5, VisibilityTimeout: 2,
	})
	if err != nil || len(out.Messages) != 1 {
		t.Fatalf("receive = %v, %v", out, err)
	}
	req, err := sqs.ParseRequest([]byte(aws.ToString(out.Messages[0].Body)))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.NewPool(context.Background(), env.dbURL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	svc, err := usecase.NewService(postgres.NewStore(pool, 5*time.Second), usecase.DefaultReferencePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessMessage(context.Background(), usecase.InboxMessage{
		ConsumerName: env.queues.Config.ConsumerName, MessageID: req.MessageID, PayloadHash: req.ContentHash, ReceivedAt: time.Now(),
	}, req.ProviderID, req.Operation); err != nil {
		t.Fatal(err)
	}

	// "Processo B" assume a reentrega.
	startInstance(t, env)
	eventually(t, 20*time.Second, "the redelivered message to be acknowledged", func() bool {
		return env.queues.Depth(t, env.queues.RequestsURL) == 0
	})
	if n := count(t, db, `SELECT count(*) FROM inbox_messages`); n != 1 || walletCents(t, db, wallet) != 7000 {
		t.Fatalf("inbox = %d, wallet = %d; want 1 and 7000 (applied once)", n, walletCents(t, db, wallet))
	}
}

func TestSQS_PermanentFailuresGoToTheDLQWithTheirReason(t *testing.T) {
	env := newEnvironment(t)
	base := startInstance(t, env)
	db := env.pool(t)
	wallet, player := openWallet(t, base, "100.00")
	first := newOperation("provider-a", wallet, player, "BET", "bet-1", "10.00", "")
	env.queues.Send(t, request(t, "msg-1", first, "k-1"), wallet, "msg-1")
	eventually(t, 15*time.Second, "the first message", func() bool { return count(t, db, `SELECT count(*) FROM inbox_messages`) == 1 })

	untrusted := newOperation("provider-c", wallet, player, "BET", "bet-c", "10.00", "")
	env.queues.Send(t, `{"messageId": "broken"`, "g-invalid", "invalid")
	env.queues.Send(t, request(t, "msg-untrusted", untrusted, "k-c"), "g-untrusted", "untrusted")
	env.queues.Send(t, request(t, "msg-1", newOperation("provider-a", wallet, player, "BET", "bet-1", "99.00", ""), "k-1"), "g-conflict", "conflict")
	env.queues.Send(t, request(t, "msg-other", newOperation("provider-a", wallet, player, "BET", "bet-1", "20.00", ""), "k-1"), "g-idem", "idem")

	var reasons []string
	eventually(t, 20*time.Second, "4 dead-lettered messages", func() bool {
		for _, m := range env.queues.Drain(t, env.queues.DeadLetterURL, time.Second) {
			reasons = append(reasons, aws.ToString(m.MessageAttributes["failureReason"].StringValue))
		}
		return len(reasons) >= 4
	})
	sort.Strings(reasons)
	if fmt.Sprint(reasons) != "[IDEMPOTENCY_CONFLICT INVALID_MESSAGE MESSAGE_CONFLICT UNTRUSTED_PROVIDER]" {
		t.Fatalf("reasons = %v", reasons)
	}
	if env.queues.Depth(t, env.queues.RequestsURL) != 0 {
		t.Fatal("permanent failures stayed in the input queue")
	}
	if walletCents(t, db, wallet) != 9000 || count(t, db, `SELECT count(*) FROM inbox_messages`) != 1 {
		t.Fatal("a refused message changed the wallet or the inbox")
	}
}

func TestSQS_TransientFailuresReachTheDLQThroughRedrive(t *testing.T) {
	env := environment{dbURL: newDatabase(t), queues: testenv.SQSQueues(t, 2)}
	cfg := env.instanceConfig()
	cfg.SQS.RetryBaseDelay, cfg.SQS.RetryMaxDelay = 0, 0
	base, stop := startConfigured(t, cfg)
	t.Cleanup(func() { _ = stop() })
	db := env.pool(t)
	wallet, player := openWallet(t, base, "100.00")
	if _, err := db.Exec(context.Background(), `
		CREATE FUNCTION test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'database keeps failing'; END $$;
		CREATE TRIGGER test_fail BEFORE INSERT ON inbox_messages FOR EACH ROW EXECUTE FUNCTION test_fail();`); err != nil {
		t.Fatal(err)
	}
	env.queues.Send(t, request(t, "msg-1", newOperation("provider-a", wallet, player, "BET", "bet-1", "10.00", ""), "k-1"), wallet, "msg-1")

	var dead []string
	eventually(t, 30*time.Second, "the redrive to the DLQ", func() bool {
		for _, m := range env.queues.Drain(t, env.queues.DeadLetterURL, time.Second) {
			dead = append(dead, aws.ToString(m.Body))
		}
		return len(dead) > 0
	})
	if env.queues.Depth(t, env.queues.RequestsURL) != 0 || walletCents(t, db, wallet) != 10000 {
		t.Fatal("the message must leave the input queue only through the DLQ, without effects")
	}
}

func TestOutbox_TwoPublishersDeliverEveryEventInWalletOrder(t *testing.T) {
	env := newEnvironment(t)
	instances := []string{startInstance(t, env), startInstance(t, env)}
	db := env.pool(t)
	for i := 0; i < 4; i++ {
		wallet, player := openWallet(t, instances[i%2], "100.00")
		for j := 0; j < 3; j++ {
			id := fmt.Sprintf("bet-%d-%d", i, j)
			submit(t, instances[(i+j)%2], "provider-a", newOperation("provider-a", wallet, player, "BET", id, "1.00", ""), "k-"+id)
		}
	}
	total := count(t, db, `SELECT count(*) FROM outbox_events`)
	eventually(t, 20*time.Second, "every event published", func() bool {
		return count(t, db, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) == 0
	})

	received := env.queues.Drain(t, env.queues.EventsURL, 3*time.Second)
	byGroup := map[string][]string{}
	seen := map[string]bool{}
	for _, m := range received {
		var envlp struct{ EventID, AggregateID string }
		if err := json.Unmarshal([]byte(aws.ToString(m.Body)), &envlp); err != nil {
			t.Fatal(err)
		}
		if envlp.EventID != aws.ToString(m.MessageAttributes["eventId"].StringValue) {
			t.Fatalf("payload eventId %s differs from the message attribute", envlp.EventID)
		}
		if !seen[envlp.EventID] {
			seen[envlp.EventID] = true
			group := m.Attributes["MessageGroupId"]
			byGroup[group] = append(byGroup[group], envlp.EventID)
		}
	}
	if len(seen) != total {
		t.Fatalf("distinct events received = %d, want %d", len(seen), total)
	}
	for group, got := range byGroup {
		rows, err := db.Query(context.Background(), `SELECT event_id::text FROM outbox_events WHERE message_group_id=$1 ORDER BY sequence`, group)
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			want = append(want, id)
		}
		rows.Close()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("group %s received %v, want %v", group, got, want)
		}
	}
}

func TestOutbox_RecoveryAfterSendBeforeConfirmation(t *testing.T) {
	env := newEnvironment(t)
	db := env.pool(t)
	// Antes de qualquer instância: toda marcação de publicado falha depois de o SQS aceitar o envio.
	if _, err := db.Exec(context.Background(), `
		CREATE FUNCTION test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'confirmation lost'; END $$;
		CREATE TRIGGER test_fail BEFORE UPDATE ON outbox_events FOR EACH ROW
			WHEN (NEW.published_at IS NOT NULL) EXECUTE FUNCTION test_fail();`); err != nil {
		t.Fatal(err)
	}
	base := startInstance(t, env)
	wallet, player := openWallet(t, base, "100.00")
	submit(t, base, "provider-a", newOperation("provider-a", wallet, player, "BET", "bet-1", "1.00", ""), "k-1")
	eventually(t, 10*time.Second, "sends without confirmation", func() bool { return env.queues.Depth(t, env.queues.EventsURL) > 0 })
	if count(t, db, `SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL`) != 0 {
		t.Fatal("an event was marked published although the confirmation failed")
	}

	if _, err := db.Exec(context.Background(), `DROP TRIGGER test_fail ON outbox_events; DROP FUNCTION test_fail()`); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, "recovery", func() bool {
		return count(t, db, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) == 0
	})
	ids := map[string]bool{}
	for _, m := range env.queues.Drain(t, env.queues.EventsURL, 3*time.Second) {
		ids[aws.ToString(m.MessageAttributes["eventId"].StringValue)] = true
	}
	rows, err := db.Query(context.Background(), `SELECT event_id::text FROM outbox_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		if !ids[id] {
			t.Fatalf("event %s was never delivered", id)
		}
		delete(ids, id)
	}
	if len(ids) != 0 {
		t.Fatalf("unknown event ids delivered: %v", ids)
	}
}

func TestReferences_ResolvedAutomaticallyOrExpired(t *testing.T) {
	env := newEnvironment(t)
	base := startInstance(t, env)
	db := env.pool(t)
	wallet, player := openWallet(t, base, "100.00")
	status := func(external string) string {
		var s string
		_ = db.QueryRow(context.Background(), `SELECT status || ':' || COALESCE(failure_code, '') FROM wager_transactions WHERE external_transaction_id=$1`, external).Scan(&s)
		return s
	}

	refund := newOperation("provider-a", wallet, player, "REFUND", "refund-1", "30.00", "bet-1")
	env.queues.Send(t, request(t, "msg-refund", refund, "k-refund"), wallet, "msg-refund")
	eventually(t, 15*time.Second, "pending refund", func() bool { return status("refund-1") == "PENDING_REFERENCE:" })
	submit(t, base, "provider-a", newOperation("provider-a", wallet, player, "BET", "bet-1", "30.00", ""), "k-bet")
	eventually(t, 15*time.Second, "automatic resolution", func() bool { return status("refund-1") == "PROCESSED:" })
	if walletCents(t, db, wallet) != 10000 {
		t.Fatalf("wallet = %d, want 10000", walletCents(t, db, wallet))
	}

	// Outra instância, com poucas tentativas: a referência que nunca chega expira.
	cfg := env.instanceConfig()
	cfg.Workers.ReferenceMaxAttempts = 2
	_, stop := startConfigured(t, cfg)
	t.Cleanup(func() { _ = stop() })
	rollback := newOperation("provider-a", wallet, player, "ROLLBACK", "rb-1", "5.00", "never-sent")
	env.queues.Send(t, request(t, "msg-rb", rollback, "k-rb"), wallet, "msg-rb")
	eventually(t, 20*time.Second, "expiration", func() bool { return status("rb-1") == "REJECTED:REFERENCE_NOT_FOUND" })
}

func TestWorkers_StopReleasesTheQueueAndARestartResumes(t *testing.T) {
	env := newEnvironment(t)
	base, stop := startConfigured(t, env.instanceConfig())
	ready := call(t, http.MethodGet, base+"/health/ready", "", nil)
	expect(t, ready, http.StatusOK, "checks.postgres", "ok", "checks.sqs", "ok")
	wallet, player := openWallet(t, base, "100.00")

	begin := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// O shutdown espera no máximo o long polling em curso (5s) e não deixa mensagem presa.
	if elapsed := time.Since(begin); elapsed > 8*time.Second {
		t.Fatalf("stop took %v", elapsed)
	}
	db := env.pool(t)
	env.queues.Send(t, request(t, "msg-1", newOperation("provider-a", wallet, player, "BET", "bet-1", "10.00", ""), "k-1"), wallet, "msg-1")
	time.Sleep(time.Second)
	if env.queues.Depth(t, env.queues.RequestsURL) != 1 || count(t, db, `SELECT count(*) FROM inbox_messages`) != 0 {
		t.Fatal("a stopped instance kept consuming")
	}

	startInstance(t, env)
	eventually(t, 15*time.Second, "the restarted instance to consume", func() bool {
		return count(t, db, `SELECT count(*) FROM inbox_messages`) == 1 && env.queues.Depth(t, env.queues.RequestsURL) == 0
	})
}

// Duas mensagens do mesmo grupo: enquanto a primeira falha, a segunda não pode produzir
// efeitos; ela só é tratada depois de a primeira ser concluída ou ir para a DLQ.
func TestSQS_SameGroupWaitsForTheFirstMessage(t *testing.T) {
	failFirst := `
		CREATE FUNCTION test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'first message fails'; END $$;
		CREATE TRIGGER test_fail BEFORE INSERT ON inbox_messages FOR EACH ROW
			WHEN (NEW.message_id = 'msg-1') EXECUTE FUNCTION test_fail();`
	for _, tc := range []struct {
		name            string
		maxReceiveCount int
		recovers        bool // a falha some (conclusão) ou persiste até o redrive (descarte)
		wantInbox       int
		wantCents       int64
	}{
		{"first message recovers", 10, true, 2, 6000},
		{"first message is dead-lettered", 2, false, 1, 8000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := environment{dbURL: newDatabase(t), queues: testenv.SQSQueues(t, tc.maxReceiveCount)}
			cfg := env.instanceConfig()
			cfg.SQS.RetryBaseDelay, cfg.SQS.RetryMaxDelay = time.Second, time.Second
			base, stop := startConfigured(t, cfg)
			t.Cleanup(func() { _ = stop() })
			db := env.pool(t)
			wallet, player := openWallet(t, base, "100.00")
			if _, err := db.Exec(context.Background(), failFirst); err != nil {
				t.Fatal(err)
			}
			env.queues.Send(t, request(t, "msg-1", newOperation("provider-a", wallet, player, "BET", "bet-1", "20.00", ""), "k-1"), wallet, "msg-1")
			env.queues.Send(t, request(t, "msg-2", newOperation("provider-a", wallet, player, "BET", "bet-2", "20.00", ""), "k-2"), wallet, "msg-2")

			time.Sleep(2500 * time.Millisecond) // a primeira já falhou mais de uma vez
			if tc.recovers {
				if n := count(t, db, `SELECT count(*) FROM wager_transactions WHERE kind='BET'`); n != 0 {
					t.Fatalf("bets = %d while the first message of the group is failing", n)
				}
				if _, err := db.Exec(context.Background(), `DROP TRIGGER test_fail ON inbox_messages; DROP FUNCTION test_fail()`); err != nil {
					t.Fatal(err)
				}
			}
			eventually(t, 30*time.Second, "the group to drain", func() bool {
				return count(t, db, `SELECT count(*) FROM inbox_messages`) == tc.wantInbox && env.queues.Depth(t, env.queues.RequestsURL) == 0
			})
			if got := walletCents(t, db, wallet); got != tc.wantCents {
				t.Fatalf("wallet = %d, want %d", got, tc.wantCents)
			}
			if tc.recovers {
				var first, second time.Time
				_ = db.QueryRow(context.Background(), `SELECT completed_at FROM inbox_messages WHERE message_id='msg-1'`).Scan(&first)
				_ = db.QueryRow(context.Background(), `SELECT completed_at FROM inbox_messages WHERE message_id='msg-2'`).Scan(&second)
				if !first.Before(second) {
					t.Fatalf("msg-2 completed at %v, before msg-1 at %v", second, first)
				}
			} else {
				dead := env.queues.Drain(t, env.queues.DeadLetterURL, 2*time.Second)
				if len(dead) != 1 || !strings.Contains(aws.ToString(dead[0].Body), `"msg-1"`) {
					t.Fatalf("dead letters = %d, want only msg-1", len(dead))
				}
			}
		})
	}
}
