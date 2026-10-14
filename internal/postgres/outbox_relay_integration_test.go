//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// sentLog registra os envios do publicador, na ordem em que o "broker" os confirmou.
type sentLog struct {
	mu   sync.Mutex
	sent []OutboxMessage
}

func (l *sentLog) send(_ context.Context, m OutboxMessage) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, m)
	return nil
}

func noDelay(int) time.Duration { return 0 }

func publishAll(t *testing.T, store *Store, send func(context.Context, OutboxMessage) error) int {
	t.Helper()
	total := 0
	for i := 0; i < 100; i++ {
		res, err := store.PublishOutbox(context.Background(), 10, noDelay, send)
		if err != nil {
			t.Fatalf("PublishOutbox: %v", err)
		}
		if res.Published+res.Failed == 0 {
			return total
		}
		total += res.Published
	}
	t.Fatal("outbox did not drain")
	return total
}

// orderedByGroup confere que, em cada grupo, os eventos saíram na ordem de sequence.
func orderedByGroup(t *testing.T, sent []OutboxMessage) {
	t.Helper()
	last := map[string]int64{}
	for _, m := range sent {
		if m.Sequence < last[m.MessageGroupID] {
			t.Fatalf("group %s: sequence %d sent after %d", m.MessageGroupID, m.Sequence, last[m.MessageGroupID])
		}
		last[m.MessageGroupID] = m.Sequence
	}
}

