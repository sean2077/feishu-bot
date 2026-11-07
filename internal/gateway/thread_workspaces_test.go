package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

func TestTopicWorkspaceBindingReachesWorkerWithoutChangingRoomIdentity(t *testing.T) {
	bound := t.TempDir()
	f := newFixture(t, func(c *core.Config) {
		c.Groups[0].SessionScope = core.SessionScopeThread
		c.Groups[0].ThreadWorkspaces = map[string]string{"om_bound": bound}
	})
	workspaces := make(chan core.Workspace, 2)
	factory := f.g.factory
	f.g.factory = func(ctx context.Context, r core.Room, s core.Session, w core.Workspace, emit func(core.WorkerEvent)) (core.Worker, error) {
		workspaces <- w
		return factory(ctx, r, s, w, emit)
	}
	f.send(f.message("om_bound", "edit feature", "member"))
	w1 := f.worker()
	d1 := f.delivery(w1)
	first := <-workspaces
	f.send(f.message("om_shared", "read shared notes", "second"))
	w2 := f.worker()
	d2 := f.delivery(w2)
	second := <-workspaces
	want, err := filepath.EvalSymlinks(bound)
	if err != nil {
		t.Fatal(err)
	}
	if first.Work != want || second.Work != filepath.Join(first.Root, "workspace") || first.Home != second.Home || first.Inbound == second.Inbound {
		t.Fatalf("wrong directories at native launch: bound=%+v shared=%+v", first, second)
	}
	if d1.RoomKey != f.topic("om_bound").Key || d2.RoomKey != f.topic("om_shared").Key || d1.SessionID == d2.SessionID {
		t.Fatal("workspace binding changed topic routing or merged sessions")
	}
	var savedWork string
	if err := f.s.DB.QueryRow("SELECT work FROM session_launch_identity WHERE session=?", d1.SessionID).Scan(&savedWork); err != nil || savedWork != first.Work {
		t.Fatal("bound directory not persisted in the launch identity", savedWork, err)
	}
	view, err := f.g.statusView(f.topic("om_bound"))
	if err != nil || view["threadRoot"] != "om_bound" || view["workspaceBinding"] != bound {
		t.Fatal("status does not identify the configured topic binding", view, err)
	}
}

func TestTopicWorkspaceRebindingRequiresNewSessionAfterRestart(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "recorded identity"
		if legacy {
			name = "legacy native session without identity"
		}
		t.Run(name, func(t *testing.T) {
			f := topicFixture(t)
			f.send(f.message("om_root", "start shared work", "member"))
			w := f.worker()
			d := f.delivery(w)
			f.finish(w, d, "done")
			eventually(t, func() bool { return f.status("om_root") == "done" })
			before := f.openSession(d.RoomKey)
			if before.CCID == "" {
				t.Fatal("native session was not saved")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := f.g.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if legacy {
				if _, err := f.s.DB.Exec("DELETE FROM session_launch_identity WHERE session=?", before.ID); err != nil {
					t.Fatal(err)
				}
			}
			bound := t.TempDir()
			f.c.Groups[0].ThreadWorkspaces = map[string]string{"om_root": bound}
			factory := f.g.factory
			f.g = New(f.c, f.s, f.tr, factory, nil)
			f.send(inTopic(f.message("om_rebound", "continue in new directory", "member"), "om_root", "omt_root"))
			eventually(t, func() bool { return noticeContains(f, "/new") })
			if f.starts.Load() != 1 {
				t.Fatal("existing native session launched in a different directory")
			}
			after := f.openSession(d.RoomKey)
			if after.ID != before.ID || after.CCID != before.CCID {
				t.Fatal("refused rebinding rotated the native session")
			}
			f.send(inTopic(f.message("om_new", "/new", "owner"), "om_root", "omt_root"))
			f.send(inTopic(f.message("om_fresh", "start explicitly", "member"), "om_root", "omt_root"))
			freshWorker := f.worker()
			fresh := f.delivery(freshWorker)
			if fresh.SessionID == before.ID || fresh.RoomKey != before.RoomKey {
				t.Fatal("explicit /new did not keep the topic and replace only its session")
			}
			var savedWork string
			want, _ := filepath.EvalSymlinks(bound)
			if err := f.s.DB.QueryRow("SELECT work FROM session_launch_identity WHERE session=?", fresh.SessionID).Scan(&savedWork); err != nil || savedWork != want {
				t.Fatal("explicit new session did not use the bound workspace", savedWork, err)
			}
		})
	}
}
