package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func testQuickValue(t *testing.T, g *Gateway, card core.Args, action string) core.Args {
	t.Helper()
	var result core.Args
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if token, ok := x["quick_control"].(string); ok {
				p, valid := g.readQuick(token)
				if valid && p.Action == action {
					result = core.Args{"quick_control": token}
				}
			}
			if controls := argsOf(x["quick_controls"]); controls != nil {
				for _, raw := range controls {
					token, ok := raw.(string)
					if !ok {
						continue
					}
					p, valid := g.readQuick(token)
					if valid && p.Action == action {
						result = core.Args{"quick_control": token}
					}
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(card)
	if result == nil {
		t.Fatal("missing quick control", action)
	}
	return result
}

func TestQuickControlsDeterministicAndRevisionBound(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, _ := f.s.Session(r.Key)
	controls := f.g.quickControls(r, s.ID, "turn-one", "")
	first, _ := json.Marshal(controls)
	second, _ := json.Marshal(f.g.quickControls(r, s.ID, "turn-one", ""))
	if string(first) != string(second) {
		t.Fatal("streaming quick controls are not deterministic")
	}
	card := DisplayCard("fixture", "reply", controls...)
	if err := f.g.bindQuickControls("bound-card", card); err != nil {
		t.Fatal(err)
	}
	a := core.CardAction{ChatID: r.ChatID, UserID: "member", MessageID: "bound-card", Value: testQuickValue(t, f.g, card, "status")}
	p, _ := f.g.readQuick(stringArg(a.Value, "quick_control"))
	if !f.g.quickBound(context.Background(), a, p) {
		t.Fatal("successfully delivered card was not bound")
	}
	updated := replaceQuickControls(card, f.g.quickControls(r, s.ID, "turn-two", ""))
	if err := f.g.bindQuickControls("bound-card", updated); err != nil {
		t.Fatal(err)
	}
	if f.g.quickBound(context.Background(), a, p) {
		t.Fatal("old card revision remained active")
	}
	for _, attack := range []core.CardAction{
		{ChatID: "other", UserID: "member", MessageID: "bound-card", Value: testQuickValue(t, f.g, updated, "status")},
		{ChatID: r.ChatID, UserID: "stranger", MessageID: "bound-card", Value: testQuickValue(t, f.g, updated, "status")},
		{ChatID: r.ChatID, UserID: "member", MessageID: "unbound-card", Value: testQuickValue(t, f.g, updated, "status")},
		{ChatID: r.ChatID, UserID: "member", MessageID: "bound-card", Value: core.Args{"quick_control": stringArg(a.Value, "quick_control") + ".tampered"}},
	} {
		if _, err := f.g.Action(context.Background(), attack); err != nil {
			t.Fatal(err)
		}
	}
	f.tr.mu.Lock()
	notices := len(f.tr.notices)
	f.tr.mu.Unlock()
	if notices != 0 || f.starts.Load() != 0 {
		t.Fatal("unauthorized callback had side effects")
	}
	var original core.Args
	_ = json.Unmarshal([]byte(jsonText(card)), &original)
	if stringArg(testQuickValue(t, f.g, original, "status"), "quick_control") != stringArg(a.Value, "quick_control") {
		t.Fatal("control refresh mutated frozen create payload")
	}
}

func TestQuickSupplementDeduplicatesAndOldStopCannotStopLaterTurn(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("original", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	r := f.room()
	card := DisplayCard("fixture", "reply", f.g.quickControls(r, d.SessionID, d.UUID, "")...)
	if err := f.g.bindQuickControls("bound-card", card); err != nil {
		t.Fatal(err)
	}
	a := core.CardAction{ChatID: r.ChatID, UserID: "second", MessageID: "bound-card", Value: testQuickValue(t, f.g, card, "supplement"), FormValue: core.Args{"quick_supplement": "/new"}}
	reply, err := f.g.Action(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reply, "不代表模型已读取") || !strings.Contains(reply, "补充已保存") {
		t.Fatalf("supplement toast = %q", reply)
	}
	extra := f.delivery(w)
	if extra.SupplementTo != d.UUID || extra.SessionID != d.SessionID || extra.Message.Text != "/new" || extra.Message.InteractionMessageID != "bound-card" {
		t.Fatal("form supplement changed command / turn semantics", extra)
	}
	if _, err := f.g.Action(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM inbox WHERE room=? AND id=?", r.Key, extra.Message.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate callback created duplicate input", count, err)
	}
	if err := f.s.DB.QueryRow("SELECT count(*) FROM receipts WHERE id=?", extra.Message.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("synthetic form input created nonexistent Feishu reaction", count, err)
	}
	select {
	case duplicate := <-w.sent:
		t.Fatal("duplicate form input reached native stream", duplicate)
	default:
	}
	stop := a
	stop.Value = testQuickValue(t, f.g, card, "stop")
	if text, err := f.g.Action(context.Background(), stop); err != nil || !strings.Contains(text, "提问人或管理员") {
		t.Fatal("ordinary group member could stop shared task", text, err)
	}
	w.emit(core.WorkerEvent{Type: "result", UUID: d.UUID, UUIDs: []string{d.UUID, extra.UUID}, ResultID: core.NewID(), Text: "done", Success: true, Source: "foreground"})
	f.send(f.message("later", "next task", "member"))
	next := f.delivery(w)
	if next.Message.ID != "later" {
		t.Fatal("unexpected input", next)
	}
	stop.UserID = "owner"
	if text, err := f.g.Action(context.Background(), stop); err != nil || !strings.Contains(text, "已经结束") {
		t.Fatal("old stop button did not reject later turn", text, err)
	}
	if w.stops.Load() != 0 {
		t.Fatal("old button stopped a later foreground turn")
	}
}

func TestCancelledQuickSupplementRepliesToRealCardMessage(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	d := core.Delivery{
		UUID:         core.NewID(),
		RoomKey:      r.Key,
		SessionID:    s.ID,
		SupplementTo: core.NewID(),
		Message: core.Message{
			ID:                   "card:" + core.NewID(),
			InteractionMessageID: "om_real_card",
			ChatID:               r.ChatID,
			ChatType:             "group",
			SenderID:             "member",
			Mentioned:            true,
			Text:                 "supplement",
		},
	}
	if err := f.g.queueInput(context.Background(), d, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if !f.g.cancelSupplement(r, d, "测试", false, "queued") {
		t.Fatal("supplement was not cancelled")
	}
	var reply string
	if err := f.s.DB.QueryRow("SELECT reply_to FROM outbox WHERE room=? AND kind='recovery'", r.Key).Scan(&reply); err != nil {
		t.Fatal(err)
	}
	if reply != d.Message.InteractionMessageID {
		t.Fatalf("cancellation notice replies to %q, want the delivered card %q", reply, d.Message.InteractionMessageID)
	}
}

func TestQuickStopIsAuthorizedOnceAndPreservesSession(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("original", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	r := f.room()
	before, _ := f.s.Session(r.Key)
	card := DisplayCard("fixture", "reply", f.g.quickControls(r, d.SessionID, d.UUID, "")...)
	if err := f.g.bindQuickControls("bound-card", card); err != nil {
		t.Fatal(err)
	}
	a := core.CardAction{ChatID: r.ChatID, UserID: "owner", MessageID: "bound-card", Value: testQuickValue(t, f.g, card, "stop")}
	if _, err := f.g.Action(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.Action(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return f.g.getSlot(r.Key) == nil })
	after, _ := f.s.Session(r.Key)
	if w.stops.Load() != 1 || before.ID != after.ID || before.CCID != after.CCID || f.starts.Load() != 1 {
		t.Fatal("duplicate quick stop changed session or started native generation", before, after, w.stops.Load())
	}
}

func TestQuickStopPersistsPauseBeforeFirstCancellation(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			f := newFixture(t, nil)
			f.send(f.message("original", "work", "member"))
			w := f.worker()
			d := f.delivery(w)
			r := f.room()
			card := DisplayCard("fixture", "reply", f.g.quickControls(r, d.SessionID, d.UUID, "")...)
			if err := f.g.bindQuickControls("bound-card", card); err != nil {
				t.Fatal(err)
			}
			if fallback {
				if _, err := f.s.DB.Exec(`CREATE TRIGGER fail_quick_stop_intent BEFORE INSERT ON room_stop_pauses BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
			}
			s := f.g.getSlot(r.Key)
			observed := make(chan error, 1)
			var once sync.Once
			s.mu.Lock()
			cancelSlot := s.cancel
			s.cancel = func() {
				once.Do(func() {
					var err error
					if fallback {
						_, err = os.Stat(f.g.stopPausePath(r.Key))
					} else {
						var count int
						err = f.s.DB.QueryRow("SELECT count(*) FROM room_stop_pauses WHERE room=? AND session=?", r.Key, d.SessionID).Scan(&count)
						if err == nil && count != 1 {
							err = fmt.Errorf("durable stop pauses at cancellation = %d, want 1", count)
						}
					}
					observed <- err
				})
				cancelSlot()
			}
			s.mu.Unlock()
			a := core.CardAction{ChatID: r.ChatID, UserID: "owner", MessageID: "bound-card", Value: testQuickValue(t, f.g, card, "stop")}
			if _, err := f.g.Action(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-observed:
				if err != nil {
					t.Fatalf("native cancellation preceded durable stop intent: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("quick stop never cancelled the native generation")
			}
			eventually(t, func() bool { return f.g.getSlot(r.Key) == nil && !f.g.stopIsBlocked(r.Key) })
			if f.status("original") != "cancelled" || w.stops.Load() != 1 {
				t.Fatal("durable pause prevented stop completion")
			}
		})
	}
}

func TestQuickControlsUseCompactLayoutAndTwoLineSupplement(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, _ := f.s.Session(r.Key)
	controls := f.g.quickControls(r, s.ID, "turn-one", "")
	if len(controls) != 4 {
		t.Fatalf("quick controls have %d elements, want divider/row/overflow/form", len(controls))
	}
	var checkIDs func(any)
	checkIDs = func(v any) {
		switch item := v.(type) {
		case []any:
			for _, child := range item {
				checkIDs(child)
			}
		case map[string]any:
			if id, ok := item["element_id"].(string); ok {
				if len(id) > 20 || id[0] < 'A' || (id[0] > 'Z' && id[0] < 'a') || id[0] > 'z' {
					t.Errorf("element_id %q violates the Card JSON 2.0 limit", id)
				}
			}
			for _, child := range item {
				checkIDs(child)
			}
		}
	}
	checkIDs(controls)
	divider := argsOf(controls[0])
	if stringArg(divider, "tag") != "hr" || stringArg(divider, "element_id") != "gateway_quick_div" {
		t.Fatalf("first control is not the divider: %#v", divider)
	}
	row := argsOf(controls[1])
	if stringArg(row, "flex_mode") != "none" {
		t.Fatalf("button row flex_mode = %q, want none", stringArg(row, "flex_mode"))
	}
	columns, _ := row["columns"].([]any)
	if len(columns) != 1 {
		t.Fatalf("button row has %d columns, want status only", len(columns))
	}
	labels := []string{}
	for _, raw := range columns {
		column := argsOf(raw)
		if stringArg(column, "width") != "auto" {
			t.Fatalf("button column width = %q, want auto", stringArg(column, "width"))
		}
		children, _ := column["elements"].([]any)
		if len(children) != 1 {
			t.Fatalf("button column has %d elements", len(children))
		}
		b := argsOf(children[0])
		if _, exists := b["width"]; exists {
			t.Fatalf("button should keep its natural width: %#v", b)
		}
		labels = append(labels, stringArg(argsOf(b["text"]), "content"))
	}
	if strings.Join(labels, ",") != "查看状态" {
		t.Fatalf("button labels = %v", labels)
	}
	overflow := argsOf(controls[2])
	if stringArg(overflow, "tag") != "overflow" || stringArg(overflow, "element_id") != "gateway_quick_more" {
		t.Fatalf("overflow control = %#v", overflow)
	}
	options, _ := overflow["options"].([]any)
	var optionLabels []string
	for _, raw := range options {
		optionLabels = append(optionLabels, stringArg(argsOf(argsOf(raw)["text"]), "content"))
	}
	if strings.Join(optionLabels, ",") != "待回答,停止本轮" {
		t.Fatalf("overflow options = %v", optionLabels)
	}

	form := argsOf(controls[3])
	formElements, _ := form["elements"].([]any)
	if len(formElements) != 1 {
		t.Fatalf("form has %d elements, want one column_set", len(formElements))
	}
	layout := argsOf(formElements[0])
	if stringArg(layout, "tag") != "column_set" {
		t.Fatalf("form child = %q, want column_set", stringArg(layout, "tag"))
	}
	layoutColumns, _ := layout["columns"].([]any)
	if len(layoutColumns) != 2 {
		t.Fatalf("form layout has %d columns, want 2", len(layoutColumns))
	}
	input := argsOf(argsOf(layoutColumns[0])["elements"].([]any)[0])
	if rows, ok := numberArg(input, "rows"); !ok || rows != 2 {
		t.Fatalf("supplement rows = %v, want 2", input["rows"])
	}
	if stringArg(input, "input_type") != "multiline_text" || stringArg(input, "width") != "fill" {
		t.Fatalf("supplement input = %#v", input)
	}
	submit := argsOf(argsOf(layoutColumns[1])["elements"].([]any)[0])
	if stringArg(submit, "type") != "primary_filled" || stringArg(argsOf(submit["text"]), "content") != "发送" {
		t.Fatalf("submit button = %#v", submit)
	}
	if stringArg(submit, "name") != "quick_supplement_submit" || stringArg(submit, "form_action_type") != "submit" {
		t.Fatalf("submit button lost its form binding: %#v", submit)
	}
	if behaviors, ok := submit["behaviors"].([]any); !ok || len(behaviors) != 1 {
		t.Fatalf("submit button lost its callback behavior: %#v", submit)
	}

	replaced := replaceQuickControls(DisplayCard("fixture", "reply", controls...), f.g.quickControls(r, s.ID, "turn-two", ""))
	var walk func(any)
	countDividers := func(v any) int {
		count := 0
		walk = func(value any) {
			switch item := value.(type) {
			case []any:
				for _, child := range item {
					walk(child)
				}
			case map[string]any:
				if item["element_id"] == "gateway_quick_div" {
					count++
				}
				for _, child := range item {
					walk(child)
				}
			}
		}
		walk(v)
		return count
	}
	if got := countDividers(replaced); got != 1 {
		t.Fatalf("replaced card has %d dividers, want 1", got)
	}
	empty := replaceQuickControls(DisplayCard("fixture", "reply", controls...), nil)
	if got := countDividers(empty); got != 0 {
		t.Fatalf("card without controls has %d dividers", got)
	}
	if jsonText(empty) != jsonText(DisplayCard("fixture", "reply")) {
		t.Fatalf("remaining quick controls were not removed: %s", jsonText(empty))
	}
}

func TestQuickStopAllowsRequester(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("original", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	r := f.room()
	card := DisplayCard("fixture", "reply", f.g.quickTurnControls(r, d.SessionID, d.UUID, "", "member")...)
	if err := f.g.bindQuickControls("bound-card", card); err != nil {
		t.Fatal(err)
	}
	a := core.CardAction{ChatID: r.ChatID, UserID: "member", MessageID: "bound-card", Value: testQuickValue(t, f.g, card, "stop")}
	if text, err := f.g.Action(context.Background(), a); err != nil || strings.Contains(text, "提问人或管理员") {
		t.Fatalf("requester could not stop own turn: %q %v", text, err)
	}
	eventually(t, func() bool { return f.g.getSlot(r.Key) == nil })
	if w.stops.Load() != 1 {
		t.Fatalf("stops = %d", w.stops.Load())
	}
}

func TestQuickStopRejectsOtherMemberAndAllowsAdmin(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("original", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	r := f.room()
	card := DisplayCard("fixture", "reply", f.g.quickTurnControls(r, d.SessionID, d.UUID, "", "member")...)
	if err := f.g.bindQuickControls("bound-card", card); err != nil {
		t.Fatal(err)
	}
	value := testQuickValue(t, f.g, card, "stop")
	other := core.CardAction{ChatID: r.ChatID, UserID: "second", MessageID: "bound-card", Value: value}
	if text, err := f.g.Action(context.Background(), other); err != nil || !strings.Contains(text, "只有提问人或管理员") {
		t.Fatalf("other member stop = %q %v", text, err)
	}
	if w.stops.Load() != 0 || f.g.getSlot(r.Key) == nil {
		t.Fatal("other member stopped the turn")
	}
	admin := other
	admin.UserID = "owner"
	if text, err := f.g.Action(context.Background(), admin); err != nil || strings.Contains(text, "只有提问人或管理员") {
		t.Fatalf("admin stop = %q %v", text, err)
	}
	eventually(t, func() bool { return f.g.getSlot(r.Key) == nil })
	if w.stops.Load() != 1 {
		t.Fatalf("stops = %d", w.stops.Load())
	}
}

func TestQuickStopWithoutRequesterRequiresAdmin(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("original", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	r := f.room()
	card := DisplayCard("fixture", "reply", f.g.quickControls(r, d.SessionID, d.UUID, "")...)
	if err := f.g.bindQuickControls("bound-card", card); err != nil {
		t.Fatal(err)
	}
	value := testQuickValue(t, f.g, card, "stop")
	action := core.CardAction{ChatID: r.ChatID, UserID: "member", MessageID: "bound-card", Value: value}
	if text, err := f.g.Action(context.Background(), action); err != nil || !strings.Contains(text, "只有提问人或管理员") {
		t.Fatalf("member stop without requester = %q %v", text, err)
	}
	action.UserID = "owner"
	if text, err := f.g.Action(context.Background(), action); err != nil || strings.Contains(text, "只有提问人或管理员") {
		t.Fatalf("admin stop without requester = %q %v", text, err)
	}
	eventually(t, func() bool { return f.g.getSlot(r.Key) == nil })
	if w.stops.Load() != 1 {
		t.Fatalf("stops = %d", w.stops.Load())
	}
}

func TestQuickStopRejectsTamperedRequester(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("original", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	r := f.room()
	card := DisplayCard("fixture", "reply", f.g.quickTurnControls(r, d.SessionID, d.UUID, "", "member")...)
	value := testQuickValue(t, f.g, card, "stop")
	token := stringArg(value, "quick_control")
	payload, signature, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatal("malformed test token")
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var p quickControl
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	p.Requester = "second"
	data, _ = json.Marshal(p)
	tampered := base64.RawURLEncoding.EncodeToString(data) + "." + signature
	a := core.CardAction{ChatID: r.ChatID, UserID: "second", MessageID: "bound-card", Value: core.Args{"quick_control": tampered}}
	if text, err := f.g.Action(context.Background(), a); err != nil || !strings.Contains(text, "已失效") {
		t.Fatalf("tampered stop = %q %v", text, err)
	}
	if w.stops.Load() != 0 {
		t.Fatal("tampered control stopped the turn")
	}
}

func TestStopControlRequesterComesFromAnchor(t *testing.T) {
	f := newFixture(t, nil)
	f.send(f.message("original", "work", "member"))
	w := f.worker()
	d := f.delivery(w)
	r := f.room()
	s := f.g.getSlot(r.Key)
	render := newRender(f.g, s, &d)
	render.mu.Lock()
	card := render.cardLocked("处理中", "", false, false)
	render.mu.Unlock()
	value := testQuickValue(t, f.g, card, "stop")
	p, valid := f.g.readQuick(stringArg(value, "quick_control"))
	if !valid || p.Requester != "member" || p.Turn != d.UUID {
		t.Fatalf("stop control provenance = %+v %t", p, valid)
	}
}

func TestFinalReplyCopySendsStoredText(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.g.saveOutput(f.s.DB, r, s.ID, "full answer text", true, "copy-card", "", "", DisplayCard("回复", "full answer text"), nil, "", "", "reply", "done")
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.g.output(id)
	if err != nil {
		t.Fatal(err)
	}
	card := f.g.deliveredOutputCard(o, r)
	if err := f.g.bindQuickControls("copy-card", card); err != nil {
		t.Fatal(err)
	}
	value := testQuickValue(t, f.g, card, "copy")
	action := core.CardAction{ChatID: r.ChatID, UserID: "member", MessageID: "copy-card", Value: value}
	if text, err := f.g.Action(context.Background(), action); err != nil || !strings.Contains(text, "复制") {
		t.Fatalf("copy toast = %q %v", text, err)
	}
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, notice := range f.tr.notices {
			if notice == "full answer text" {
				return true
			}
		}
		return false
	})
}

func TestOverflowOptionSelectsSignedAction(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, _ := f.s.Session(r.Key)
	controls := f.g.quickControls(r, s.ID, "turn-one", "")
	card := DisplayCard("fixture", "reply", controls...)
	if err := f.g.bindQuickControls("overflow-card", card); err != nil {
		t.Fatal(err)
	}
	overflow := argsOf(controls[2])
	behaviors, _ := overflow["behaviors"].([]any)
	value := argsOf(argsOf(behaviors[0])["value"])
	action := core.CardAction{ChatID: r.ChatID, UserID: "member", MessageID: "overflow-card", Option: "answer", Value: value}
	if text, err := f.g.Action(context.Background(), action); err != nil || !strings.Contains(text, "待回答") {
		t.Fatalf("overflow answer = %q %v", text, err)
	}
}

func TestCopyCommandSendsLatestReply(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	s, err := f.s.Session(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.g.saveOutput(f.s.DB, r, s.ID, "latest answer", true, "done-card", "", "", DisplayCard("回复", "latest answer"), nil, "", "", "reply", "done"); err != nil {
		t.Fatal(err)
	}
	f.send(f.message("copy-command", "/copy", "owner"))
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, notice := range f.tr.notices {
			if notice == "latest answer" {
				return true
			}
		}
		return false
	})
}
