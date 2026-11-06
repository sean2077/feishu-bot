package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

func verificationConfig(t *testing.T) core.Config {
	t.Helper()
	c := config.Defaults()
	c.DataDir = filepath.Join(t.TempDir(), "state")
	c.TenantID, c.AppID = "tenant_fixture", "cli_fixture"
	c.Projects = []core.Project{{ID: "test", Maintainers: []string{"owner"}}}
	c.Groups = []core.Group{{ChatID: "oc_test", ProjectID: "test", Members: []string{"member"}}}
	c.DMs = []core.DM{{UserID: "dm_user", ProjectID: "test"}}
	return c
}
func TestVerificationRejectsUnknownScopeBeforeCreatingState(t *testing.T) {
	c := verificationConfig(t)
	r := VerifyConfiguredChat(context.Background(), c, "oc_not_allowed", io.Discard)
	if r.Status != "blocked" {
		t.Fatal(r)
	}
	if _, err := os.Stat(c.DataDir); !os.IsNotExist(err) {
		t.Fatal("invalid target created state", err)
	}
	for _, args := range [][]string{{"doctor", "--verify-chat"}, {"doctor", "--verify-chat", "oc_test", "--verify-chat=oc_test"}, {"run", "--verify-chat", "oc_test"}} {
		if _, err := Parse(args); err == nil {
			t.Fatal("accepted ambiguous verification option", args)
		}
	}
	o, err := Parse([]string{"doctor", "--verify-chat=dm:dm_user"})
	if err != nil || o.VerifyChat != "dm:dm_user" {
		t.Fatal(o, err)
	}
}
func TestVerificationAcceptsDefaultGroupBinding(t *testing.T) {
	c := verificationConfig(t)
	if configuredVerificationTarget(c, "oc_not_allowed") {
		t.Fatal("unlisted group accepted without a default binding")
	}
	c.Groups = append(c.Groups, core.Group{ChatID: core.AnyGroupChatID, ProjectID: "test"})
	if !configuredVerificationTarget(c, "oc_new_group") {
		t.Fatal("default group binding was not accepted")
	}
	if configuredVerificationTarget(c, core.AnyGroupChatID) || configuredVerificationTarget(c, "not-a-chat") {
		t.Fatal("invalid verification target accepted")
	}
}
func TestVerificationInputKeepsGroupAndDMBoundaries(t *testing.T) {
	c := verificationConfig(t)
	m := core.Message{ID: "om_test", ChatID: "oc_test", ChatType: "group", SenderID: "member", Text: "/verify nonce", Mentioned: true}
	if _, ok := verificationMessage(c, "oc_test", "nonce", m); !ok {
		t.Fatal("valid test trigger rejected")
	}
	m.Mentioned = false
	if _, ok := verificationMessage(c, "oc_test", "nonce", m); ok {
		t.Fatal("missing mention accepted")
	}
	m.Mentioned = true
	m.SenderID = "stranger"
	if _, ok := verificationMessage(c, "oc_test", "nonce", m); ok {
		t.Fatal("unauthorized author accepted")
	}
	m.SenderID = "dm_user"
	m.ChatType = "p2p"
	m.ChatID = "oc_dm"
	if room, ok := verificationMessage(c, "dm:dm_user", "nonce", m); !ok || room.ChatID != "oc_dm" {
		t.Fatal("DM did not bind observed chat")
	}
	if _, ok := verificationMessage(c, "oc_test", "nonce", m); ok {
		t.Fatal("DM became configured group")
	}
}

type verifierFixture struct {
	t                     *testing.T
	fileSent              chan struct{}
	closed                chan struct{}
	once                  sync.Once
	fileOnce              sync.Once
	cards, files, updates int
}

func (f *verifierFixture) Start(ctx context.Context, onMessage func(context.Context, core.Message) error, onAction func(context.Context, core.CardAction) (core.Args, error)) error {
	_ = onMessage(ctx, core.Message{ID: "om_input", ChatID: "oc_test", ChatType: "group", SenderID: "member", Mentioned: true, Text: "/verify nonce"})
	select {
	case <-f.fileSent:
	case <-ctx.Done():
		return nil
	}
	good := core.CardAction{ChatID: "oc_test", MessageID: "om_card", UserID: "member", Value: core.Args{"diagnostic": "first-connection", "nonce": "nonce", "choice": "card"}}
	for _, change := range []func(*core.CardAction){func(a *core.CardAction) { a.ChatID = "oc_other" }, func(a *core.CardAction) { a.MessageID = "om_old" }, func(a *core.CardAction) { a.UserID = "stranger" }} {
		a := good
		change(&a)
		_, _ = onAction(ctx, a)
	}
	_, _ = onAction(ctx, good)
	good.Value = core.Args{"diagnostic": "first-connection", "nonce": "nonce", "choice": "file"}
	_, _ = onAction(ctx, good)
	<-ctx.Done()
	return nil
}
func (f *verifierFixture) Close() error { f.once.Do(func() { close(f.closed) }); return nil }
func (f *verifierFixture) Card(context.Context, core.Room, core.Args, string, string) (string, error) {
	f.cards++
	return "om_card", nil
}
func (f *verifierFixture) Update(context.Context, string, core.Args) error {
	f.updates++
	f.fileOnce.Do(func() { close(f.fileSent) })
	return nil
}
func (f *verifierFixture) File(_ context.Context, r core.Room, path, _ string) (string, error) {
	f.files++
	if r.ChatID != "oc_test" {
		f.t.Error("file escaped target chat")
	}
	if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
		f.t.Error("missing real test file", err)
	}
	return "om_file", nil
}
func TestVerificationCardButtonsUseCallbackBehaviors(t *testing.T) {
	card := verificationCard("nonce", "status", true)
	buttons := 0
	var walk func(any)
	walk = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			if item["tag"] == "button" {
				buttons++
				if _, exists := item["value"]; exists {
					t.Fatalf("card 2.0 button uses a top-level value: %#v", item)
				}
				behaviors, ok := item["behaviors"].([]any)
				if !ok || len(behaviors) != 1 {
					t.Fatalf("button is missing a callback behavior: %#v", item)
				}
				behavior, _ := behaviors[0].(map[string]any)
				callback, _ := behavior["value"].(map[string]any)
				if behavior["type"] != "callback" || callback["nonce"] != "nonce" {
					t.Fatalf("button callback behavior is invalid: %#v", item)
				}
			}
			for _, child := range item {
				walk(child)
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(card)
	if buttons != 2 {
		t.Fatalf("verification card buttons=%d", buttons)
	}
}

func TestExplicitChatVerificationConfirmsScopedCardAndFile(t *testing.T) {
	c := verificationConfig(t)
	f := &verifierFixture{t: t, fileSent: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := verifyChatSession(ctx, c, "oc_test", "nonce", t.TempDir(), io.Discard, f)
	if r.Status != "passed" || r.Checks["incomingMessage"] != "passed" || r.Checks["cardCallbacks"] != "passed" || r.Checks["fileDownload"] != "confirmed_by_user" {
		t.Fatal(r)
	}
	if f.cards != 1 || f.files != 1 || f.updates != 2 {
		t.Fatal("unexpected delivery/replay count", f.cards, f.files, f.updates)
	}
	if r.Checks["feishuReconnect"] != "not_checked" {
		t.Fatal("overclaimed reconnect coverage")
	}
	select {
	case <-f.closed:
	default:
		t.Fatal("verification transport retained")
	}
}
