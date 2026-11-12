package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func mentionFixture(t *testing.T, sender string) (*fixture, *fakeWorker, *slot) {
	t.Helper()
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].AllowOtherBots = true
		c.Groups[0].OtherBots = []core.OtherBot{{AppID: "cli_helper", Name: "Helper", OpenID: "ou_helper"}}
		c.Limits.BackgroundMessages = 1
	})
	f.send(f.message("human", "ask Helper", sender))
	w := f.worker()
	_ = f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	if s == nil {
		t.Fatal("missing slot")
	}
	return f, w, s
}

func TestMentionBotMaintainerSendsWhitelistedMention(t *testing.T) {
	f, _, s := mentionFixture(t, "owner")
	result, err := f.g.request(s, core.WorkerEvent{Kind: "mention_bot", Input: core.Args{"target": "Helper", "body": "run this"}})
	if err != nil || !boolArg(result, "ok") {
		t.Fatalf("mention = %+v %v", result, err)
	}
	f.tr.mu.Lock()
	posts := append([]sentCard(nil), f.tr.posts...)
	f.tr.mu.Unlock()
	if len(posts) != 1 || !strings.Contains(jsonText(posts[0].card), "ou_helper") || !strings.Contains(jsonText(posts[0].card), "run this") {
		t.Fatalf("unexpected posts: %s", jsonText(posts))
	}
	var kind, target, reply string
	if err = f.s.DB.QueryRow("SELECT kind,target,reply_to FROM outbox WHERE kind='mention'").Scan(&kind, &target, &reply); err != nil || kind != "mention" || target != posts[0].id || reply != "human" {
		t.Fatalf("mention outbox = %q %q %q %v", kind, target, reply, err)
	}
}

func TestMentionBotRejectsNonMaintainer(t *testing.T) {
	f, _, s := mentionFixture(t, "member")
	if _, err := f.g.request(s, core.WorkerEvent{Kind: "mention_bot", Input: core.Args{"target": "Helper"}}); err == nil {
		t.Fatal("non-maintainer mention was accepted")
	}
	f.tr.mu.Lock()
	posts := len(f.tr.posts)
	f.tr.mu.Unlock()
	if posts != 0 {
		t.Fatalf("non-maintainer sent %d posts", posts)
	}
}

func TestMentionBotRejectsUnknownTargetAndSecondMention(t *testing.T) {
	f, _, s := mentionFixture(t, "owner")
	if _, err := f.g.request(s, core.WorkerEvent{Kind: "mention_bot", Input: core.Args{"target": "Unknown"}}); err == nil {
		t.Fatal("unknown target was accepted")
	}
	if _, err := f.g.request(s, core.WorkerEvent{Kind: "mention_bot", Input: core.Args{"target": "cli_helper"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.request(s, core.WorkerEvent{Kind: "mention_bot", Input: core.Args{"target": "Helper"}}); err == nil {
		t.Fatal("second mention in one turn was accepted")
	}
	f.tr.mu.Lock()
	posts := len(f.tr.posts)
	f.tr.mu.Unlock()
	if posts != 1 {
		t.Fatalf("posts = %d", posts)
	}
}

func TestMentionBotReplyDoesNotTrigger(t *testing.T) {
	f, _, s := mentionFixture(t, "owner")
	if _, err := f.g.request(s, core.WorkerEvent{Kind: "mention_bot", Input: core.Args{"target": "Helper", "body": "check"}}); err != nil {
		t.Fatal(err)
	}
	f.tr.mu.Lock()
	post := f.tr.posts[0].id
	f.tr.mu.Unlock()
	reply := core.Message{ID: "bot-reply", ChatID: "group", ChatType: "group", SenderID: "cli_other", SenderIsBot: true, Text: "result", Time: time.Now().UnixMilli(), Mentioned: true, ReplyTo: post}
	f.send(reply)
	eventually(t, func() bool {
		var status string
		return f.s.DB.QueryRow("SELECT status FROM inbox WHERE id=?", reply.ID).Scan(&status) == nil && status == "background"
	})
}

func TestMentionAdminsRequiresAllActiveSenders(t *testing.T) {
	room := core.Room{Maintainers: []string{"owner"}}
	if !mentionAdmins(room, &core.RequestContext{Senders: []string{"owner"}}) {
		t.Fatal("owner set was rejected")
	}
	if mentionAdmins(room, &core.RequestContext{Senders: []string{"owner", "member"}}) {
		t.Fatal("mixed sender set was accepted")
	}
	if mentionAdmins(room, nil) {
		t.Fatal("inactive context was accepted")
	}
}

func TestMentionBotDisabledWithoutAllowOtherBots(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].OtherBots = []core.OtherBot{{AppID: "cli_helper", Name: "Helper", OpenID: "ou_helper"}}
	})
	f.send(f.message("human", "ask Helper", "owner"))
	w := f.worker()
	_ = f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	if _, err := f.g.request(s, core.WorkerEvent{Kind: "mention_bot", Input: core.Args{"target": "Helper"}}); err == nil {
		t.Fatal("mention_bot was available without allowOtherBots")
	}
}

func TestMentionBotRequiresRequestToNameTarget(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].AllowOtherBots = true
		c.Groups[0].OtherBots = []core.OtherBot{{AppID: "cli_helper", Name: "Helper", OpenID: "ou_helper"}}
	})
	f.send(f.message("human", "hello", "owner"))
	w := f.worker()
	_ = f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	if _, err := f.g.request(s, core.WorkerEvent{Kind: "mention_bot", Input: core.Args{"target": "Helper"}}); err == nil {
		t.Fatal("mention_bot accepted a request that did not name the target")
	}
	f.tr.mu.Lock()
	posts := len(f.tr.posts)
	f.tr.mu.Unlock()
	if posts != 0 {
		t.Fatalf("posts = %d", posts)
	}
}
