package gateway

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sean2077/feishu-bot/internal/core"
	nativepkg "github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

func TestAdoptedTranscriptModelIsScopedToTheAdoptedSession(t *testing.T) {
	f, work, native := adoptFixture(t)
	result := f.g.Attach(context.Background(), attachRequest(native, work))
	if !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	record, _, _ := f.s.AdoptedTopicByNative(native)
	room, ok := f.g.roomFromKey(record.Room)
	if !ok {
		t.Fatal("adopted room did not resolve")
	}
	project, _ := f.g.config.Project(room.ProjectID)
	if model, ok := f.g.adoptedTranscriptModel(room, record.Session, ""); !ok || model != "test-model" {
		t.Fatalf("transcript model = %q %v", model, ok)
	}
	if _, ok := f.g.adoptedTranscriptModel(room, record.Session, "default"); ok {
		t.Fatal("transcript model applied after an explicit profile")
	}
	if model, source := f.g.roomModelSelection(room, project, record.Session, "default"); model != "test-model" || source != "session" {
		t.Fatalf("explicit profile selection = %q %q", model, source)
	}

	view, err := f.g.statusView(room)
	if err != nil {
		t.Fatal(err)
	}
	adoption := argsOf(view["adoption"])
	if stringArg(adoption, "model") != "test-model" || stringArg(adoption, "modelSource") != "transcript" {
		t.Fatalf("adopted status model: %+v", adoption)
	}
	text, _, err := f.g.management("/status", "", room, "member")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "模型：test-model（来源：本地会话 transcript）") {
		t.Fatalf("status text: %s", text)
	}

	applyTestProfile(t, f, room.Key, record.Session, "default")
	view, err = f.g.statusView(room)
	if err != nil {
		t.Fatal(err)
	}
	adoption = argsOf(view["adoption"])
	if stringArg(adoption, "modelSource") != "session" || stringArg(adoption, "model") != "test-model" {
		t.Fatalf("explicit profile status: %+v", adoption)
	}

	// /new selects another session, so it must fall back to the project model.
	if _, err = f.s.NewSession(room.Key); err != nil {
		t.Fatal(err)
	}
	current, found, err := f.s.CurrentSession(room.Key)
	if err != nil || !found || current.ID == record.Session {
		t.Fatalf("new session: %+v %v %v", current, found, err)
	}
	if _, ok := f.g.adoptedTranscriptModel(room, current.ID, ""); ok {
		t.Fatal("new session inherited the transcript model")
	}
	if model, source := f.g.roomModelSelection(room, project, current.ID, ""); model != "test-model" || source == "transcript" {
		t.Fatalf("new session model = %q %q", model, source)
	}
	view, err = f.g.statusView(room)
	if err != nil {
		t.Fatal(err)
	}
	adoption = argsOf(view["adoption"])
	if stringArg(adoption, "modelSource") == "transcript" {
		t.Fatalf("new session status kept the transcript source: %+v", adoption)
	}
}

func TestReserveAppliesAdoptedTranscriptModel(t *testing.T) {
	f, work, native := adoptFixture(t)
	if result := f.g.Attach(context.Background(), attachRequest(native, work)); !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	record, _, _ := f.s.AdoptedTopicByNative(native)
	room, ok := f.g.roomFromKey(record.Room)
	if !ok {
		t.Fatal("adopted room did not resolve")
	}
	slot, err := f.g.reserve(room)
	if err != nil || slot == nil {
		t.Fatalf("reserve: %+v %v", slot, err)
	}
	if slot.session.ModelOverride != "test-model" {
		t.Fatalf("reserved model override = %q", slot.session.ModelOverride)
	}
	slot.cancel()
	f.g.mu.Lock()
	delete(f.g.slots, room.Key)
	f.g.mu.Unlock()
}

func TestAttachWarningExplainsTranscriptModel(t *testing.T) {
	f, work, native := adoptFixture(t)
	f.g.config.Model.Model = "project-model"
	result := f.g.Attach(context.Background(), attachRequest(native, work))
	if !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	found := false
	for _, warning := range result.Warnings {
		if warning == "终端退出后将沿用 test-model 续接。" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v", result.Warnings)
	}
}

func TestAdoptedTranscriptModelFallsBackWhenUnreadable(t *testing.T) {
	f, work, native := adoptFixture(t)
	if result := f.g.Attach(context.Background(), attachRequest(native, work)); !result.OK {
		t.Fatalf("attach: %+v", result)
	}
	record, _, _ := f.s.AdoptedTopicByNative(native)
	room, ok := f.g.roomFromKey(record.Room)
	if !ok {
		t.Fatal("adopted room did not resolve")
	}
	_, configDir, _, err := workspace.InheritedConfig()
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := nativepkg.LocateTranscript(configDir, native)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(transcript.Path); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.g.adoptedTranscriptModel(room, record.Session, ""); ok {
		t.Fatal("missing transcript still produced an override")
	}
	project, _ := f.g.config.Project(room.ProjectID)
	if model, source := f.g.roomModelSelection(room, project, record.Session, ""); model != "test-model" || source == "transcript" {
		t.Fatalf("fallback model = %q %q", model, source)
	}
}

func TestRenderUsesAdoptedModelOverride(t *testing.T) {
	f := newFixture(t, nil)
	slot := &slot{room: f.room(), session: core.Session{ID: "session", ModelOverride: "claude-opus-5-5"}, ctx: context.Background()}
	render := newRender(f.g, slot, nil)
	if render.details.Model != "claude-opus-5-5" {
		t.Fatalf("render model = %q", render.details.Model)
	}
}

func TestAdoptedModelWarningDescribesTakeover(t *testing.T) {
	if got := adoptedModelWarning("project-model", "claude-opus-5-5"); got != "终端退出后将沿用 claude-opus-5-5 续接。" {
		t.Fatalf("warning = %q", got)
	}
	if got := adoptedModelWarning("claude-opus-5-5[1m]", "claude-opus-5-5"); got != "" {
		t.Fatalf("same model warning = %q", got)
	}
	if got := adoptedModelWarning("project-model", ""); got != "" {
		t.Fatalf("empty transcript warning = %q", got)
	}
}
