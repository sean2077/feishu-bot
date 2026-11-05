package feishu

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func seedCardState(t *testing.T, db *sql.DB, room, message string, card core.Args, updated, terminal int64) {
	t.Helper()
	snapshot, err := canonicalJSON(card)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO feishu_cards(message,card,room,sequence,snapshot) VALUES(?,?,?,0,?)`, message, "card_"+message, room, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO feishu_card_lifecycle(message,updated_at,terminal_at) VALUES(?,?,?)`, message, updated, terminal); err != nil {
		t.Fatal(err)
	}
}

func seedDeliveryState(t *testing.T, db *sql.DB, room, id, message, state string, updated int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO feishu_deliveries(id,step,room,fingerprint,payload,uuid,state,result,updated_at,message)
		VALUES(?,'message',?,'fixture','{}','uuid_fixture',?,?,?,?)`, id, room, state, message, updated, message); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredStreamingFallsBackToSameCardAndStaysNonterminal(t *testing.T) {
	for _, code := range []int{200850, 300309} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			db := fixtureDB(t)
			var writes []observedWrite
			handler := func(w http.ResponseWriter, r *http.Request) {
				body := decodedBody(t, r)
				writes = append(writes, observedWrite{r.Method, r.URL.Path, body})
				if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "cardkit") {
					apiResponse(w, core.Args{"card_id": "card_same"})
					return
				}
				if r.Method == http.MethodPost {
					apiResponse(w, core.Args{"message_id": "om_same"})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/content") {
					jsonResponse(w, 400, core.Args{"code": code})
					return
				}
				apiResponse(w, core.Args{})
			}
			c, _ := fixtureClient(t, db, handler)
			id, err := c.Card(context.Background(), fixtureRoom(t, c), replyFixture("old", false), "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err = c.Update(context.Background(), id, replyFixture("after long wait", false)); err != nil {
				t.Fatal(err)
			}
			if len(writes) != 4 || writes[2].body["sequence"] != float64(1) || writes[3].path != "/open-apis/cardkit/v1/cards/card_same" || writes[3].body["sequence"] != float64(2) {
				t.Fatalf("expiry changed identity or reused sequence: %+v", writes)
			}
			var terminal int64
			var closed int
			if err = db.QueryRow(`SELECT terminal_at,stream_closed FROM feishu_card_lifecycle WHERE message=?`, id).Scan(&terminal, &closed); err != nil || terminal != 0 || closed != 1 {
				t.Fatalf("expiry completed a live turn: terminal=%d closed=%d err=%v", terminal, closed, err)
			}
			c2, _ := fixtureClient(t, db, handler)
			if err = c2.Update(context.Background(), id, replyFixture("later answer", false)); err != nil {
				t.Fatal(err)
			}
			if len(writes) != 5 || writes[4].path != writes[3].path || writes[4].body["sequence"] != float64(3) {
				t.Fatalf("restart forgot closed stream: %+v", writes)
			}
			if err = c2.Update(core.WithTerminalCard(context.Background()), id, replyFixture("final answer", true)); err != nil {
				t.Fatal(err)
			}
			if err = db.QueryRow(`SELECT terminal_at FROM feishu_card_lifecycle WHERE message=?`, id).Scan(&terminal); err != nil || terminal <= 0 {
				t.Fatal("final acknowledgement was not marked terminal")
			}
		})
	}
}

func TestOversizedChineseCardIsDefinitivelyRejectedBeforeNetwork(t *testing.T) {
	calls := 0
	c, _ := fixtureClient(t, fixtureDB(t), func(w http.ResponseWriter, r *http.Request) { calls++; apiResponse(w, core.Args{}) })
	_, err := c.Card(context.Background(), fixtureRoom(t, c), replyFixture(strings.Repeat("中", 10000), false), "", "")
	var failure core.DeliveryFailure
	if !errors.As(err, &failure) || failure.DeliveryUncertain() || failure.DeliveryRetryable() || calls != 0 {
		t.Fatalf("oversize should be a definite local rejection: %v calls=%d", err, calls)
	}
}

func TestRetentionPreservesActiveOutboxUncertainAndLogicalReferences(t *testing.T) {
	db := fixtureDB(t)
	c, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) {
		t.Error("retention made a network request")
		apiResponse(w, core.Args{})
	})
	room := fixtureRoom(t, c)
	old := time.Now().Add(-60 * 24 * time.Hour).UnixMilli()
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	if _, err := db.Exec(`CREATE TABLE outbox(id TEXT PRIMARY KEY,state TEXT,target TEXT,file_target TEXT,delivery_key TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE callback_controls(message TEXT PRIMARY KEY,value TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"om_old", "om_active", "om_protected", "om_logical", "om_unknown", "om_pending", "om_pending_key", "om_recent"} {
		terminal, updated := old, old
		if message == "om_active" {
			terminal = 0
		}
		if message == "om_recent" {
			terminal, updated = time.Now().UnixMilli(), time.Now().UnixMilli()
		}
		seedCardState(t, db, room.Key, message, replyFixture(message, true), updated, terminal)
		state := "confirmed"
		if message == "om_unknown" {
			state = "uncertain"
		}
		seedDeliveryState(t, db, room.Key, "delivery:"+message, message, state, old)
		if _, err := db.Exec(`INSERT INTO callback_controls(message,value) VALUES(?,'authorized fixture control')`, message); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO outbox(id,state,target,delivery_key) VALUES('pending','pending','om_pending',NULL),('pending-key','uncertain',NULL,'delivery:om_pending_key')`); err != nil {
		t.Fatal(err)
	}
	removed, err := c.Retain(context.Background(), cutoff, []string{"om_protected", "delivery:om_logical"})
	if err != nil || removed != 1 {
		t.Fatalf("retention removed=%d err=%v", removed, err)
	}
	for _, message := range []string{"om_active", "om_protected", "om_logical", "om_unknown", "om_pending", "om_pending_key", "om_recent"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM feishu_cards WHERE message=?`, message).Scan(&count); err != nil || count != 1 {
			t.Fatalf("protected card %s was deleted: %v", message, err)
		}
	}
	var cards, controls, oldJournal int
	if err = db.QueryRow(`SELECT COUNT(*) FROM feishu_cards`).Scan(&cards); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM callback_controls`).Scan(&controls); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM feishu_deliveries WHERE id='delivery:om_old'`).Scan(&oldJournal); err != nil {
		t.Fatal(err)
	}
	if cards != 7 || controls != 8 || oldJournal != 0 {
		t.Fatalf("retention leaked records or changed controls: cards=%d controls=%d old journal=%d", cards, controls, oldJournal)
	}
}