func (db *schemaDB) unpublished(t *testing.T) int {
	t.Helper()
	return db.count(t, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`)
}

func TestPublishOutbox_PublishesEveryEventInOrderAndMarksAfterTheSend(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	w := openFunded(t, svc, "100.00")
	for i := 0; i < 3; i++ {
		submit(t, svc, newOp(t, w, "BET", fmt.Sprintf("bet-%d", i), "1.00", ""))
	}
	events := db.unpublished(t)

	var log sentLog
	store := NewStore(db.pool, time.Second)
	if n := publishAll(t, store, log.send); n != events {
		t.Fatalf("published = %d, want %d", n, events)
	}
	orderedByGroup(t, log.sent)
	if db.unpublished(t) != 0 || len(log.sent) != events {
		t.Fatalf("unpublished = %d, sent = %d", db.unpublished(t), len(log.sent))
	}
	var payloadEventID string
	if err := db.pool.QueryRow(context.Background(), `SELECT payload->>'eventId' FROM outbox_events WHERE event_id=$1`, log.sent[0].EventID).Scan(&payloadEventID); err != nil || payloadEventID != log.sent[0].EventID.String() {
		t.Fatalf("payload eventId = %q, %v", payloadEventID, err)
	}
}

func TestPublishOutbox_FailedSendBlocksOnlyItsGroupAndIsRetried(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	blocked := openFunded(t, svc, "100.00")
	free := openFunded(t, svc, "100.00")
	submit(t, svc, newOp(t, blocked, "BET", "b-1", "1.00", ""))
	submit(t, svc, newOp(t, free, "BET", "f-1", "1.00", ""))
	store := NewStore(db.pool, time.Second)

	var log sentLog
	failing := func(ctx context.Context, m OutboxMessage) error {
		if m.MessageGroupID == blocked.ID.String() {
			return errors.New("broker unavailable")
		}
		return log.send(ctx, m)
	}
	// Atraso longo: o evento que falhou não volta neste teste até ser liberado à mão.
	res, err := store.PublishOutbox(context.Background(), 10, func(int) time.Duration { return time.Hour }, failing)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 {
		t.Fatalf("result = %+v, want one failure", res)
	}
	publishAll(t, store, failing)
	for _, m := range log.sent {
		if m.MessageGroupID == blocked.ID.String() {
			t.Fatalf("event %d of the failing wallet was published", m.Sequence)
		}
	}
	if n := db.count(t, `SELECT count(*) FROM outbox_events WHERE message_group_id=$1 AND published_at IS NULL`, blocked.ID.String()); n != 4 {
		t.Fatalf("unpublished events of the failing wallet = %d, want all 4 (opening and bet)", n)
	}
	if n := db.count(t, `SELECT count(*) FROM outbox_events WHERE message_group_id=$1 AND attempts=1 AND next_attempt_at > now() + interval '50 minutes'`, blocked.ID.String()); n != 1 {
		t.Fatalf("scheduled retries = %d, want the head event only", n)
	}
	if n := db.count(t, `SELECT count(*) FROM outbox_events WHERE message_group_id=$1 AND published_at IS NULL`, free.ID.String()); n != 0 {
		t.Fatalf("the healthy wallet still has %d unpublished events", n)
	}

	db.mustExec(t, fmt.Sprintf(`UPDATE outbox_events SET next_attempt_at = now() WHERE message_group_id='%s'`, blocked.ID))
	log.sent = nil
	publishAll(t, store, log.send)
	orderedByGroup(t, log.sent)
	if db.unpublished(t) != 0 {
		t.Fatalf("unpublished after recovery = %d", db.unpublished(t))
	}
}

func TestPublishOutbox_FailureBetweenSendAndConfirmationRepublishesTheSameEvent(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	openFunded(t, svc, "100.00")
	store := NewStore(db.pool, time.Second)

	// O envio é confirmado pelo "broker", mas a marcação de publicado falha (trigger de teste).
	db.mustExec(t, `
		CREATE FUNCTION test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced failure'; END $$;
		CREATE TRIGGER test_fail BEFORE UPDATE ON outbox_events FOR EACH ROW
			WHEN (NEW.published_at IS NOT NULL) EXECUTE FUNCTION test_fail();`)
	var log sentLog
	if _, err := store.PublishOutbox(context.Background(), 10, noDelay, log.send); err == nil {
		t.Fatal("PublishOutbox succeeded although the confirmation failed")
	}
	firstSends := len(log.sent)
	if firstSends == 0 || db.unpublished(t) != 2 {
		t.Fatalf("sent = %d, unpublished = %d; want sends and nothing marked", firstSends, db.unpublished(t))
	}

	db.mustExec(t, `DROP TRIGGER test_fail ON outbox_events; DROP FUNCTION test_fail()`)
	publishAll(t, store, log.send)
	if db.unpublished(t) != 0 {
		t.Fatalf("unpublished after recovery = %d", db.unpublished(t))
	}
	byID := map[uuid.UUID][]OutboxMessage{}
	for _, m := range log.sent {
		byID[m.EventID] = append(byID[m.EventID], m)
	}
	// Todo evento enviado na tentativa sem confirmação foi reenviado, com o mesmo eventId e payload.
	for _, first := range log.sent[:firstSends] {
		id, sends := first.EventID, byID[first.EventID]
		if len(sends) < 2 || string(sends[0].Payload) != string(sends[1].Payload) {
			t.Fatalf("event %s: %d sends, want a republication with the identical payload", id, len(sends))
		}
	}
}

func TestPublishOutbox_ConcurrentPublishersLoseNothingAndKeepWalletOrder(t *testing.T) {
	db := newSchemaDB(t)
	svc := db.instances(t, 1)[0]
	for i := 0; i < 8; i++ {
		w := openFunded(t, svc, "100.00")
		for j := 0; j < 4; j++ {
			submit(t, svc, newOp(t, w, "BET", fmt.Sprintf("bet-%d-%d", i, j), "1.00", ""))
		}
	}
	events := db.unpublished(t)

	var log sentLog
	stores := []*Store{NewStore(openPool(t, db.url), time.Second), NewStore(openPool(t, db.url), time.Second)}
	runConcurrently(len(stores), func(i int) {
		for n := 0; n < 200 && db.unpublished(t) > 0; n++ {
			if _, err := stores[i].PublishOutbox(context.Background(), 3, noDelay, log.send); err != nil {
				t.Errorf("publisher %d: %v", i, err)
				return
			}
		}
	})
	if db.unpublished(t) != 0 {
		t.Fatalf("unpublished = %d", db.unpublished(t))
	}
	seen := map[uuid.UUID]int{}
	for _, m := range log.sent {
		seen[m.EventID]++
	}
	if len(seen) != events || len(log.sent) != events {
		t.Fatalf("distinct events sent = %d, sends = %d, want %d each", len(seen), len(log.sent), events)
	}
	orderedByGroup(t, log.sent)
}
