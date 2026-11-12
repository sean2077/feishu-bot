package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func otherBotConfig(allow bool) core.Config {
	return core.Config{
		TenantID: "tenant",
		AppID:    "app",
		Projects: []core.Project{{ID: "project"}},
		Groups: []core.Group{{
			ChatID:         "group",
			ProjectID:      "project",
			Members:        []string{"member"},
			AllowOtherBots: allow,
		}},
	}
}

func otherBotMessage(mention bool) core.Message {
	return core.Message{ID: "bot-message", ChatID: "group", ChatType: "group", SenderID: "other-bot", SenderIsBot: true, Mentioned: mention, Text: "hello", Time: time.Now().UnixMilli()}
}

func TestOtherBotPolicy(t *testing.T) {
	m := otherBotMessage(true)
	c := otherBotConfig(true)
	r, ok := Route(c, m)
	if !ok || !Triggers(r, m) || !Readable(r, m) {
		t.Fatalf("mentioned other bot was not admitted: %+v %t", r, ok)
	}
	m.Mentioned = false
	if Triggers(r, m) {
		t.Fatal("unmentioned other bot must not trigger")
	}
	if !Readable(r, m) {
		t.Fatal("unmentioned other bot should still be readable")
	}
	m.SenderIsSelf = true
	if _, ok = Route(c, m); ok {
		t.Fatal("self bot message must not be routed")
	}
	m.SenderIsSelf = false

	c.Groups[0].AllowOtherBots = false
	if _, ok = Route(c, m); ok {
		t.Fatal("other bot admitted without allowOtherBots")
	}
	room := core.Room{ChatID: "group", ProjectID: "project", Members: []string{"member"}}
	if Readable(room, m) {
		t.Fatal("other bot readable without allowOtherBots")
	}

	c.Groups[0].AllowOtherBots = true
	c.Groups[0].MentionOptional = true
	if Triggers(r, m) {
		t.Fatal("mentionOptional must not make other bots trigger without @")
	}
}

func TestOtherBotAdmissionAndHumanUnchanged(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].AllowOtherBots = true
		c.Limits.BackgroundMessages = 1
	})
	background := otherBotMessage(false)
	background.ID = "bot-background"
	f.send(background)
	eventually(t, func() bool {
		var status string
		return f.s.DB.QueryRow("SELECT status FROM inbox WHERE id=?", background.ID).Scan(&status) == nil && status == "background"
	})
	if f.starts.Load() != 0 {
		t.Fatal("unmentioned other bot started a worker")
	}

	trigger := otherBotMessage(true)
	trigger.ID = "bot-trigger"
	f.send(trigger)
	w := f.worker()
	d := f.delivery(w)
	f.finish(w, d, "ok")
	if f.starts.Load() != 1 {
		t.Fatalf("other bot trigger starts = %d", f.starts.Load())
	}

	human := f.message("human", "hello", "member")
	f.send(human)
	humanDelivery := f.delivery(w)
	f.finish(w, humanDelivery, "ok")
}

func TestOtherBotDisabledByDefault(t *testing.T) {
	f := newFixture(t, nil)
	f.send(otherBotMessage(true))
	if f.starts.Load() != 0 {
		t.Fatal("other bot started a worker without allowOtherBots")
	}
	var count int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM inbox WHERE id='bot-message'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("other bot input was stored: %d %v", count, err)
	}
}

func TestOtherBotTriggerLimit(t *testing.T) {
	f := newFixture(t, nil)
	r := f.room()
	m := core.Message{ID: "bot", ChatID: r.ChatID, ChatType: "group", SenderID: "other-bot", SenderIsBot: true, Mentioned: true, ThreadID: "thread-1", Time: time.Now().UnixMilli()}
	for i := 0; i < otherBotLimit; i++ {
		if allowed, warn := f.g.otherBotTrigger(r, m); !allowed || warn {
			t.Fatalf("round %d = %t %t", i+1, allowed, warn)
		}
	}
	if allowed, warn := f.g.otherBotTrigger(r, m); allowed || !warn {
		t.Fatalf("limit round = %t %t", allowed, warn)
	}
	if allowed, warn := f.g.otherBotTrigger(r, m); allowed || warn {
		t.Fatalf("repeated blocked round = %t %t", allowed, warn)
	}
	human := m
	human.SenderIsBot = false
	human.SenderID = "member"
	f.g.resetOtherBotRounds(r, human)
	if allowed, warn := f.g.otherBotTrigger(r, m); !allowed || warn {
		t.Fatalf("human reset did not clear bot rounds: %t %t", allowed, warn)
	}
	otherTopic := m
	otherTopic.ThreadID = "thread-2"
	for i := 0; i < otherBotLimit; i++ {
		if allowed, _ := f.g.otherBotTrigger(r, otherTopic); !allowed {
			t.Fatal("second topic shared the first topic's limit")
		}
	}
}

func TestOtherBotLimitThroughMessage(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].AllowOtherBots = true
		c.Limits.BackgroundMessages = 1
	})
	for i := 0; i < otherBotLimit; i++ {
		m := otherBotMessage(true)
		m.ID = "bot-trigger-" + string(rune('1'+i))
		f.send(m)
	}
	f.worker()
	blocked := otherBotMessage(true)
	blocked.ID = "bot-blocked"
	f.send(blocked)
	eventually(t, func() bool {
		var status string
		return f.s.DB.QueryRow("SELECT status FROM inbox WHERE id=?", blocked.ID).Scan(&status) == nil && status == "background"
	})
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, notice := range f.tr.notices {
			if notice == otherBotLimitNotice {
				return true
			}
		}
		return false
	})
}

func TestOtherBotLimitNoticeText(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].AllowOtherBots = true
	})
	r := f.room()
	m := otherBotMessage(true)
	m.Time = time.Now().UnixMilli()
	for i := 0; i < otherBotLimit; i++ {
		if allowed, _ := f.g.otherBotTrigger(r, m); !allowed {
			t.Fatal("early trigger was blocked")
		}
	}
	if allowed, warn := f.g.otherBotTrigger(r, m); allowed || !warn {
		t.Fatal("limit trigger was not blocked with a warning")
	}
	if allowed, warn := f.g.otherBotTrigger(r, m); allowed || warn {
		t.Fatal("repeated limit trigger warned again")
	}
	if !strings.Contains(otherBotLimitNotice, "2 轮") {
		t.Fatal("limit notice does not state the round limit")
	}
}
