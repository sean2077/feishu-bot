package gateway

import (
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestMentionOptionalGroupTriggersUnmentionedMessages(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Groups[0].MentionOptional = true })
	f.send(unmentioned(f.message("one", "hello", "member")))
	w := f.worker()
	if d := f.delivery(w); d.Message.ID != "one" {
		t.Fatalf("delivered %q", d.Message.ID)
	}
	f.send(unmentioned(f.message("two", "hello", "owner")))
	if d := f.delivery(w); d.Message.ID != "two" {
		t.Fatalf("delivered %q", d.Message.ID)
	}
}

func TestMentionOptionalGroupStillRejectsUnauthorized(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Groups[0].MentionOptional = true })
	stranger := unmentioned(f.message("stranger", "secret", "stranger"))
	foreign := unmentioned(f.message("foreign", "secret", "member"))
	foreign.ChatID = "unknown"
	f.send(stranger)
	f.send(foreign)
	var n int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM inbox").Scan(&n); err != nil {
		t.Fatal(err)
	}
	f.tr.mu.Lock()
	effects := f.tr.downloads + f.tr.fetches + len(f.tr.receipts) + len(f.tr.cards) + len(f.tr.notices)
	f.tr.mu.Unlock()
	if n != 0 || effects != 0 || f.starts.Load() != 0 {
		t.Fatalf("rejected input had effects: rows=%d remote=%d starts=%d", n, effects, f.starts.Load())
	}
}

func TestMentionOptionalIsPerGroupAndDoesNotAffectDM(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Groups[0].MentionOptional = true })
	other := unmentioned(f.message("other", "hello", "member"))
	other.ChatID = "other"
	f.send(other)
	if f.starts.Load() != 0 {
		t.Fatal("unmentioned message in a mention-required group started a worker")
	}
	dm := unmentioned(f.message("dm", "hello", "member"))
	dm.ChatType = "p2p"
	dm.ChatID = "dm-chat"
	f.send(dm)
	w := f.worker()
	if d := f.delivery(w); d.Message.ID != "dm" {
		t.Fatalf("delivered %q", d.Message.ID)
	}
}

func TestMentionOptionalGroupBypassesBackgroundCache(t *testing.T) {
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].MentionOptional = true
		c.Limits.BackgroundMessages = 5
	})
	f.send(unmentioned(f.message("one", "hello", "member")))
	w := f.worker()
	f.delivery(w)
	var n int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM inbox WHERE status='background'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("mention-optional input was cached as background: %d", n)
	}
}

func TestDefaultGroupStillUsesBackgroundCache(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Limits.BackgroundMessages = 5 })
	f.send(unmentioned(f.message("one", "hello", "member")))
	var n int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM inbox WHERE status='background'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("unmentioned input was not cached as background: %d", n)
	}
	if f.starts.Load() != 0 {
		t.Fatal("background input started a worker")
	}
}

func TestMentionOptionalGroupCommandsSkipMention(t *testing.T) {
	f := newFixture(t, func(c *core.Config) { c.Groups[0].MentionOptional = true })
	f.send(unmentioned(f.message("status", "/status --json", "member")))
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		return len(f.tr.notices) == 1
	})
	if f.starts.Load() != 0 {
		t.Fatal("read command created a native session")
	}
}

func unmentioned(m core.Message) core.Message {
	m.Mentioned = false
	return m
}
