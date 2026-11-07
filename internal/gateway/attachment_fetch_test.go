package gateway

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestRepeatedAttachmentFetchReusesTheFinishedFile(t *testing.T) {
	f := newFixture(t, nil)
	m := f.message("with-file", "see file", "member")
	m.Resources = []core.Resource{{Key: "file_key", Kind: "file", Name: "report.txt"}}
	f.send(m)
	w := f.worker()
	f.delivery(w)
	s := f.g.getSlot(f.room().Key)
	ev := core.WorkerEvent{Type: "request", Kind: "attachment", Input: core.Args{"messageId": "with-file", "key": "file_key"}}
	first, err := f.g.request(s, ev)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.g.request(s, ev)
	if err != nil {
		t.Fatalf("repeated fetch failed: %v", err)
	}
	path := stringArg(first, "path")
	if path == "" || stringArg(second, "path") != path {
		t.Fatalf("paths differ: %v %v", first, second)
	}
	if data, e := os.ReadFile(path); e != nil || string(data) != "image bytes" {
		t.Fatalf("stored attachment: %q %v", data, e)
	}
	entries, _ := os.ReadDir(s.workspace.Inbound)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".part-") {
			t.Fatalf("partial download left behind: %s", entry.Name())
		}
	}
}

func TestPreparationFailureIsReportedAsNotSubmitted(t *testing.T) {
	f := newFixture(t, nil)
	f.tr.downloadHook = func(context.Context, string, core.Resource, string, int64) error {
		return errors.New("fixture download failure")
	}
	m := f.message("broken", "see file", "member")
	m.Resources = []core.Resource{{Key: "file_key", Kind: "file", Name: "a.txt"}}
	f.send(m)
	f.worker()
	eventually(t, func() bool { return f.status("broken") == "cancelled" })
	eventually(t, func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, n := range f.tr.notices {
			if strings.Contains(n, "未提交给模型") {
				return true
			}
		}
		return false
	})
}