func TestRetentionProtectedFirstPageDoesNotStarveLaterCards(t *testing.T) {
	db := fixtureDB(t)
	c, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) { t.Error("retention wrote to provider") })
	room := fixtureRoom(t, c)
	old := time.Now().Add(-60 * 24 * time.Hour).UnixMilli()
	var protected []string
	for i := 0; i < 101; i++ {
		message := fmt.Sprintf("om_%03d", i)
		seedCardState(t, db, room.Key, message, replyFixture("final", true), old, old)
		if i < 100 {
			protected = append(protected, message)
		}
	}
	removed, err := c.Retain(context.Background(), time.Now().Add(-30*24*time.Hour), protected)
	if err != nil || removed != 1 {
		t.Fatalf("protected first page starved cleanup: removed=%d error=%v", removed, err)
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM feishu_cards WHERE message='om_100'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("later eligible card was not removed")
	}
}

func TestRetentionScanBudgetResumesAfterRestart(t *testing.T) {
	db := fixtureDB(t)
	handler := func(w http.ResponseWriter, r *http.Request) { t.Error("retention called provider") }
	c, _ := fixtureClient(t, db, handler)
	room := fixtureRoom(t, c)
	old := time.Now().Add(-60 * 24 * time.Hour).UnixMilli()
	var protected []string
	for i := 0; i < 202; i++ {
		message := fmt.Sprintf("om_scan_%03d", i)
		seedCardState(t, db, room.Key, message, replyFixture("saved", true), old, old)
		if i < 201 {
			protected = append(protected, message)
		}
	}
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	if removed, err := c.Retain(context.Background(), cutoff, protected); err != nil || removed != 0 {
		t.Fatalf("sweep exceeded its 200-row scan budget: removed=%d err=%v", removed, err)
	}
	var cursor string
	if err := db.QueryRow("SELECT record FROM feishu_maintenance WHERE name='cards'").Scan(&cursor); err != nil || cursor != "om_scan_199" {
		t.Fatalf("scan progress was not durable: %q %v", cursor, err)
	}
	c2, _ := fixtureClient(t, db, handler)
	if removed, err := c2.Retain(context.Background(), cutoff, protected); err != nil || removed != 1 {
		t.Fatalf("restart starved the later eligible card: removed=%d err=%v", removed, err)
	}
}

