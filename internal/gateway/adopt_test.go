package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	nativepkg "github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

type fakeDeliveryFailure struct{ uncertain bool }

func (e *fakeDeliveryFailure) Error() string                     { return "fixture transport failure" }
func (e *fakeDeliveryFailure) DeliveryRetryable() bool           { return false }
func (e *fakeDeliveryFailure) DeliveryUncertain() bool           { return e.uncertain }
func (e *fakeDeliveryFailure) DeliveryRetryAfter() time.Duration { return 0 }

func adoptFixture(t *testing.T) (*fixture, string, string) {
	t.Helper()
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	native := "11111111-2222-3333-4444-555555555555"
	project := filepath.Join(configDir, "projects", nativepkg.EncodeProjectDir(work))
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	cwdJSON, _ := json.Marshal(work)
	lines := []string{
		`{"type":"user","cwd":` + string(cwdJSON) + `}`,
		`{"type":"assistant","cwd":` + string(cwdJSON) + `,"message":{"model":"test-model"}}`,
	}
	if err := os.WriteFile(filepath.Join(project, native+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].NativeConfig = "inherit"
		c.DMs[0].AdoptRoots = []string{work}
	})
	f.tr.fetchHook = func(_ context.Context, id string, room core.Room) (*core.Message, error) {
		return &core.Message{ID: id, ChatID: room.ChatID, ChatType: "p2p", SenderID: "app", SenderIsSelf: true, ThreadID: "omt_fixture", RootID: room.Thread}, nil
	}
	return f, work, native
}