func TestCrashRecoveryUpdatesOnlyAuthorizedUnownedExistingCards(t *testing.T) {
	db := fixtureDB(t)
	var writes []observedWrite
	c, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) {
		writes = append(writes, observedWrite{r.Method, r.URL.Path, decodedBody(t, r)})
		apiResponse(w, core.Args{})
	})
	room := fixtureRoom(t, c)
	if _, err := db.Exec(`CREATE TABLE outbox(id TEXT PRIMARY KEY,state TEXT,target TEXT,file_target TEXT,delivery_key TEXT)`); err != nil {
		t.Fatal(err)
	}
	card := replyFixture("partial answer", false)
	body, _ := object(card["body"])
	elements := body["elements"].([]any)
	body["elements"] = elements[:1] // Ordinary reply cards may not have a status element.
	now := time.Now().UnixMilli()
	seedCardState(t, db, room.Key, "om_interrupted", card, now, 0)
	seedCardState(t, db, room.Key, "om_waiting", replyFixture("waiting", true), now, 0)
	seedCardState(t, db, room.Key, "om_pending", card, now, 0)
	seedCardState(t, db, "revoked-room", "om_revoked", card, now, 0)
	seedCardState(t, db, room.Key, "om_done", replyFixture("done", true), now, now)
	if _, err := db.Exec(`INSERT INTO outbox(id,state,target) VALUES('final','pending','om_pending')`); err != nil {
		t.Fatal(err)
	}
	recovered, err := c.RecoverCards(context.Background())
	if err != nil || recovered != 2 || len(writes) != 2 {
		t.Fatalf("recovery: count=%d err=%v writes=%+v", recovered, err, writes)
	}
	for _, write := range writes {
		if write.method != http.MethodPut || !strings.HasPrefix(write.path, "/open-apis/cardkit/v1/cards/card_om_") || strings.Contains(write.path, "/elements/") {
			t.Fatalf("recovery created/replaced a card: %+v", write)
		}
		value, _ := object(write.body["card"])
		var restored core.Args
		if err = json.Unmarshal([]byte(stringValue(value, "data")), &restored); err != nil {
			t.Fatal(err)
		}
		config, _ := object(restored["config"])
		header, _ := object(restored["header"])
		if config["streaming_mode"] != false || header["template"] != "orange" || !strings.Contains(cardTexts(restored)["reply_status"], "尚未确认完成") {
			t.Fatalf("recovery gave misleading completion state: %+v", restored)
		}
	}
	if again, err := c.RecoverCards(context.Background()); err != nil || again != 0 || len(writes) != 2 {
		t.Fatalf("recovery replayed completed repair: %d %v", again, err)
	}
}

func TestCrashBetweenMessageAcknowledgementAndCardMappingRecoversWithoutPost(t *testing.T) {
	db := fixtureDB(t)
	creates, sends, updates := 0, 0, 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "cardkit") {
			creates++
			apiResponse(w, core.Args{"card_id": "card_acknowledged"})
			return
		}
		if r.Method == http.MethodPost {
			sends++
			apiResponse(w, core.Args{"message_id": "om_acknowledged"})
			return
		}
		updates++
		if r.Method != http.MethodPut || r.URL.Path != "/open-apis/cardkit/v1/cards/card_acknowledged" {
			t.Errorf("recovery changed target: %s %s", r.Method, r.URL.Path)
		}
		apiResponse(w, core.Args{})
	}
	c, _ := fixtureClient(t, db, handler)
	ctx := core.WithDeliveryID(context.Background(), "preview:ack-before-mapping")
	if _, err := c.Card(ctx, fixtureRoom(t, c), replyFixture("saved partial", false), "", ""); err != nil {
		t.Fatal(err)
	}
	// Keep the two durable acknowledgements, but emulate a crash before Card's
	// mapping/lifecycle INSERT and before linkDeliveryMessage could run.
	if _, err := db.Exec(`DELETE FROM feishu_cards; DELETE FROM feishu_card_lifecycle; UPDATE feishu_deliveries SET message='' WHERE step='card-create'`); err != nil {
		t.Fatal(err)
	}
	c2, _ := fixtureClient(t, db, handler)
	recovered, err := c2.RecoverCards(context.Background())
	if err != nil || recovered != 1 || creates != 1 || sends != 1 || updates != 1 {
		t.Fatalf("confirmed card was lost or recreated: count=%d err=%v creates=%d sends=%d updates=%d", recovered, err, creates, sends, updates)
	}
	var card, snapshot string
	var sequence int64
	if err = db.QueryRow(`SELECT card,sequence,snapshot FROM feishu_cards WHERE message='om_acknowledged'`).Scan(&card, &sequence, &snapshot); err != nil || card != "card_acknowledged" || sequence != 1 || !strings.Contains(snapshot, "尚未确认完成") {
		t.Fatalf("mapping recovery failed: %s %d %v", card, sequence, err)
	}
}

func TestRetainedCardCannotBeResurrectedFromCreationJournal(t *testing.T) {
	db := fixtureDB(t)
	writes := 0
	c, _ := fixtureClient(t, db, func(w http.ResponseWriter, r *http.Request) {
		writes++
		if strings.Contains(r.URL.Path, "cardkit") {
			apiResponse(w, core.Args{"card_id": "card_terminal"})
			return
		}
		apiResponse(w, core.Args{"message_id": "om_terminal"})
	})
	ctx := core.WithTerminalCard(core.WithDeliveryID(context.Background(), "output:terminal"))
	if _, err := c.Card(ctx, fixtureRoom(t, c), replyFixture("completed answer", true), "", ""); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour).UnixMilli()
	if _, err := db.Exec(`UPDATE feishu_card_lifecycle SET updated_at=?,terminal_at=?`, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE feishu_deliveries SET updated_at=?`, old); err != nil {
		t.Fatal(err)
	}
	if count, err := c.Retain(context.Background(), time.Now().Add(-30*24*time.Hour), nil); err != nil || count != 1 {
		t.Fatalf("terminal prune: %d %v", count, err)
	}
	if count, err := c.RecoverCards(context.Background()); err != nil || count != 0 || writes != 2 {
		t.Fatalf("intentional prune was undone: recovered=%d writes=%d err=%v", count, writes, err)
	}
}