func attachRequest(native, work string) core.AttachRequest {
	return core.AttachRequest{ID: core.NewID(), At: time.Now().UnixMilli(), User: "member", Native: native, CWD: work}
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(fixtureWait)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAttachCreatesOpenTopicAndIsIdempotent(t *testing.T) {
	f, work, native := adoptFixture(t)
	result := f.g.Attach(context.Background(), attachRequest(native, work))
	if !result.OK || result.State != store.AdoptedOpen || result.ThreadID != "omt_fixture" || result.RootMessageID == "" || result.ChatID != "oc_adopted" {
		t.Fatalf("attach result: %+v", result)
	}
	record, found, err := f.s.AdoptedTopicByNative(native)
	if err != nil || !found || record.State != store.AdoptedOpen || record.Thread != "omt_fixture" || record.Root != result.RootMessageID {
		t.Fatalf("adoption record: %+v %v %v", record, found, err)
	}
	if record.Room != result.Room {
		t.Fatalf("room mismatch: %q != %q", record.Room, result.Room)
	}
	if len(f.tr.userCards) != 1 {
		t.Fatalf("root cards = %d", len(f.tr.userCards))
	}
	again := f.g.Attach(context.Background(), attachRequest(native, work))
	if !again.OK || again.Room != result.Room || again.ThreadID != result.ThreadID {
		t.Fatalf("re-attach: %+v", again)
	}
	if len(f.tr.userCards) != 1 {
		t.Fatalf("re-attach sent another root card: %d", len(f.tr.userCards))
	}
	f.c.DMs[1].AdoptRoots = f.c.DMs[0].AdoptRoots
	foreign := attachRequest(native, work)
	foreign.User = "owner"
	if result := f.g.Attach(context.Background(), foreign); result.OK {
		t.Fatalf("another DM adopted the same native session: %+v", result)
	}
}

func TestAttachDefiniteRootRejectionCanBeRetried(t *testing.T) {
	f, work, native := adoptFixture(t)
	f.tr.userCardHook = func(context.Context, string, core.Args) (string, string, error) {
		return "", "", errors.New("definite rejection")
	}
	if result := f.g.Attach(context.Background(), attachRequest(native, work)); result.OK {
		t.Fatalf("rejected attach reported success: %+v", result)
	}
	if _, found, _ := f.s.AdoptedTopicByNative(native); found {
		t.Fatal("definite root rejection kept a pending record")
	}
	f.tr.userCardHook = nil
	if result := f.g.Attach(context.Background(), attachRequest(native, work)); !result.OK {
		t.Fatalf("retry after definite rejection failed: %+v", result)
	}
}

func TestAttachUncertainRootSendIsNotReplayed(t *testing.T) {
	f, work, native := adoptFixture(t)
	f.tr.userCardHook = func(context.Context, string, core.Args) (string, string, error) {
		return "", "", &fakeDeliveryFailure{uncertain: true}
	}
	result := f.g.Attach(context.Background(), attachRequest(native, work))
	if result.OK || result.State != store.AdoptedUncertain {
		t.Fatalf("uncertain root send: %+v", result)
	}
	if record, found, _ := f.s.AdoptedTopicByNative(native); !found || record.State != store.AdoptedUncertain {
		t.Fatalf("uncertain record: %+v %v", record, found)
	}
	f.tr.userCardHook = nil
	again := f.g.Attach(context.Background(), attachRequest(native, work))
	if again.OK || again.State != store.AdoptedUncertain {
		t.Fatalf("uncertain retry: %+v", again)
	}
	if len(f.tr.userCards) != 1 {
		t.Fatalf("uncertain retry sent another card: %d", len(f.tr.userCards))
	}
}

func TestAttachAfterRootFailureKeepsRootAndNeverReplays(t *testing.T) {
	f, work, native := adoptFixture(t)
	f.tr.fetchHook = func(_ context.Context, id string, room core.Room) (*core.Message, error) {
		return &core.Message{ID: id, ChatID: room.ChatID, ChatType: "p2p", SenderIsSelf: true, RootID: room.Thread}, nil
	}
	result := f.g.Attach(context.Background(), attachRequest(native, work))
	if result.OK || result.State != store.AdoptedUncertain || result.RootMessageID == "" {
		t.Fatalf("thread readback failure: %+v", result)
	}
	record, found, _ := f.s.AdoptedTopicByNative(native)
	if !found || record.State != store.AdoptedUncertain || record.Root != result.RootMessageID {
		t.Fatalf("root evidence lost: %+v %v", record, found)
	}
	again := f.g.Attach(context.Background(), attachRequest(native, work))
	if again.OK || again.RootMessageID != result.RootMessageID {
		t.Fatalf("uncertain retry: %+v", again)
	}
	if len(f.tr.userCards) != 1 {
		t.Fatalf("retry resent the root card: %d", len(f.tr.userCards))
	}
}

func TestAttachRejectsSymlinkedTranscriptPath(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	if err = os.MkdirAll(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err = os.Symlink(real, link); err != nil {
		t.Skipf("directory symlinks are unavailable: %v", err)
	}
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	native := "11111111-2222-3333-4444-555555555555"
	project := filepath.Join(configDir, "projects", nativepkg.EncodeProjectDir(link))
	if err = os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	cwdJSON, _ := json.Marshal(link)
	if err = os.WriteFile(filepath.Join(project, native+".jsonl"), []byte(`{"cwd":`+string(cwdJSON)+`}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(c *core.Config) {
		c.Projects[0].NativeConfig = "inherit"
		c.DMs[0].AdoptRoots = []string{real}
	})
	f.tr.fetchHook = func(_ context.Context, id string, room core.Room) (*core.Message, error) {
		return &core.Message{ID: id, ChatID: room.ChatID, ChatType: "p2p", SenderIsSelf: true, ThreadID: "omt_fixture", RootID: room.Thread}, nil
	}
	result := f.g.Attach(context.Background(), attachRequest(native, link))
	if result.OK {
		t.Fatalf("symlinked transcript path was adopted: %+v", result)
	}
	if !strings.Contains(result.Error, "物理工作目录") {
		t.Fatalf("unexpected rejection: %q", result.Error)
	}
}

func TestAttachRejectsOutsideRootsAndMissingInheritance(t *testing.T) {
	f, _, native := adoptFixture(t)
	outside := t.TempDir()
	if result := f.g.Attach(context.Background(), attachRequest(native, outside)); result.OK {
		t.Fatalf("outside work was accepted: %+v", result)
	}
	f.c.Projects[0].NativeConfig = "isolated"
	if result := f.g.Attach(context.Background(), attachRequest(native, "")); result.OK {
		t.Fatalf("isolated project was accepted: %+v", result)
	}
}

func openAdoptionFixture(t *testing.T) (*fixture, core.Room, store.AdoptedTopic) {
	t.Helper()
	f, work, native := adoptFixture(t)
	result := f.g.Attach(context.Background(), attachRequest(native, work))
	if !result.OK {
		t.Fatalf("attach failed: %+v", result)
	}
	record, _, _ := f.s.AdoptedTopicByNative(native)
	room, ok := f.c.RoomForKey(record.Room)
	if !ok {
		t.Fatal("adopted room key did not resolve")
	}
	return f, room, record
}

func TestAdoptedRouteRequiresExactThreadAndRoot(t *testing.T) {
	f, room, _ := openAdoptionFixture(t)
	base := core.Message{ID: "om_in", ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", ThreadID: "omt_fixture", RootID: room.Thread, Text: "hello", Time: time.Now().UnixMilli(), Resources: []core.Resource{}}
	routed, ok := f.g.route(base)
	if !ok || routed.Key != room.Key || routed.AdoptedState != store.AdoptedOpen || routed.WorkspaceCWD == "" {
		t.Fatalf("routed room: %+v %v", routed, ok)
	}
	wrongRoot := base
	wrongRoot.RootID = "om_other"
	if _, ok := f.g.route(wrongRoot); ok {
		t.Fatal("wrong root was routed to the adopted topic")
	}
	wrongThread := base
	wrongThread.ThreadID = "omt_other"
	if _, ok := f.g.route(wrongThread); ok {
		t.Fatal("wrong thread was routed to the adopted topic")
	}
	var rejected int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='adopted_topic_rejected'").Scan(&rejected); err != nil || rejected == 0 {
		t.Fatalf("rejection audit rows = %d, %v", rejected, err)
	}
}

func TestShapeValidAdoptedKeyWithoutRecordIsRejected(t *testing.T) {
	f, _, _ := adoptFixture(t)
	room, ok := f.c.AdoptedTopicRoom("oc_adopted", "member", "om_unregistered")
	if !ok {
		t.Fatal("fixture config did not produce a shape-valid key")
	}
	if _, ok := f.g.roomFromKey(room.Key); ok {
		t.Fatal("shape-valid key without an adoption record was accepted")
	}
}

func TestPendingAdoptionGetsNoticeWithoutStartingWorker(t *testing.T) {
	f, work, native := adoptFixture(t)
	room, ok := f.c.AdoptedTopicRoom("oc_adopted", "member", "om_pending_root")
	if !ok {
		t.Fatal("fixture config did not produce an adopted room")
	}
	_, configDir, override, err := workspace.InheritedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.s.BeginAdoption(native, "member", work, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = f.s.BindAdoptionRoot(native, room.Key, "om_pending_root", core.NewID(), configDir, override, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	msg := core.Message{ID: "om_pending_in", ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", ThreadID: "omt_pending", RootID: "om_pending_root", Text: "hello", Time: time.Now().UnixMilli(), Resources: []core.Resource{}}
	f.send(msg)
	if f.starts.Load() != 0 {
		t.Fatal("pending adoption started a native worker")
	}
	waitFor(t, "pending notice", func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, notice := range f.tr.notices {
			if strings.Contains(notice, "认领尚未完成") {
				return true
			}
		}
		return false
	})
}

func TestInvalidAdoptedWorkIsRejectedWithNotice(t *testing.T) {
	f, work, native := adoptFixture(t)
	result := f.g.Attach(context.Background(), attachRequest(native, work))
	if !result.OK {
		t.Fatalf("attach failed: %+v", result)
	}
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	msg := core.Message{ID: "om_missing_work", ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", ThreadID: "omt_fixture", RootID: result.RootMessageID, Text: "hello", Time: time.Now().UnixMilli(), Resources: []core.Resource{}}
	f.send(msg)
	if f.starts.Load() != 0 {
		t.Fatal("missing work directory started a worker")
	}
	waitFor(t, "invalid work notice", func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, notice := range f.tr.notices {
			if strings.Contains(notice, "工作目录已不可用或不在 adoptRoots 内") {
				return true
			}
		}
		return false
	})
}

func TestNewInHeldTopicRotatesSessionAndReleasesGate(t *testing.T) {
	f, work, native := adoptFixture(t)
	pid := os.Getpid()
	started, ok := process.StartTime(pid)
	if !ok {
		t.Skip("process start time is unavailable")
	}
	req := attachRequest(native, work)
	req.HolderPID, req.HolderStarted = pid, started
	result := f.g.Attach(context.Background(), req)
	if !result.OK {
		t.Fatalf("attach failed: %+v", result)
	}
	room, ok := f.g.roomFromKey(result.Room)
	if !ok {
		t.Fatal("adopted room did not resolve")
	}
	f.send(core.Message{ID: "om_new", ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", ThreadID: "omt_fixture", RootID: room.Thread, Text: "/new", Time: time.Now().UnixMilli(), Resources: []core.Resource{}})
	current, found, err := f.s.CurrentSession(room.Key)
	if err != nil || !found {
		t.Fatalf("current session: %+v %v", current, err)
	}
	record, _, _ := f.s.AdoptedTopicByNative(native)
	if current.ID == record.Session {
		t.Fatal("/new did not rotate the adopted session")
	}
	f.send(core.Message{ID: "om_after_new", ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", ThreadID: "omt_fixture", RootID: room.Thread, Text: "hello", Time: time.Now().UnixMilli(), Resources: []core.Resource{}})
	worker := f.worker()
	select {
	case delivery := <-worker.sent:
		if delivery.SessionID != current.ID {
			t.Fatalf("delivery session = %q, want %q", delivery.SessionID, current.ID)
		}
	case <-time.After(fixtureWait):
		t.Fatal("new session was not dispatched after /new")
	}
}

func TestReattachIsRejectedWhileRoomHasPendingInput(t *testing.T) {
	f, work, native := adoptFixture(t)
	pid := os.Getpid()
	started, ok := process.StartTime(pid)
	if !ok {
		t.Skip("process start time is unavailable")
	}
	req := attachRequest(native, work)
	req.HolderPID, req.HolderStarted = pid, started
	result := f.g.Attach(context.Background(), req)
	if !result.OK {
		t.Fatalf("attach failed: %+v", result)
	}
	room, ok := f.g.roomFromKey(result.Room)
	if !ok {
		t.Fatal("adopted room did not resolve")
	}
	if _, err := f.s.Insert(room.Key, core.Message{ID: "om_pending", ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", Text: "queued", Time: time.Now().UnixMilli(), Resources: []core.Resource{}}, "queued"); err != nil {
		t.Fatal(err)
	}
	next := attachRequest(native, work)
	next.HolderPID, next.HolderStarted = pid, started
	if result := f.g.Attach(context.Background(), next); result.OK {
		t.Fatalf("re-attach accepted a room with pending input: %+v", result)
	} else if !strings.Contains(result.Error, "/stop") {
		t.Fatalf("re-attach rejection did not point to /stop: %q", result.Error)
	}
	var rejected int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM audit WHERE event='adopt_reattach_rejected'").Scan(&rejected); err != nil || rejected == 0 {
		t.Fatalf("re-attach rejection audit: %d %v", rejected, err)
	}
}

func TestHolderQueuesWithoutStartingAndResumesAfterExit(t *testing.T) {
	f, work, native := adoptFixture(t)
	pid := os.Getpid()
	started, ok := process.StartTime(pid)
	if !ok {
		t.Skip("process start time is unavailable")
	}
	req := attachRequest(native, work)
	req.HolderPID, req.HolderStarted = pid, started
	result := f.g.Attach(context.Background(), req)
	if !result.OK {
		t.Fatalf("attach failed: %+v", result)
	}
	room, ok := f.g.roomFromKey(result.Room)
	if !ok {
		t.Fatal("adopted room did not resolve")
	}
	msg := core.Message{ID: "om_held", ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", ThreadID: "omt_fixture", RootID: room.Thread, Text: "hello", Time: time.Now().UnixMilli(), Resources: []core.Resource{}}
	f.send(msg)
	if f.starts.Load() != 0 {
		t.Fatal("holder did not block worker startup")
	}
	waitFor(t, "holder notice", func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, notice := range f.tr.notices {
			if strings.Contains(notice, "本地终端仍在使用") {
				return true
			}
		}
		return false
	})
	var status string
	if err := f.s.DB.QueryRow("SELECT status FROM inbox WHERE room=? AND id=?", room.Key, "om_held").Scan(&status); err != nil || status != "queued" {
		t.Fatalf("held input status = %q, %v", status, err)
	}
	var receipts int
	if err := f.s.DB.QueryRow("SELECT count(*) FROM receipts WHERE id='om_held'").Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("held input did not get a processing receipt: %d %v", receipts, err)
	}
	// Simulate the terminal exiting without deleting the whole record: the
	// sweep sees a dead PID, releases the holder and kicks the queued input.
	if err := f.s.UpdateAdoptionHolder(native, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	f.g.Sweep(context.Background(), time.Now().UnixMilli())
	worker := f.worker()
	delivery := f.delivery(worker)
	if delivery.Message.ID != "om_held" {
		t.Fatalf("delivery = %+v", delivery)
	}
	f.finish(worker, delivery, "done")
	eventually(t, func() bool { return f.status("om_held") == "done" })
}

func TestUncertainAdoptionWithRootEvidenceStillNotifies(t *testing.T) {
	f, work, native := adoptFixture(t)
	f.tr.userCardHook = func(context.Context, string, core.Args) (string, string, error) {
		if _, err := f.s.DB.Exec("INSERT INTO sessions(id,room,cc,state,created) VALUES('conflict','room:conflict',?,'open',1)", native); err != nil {
			return "", "", err
		}
		return "om_evidence_root", "oc_adopted", nil
	}
	result := f.g.Attach(context.Background(), attachRequest(native, work))
	if result.OK || result.State != store.AdoptedUncertain || result.RootMessageID != "om_evidence_root" {
		t.Fatalf("attach result: %+v", result)
	}
	record, found, err := f.s.AdoptedTopicByNative(native)
	if err != nil || !found || record.State != store.AdoptedUncertain || record.Room == "" || record.Root != "om_evidence_root" {
		t.Fatalf("root evidence: %+v %v %v", record, found, err)
	}
	msg := core.Message{ID: "om_uncertain_in", ChatID: "oc_adopted", ChatType: "p2p", SenderID: "member", ThreadID: "omt_unknown", RootID: "om_evidence_root", Text: "hello", Time: time.Now().UnixMilli(), Resources: []core.Resource{}}
	f.send(msg)
	if f.starts.Load() != 0 {
		t.Fatal("uncertain adoption started a worker")
	}
	waitFor(t, "uncertain notice", func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		for _, notice := range f.tr.notices {
			if strings.Contains(notice, "认领结果未确认") {
				return true
			}
		}
		return false
	})
}

func TestReattachRejectsDifferentLiveHolder(t *testing.T) {
	f, work, native := adoptFixture(t)
	pid := os.Getpid()
	started, ok := process.StartTime(pid)
	if !ok {
		t.Skip("process start time is unavailable")
	}
	req := attachRequest(native, work)
	req.HolderPID, req.HolderStarted = pid, started
	if result := f.g.Attach(context.Background(), req); !result.OK {
		t.Fatalf("attach failed: %+v", result)
	}
	parent := os.Getppid()
	if parent <= 0 || parent == pid {
		t.Skip("no distinct live parent process")
	}
	parentStarted, parentOK := process.StartTime(parent)
	next := attachRequest(native, work)
	next.HolderPID, next.HolderStarted = parent, parentStarted
	if !parentOK {
		next.HolderStarted = 0
	}
	result := f.g.Attach(context.Background(), next)
	if result.OK {
		t.Fatal("re-attach replaced a different live holder")
	}
	if !strings.Contains(result.Error, "仍由本地终端") {
		t.Fatalf("re-attach rejection: %q", result.Error)
	}
}
