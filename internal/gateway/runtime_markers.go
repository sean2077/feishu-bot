package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

// syncStateMarker keeps the fallback independent of SQLite. A partial write
// never replaces a valid marker; file sync precedes its atomic publication.
func syncStateMarker(path string, data []byte) error {
	tmp := path + ".tmp-" + core.NewID()
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	// Windows does not expose directory fsync through os.File.Sync.
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		return errors.Join(dir.Sync(), dir.Close())
	}
	return nil
}

type stopPauseMarker struct {
	Room    string `json:"room"`
	Session string `json:"session"`
}

func (g *Gateway) stopPausePath(room string) string {
	return filepath.Join(g.config.DataDir, "stop-pauses", core.Hash(room)+".json")
}

func (g *Gateway) persistStopPause(ctx context.Context, room, session string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dbErr := g.store.PauseRoomStop(ctx, room, session)
	if dbErr == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path := g.stopPausePath(room)
	err := os.MkdirAll(filepath.Dir(path), 0700)
	if err == nil {
		data, _ := json.Marshal(stopPauseMarker{room, session})
		err = syncStateMarker(path, data)
	}
	if err != nil {
		return errors.Join(dbErr, err)
	}
	g.audit(room, "stop_pause_marked", core.Args{"session": session})
	return nil
}

func (g *Gateway) stopPauseMarkers() ([]string, error) {
	dir := filepath.Dir(g.stopPausePath(""))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rooms []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		var marker stopPauseMarker
		if err != nil || len(data) > 65536 || json.Unmarshal(data, &marker) != nil || marker.Room == "" || marker.Session == "" || filepath.Base(g.stopPausePath(marker.Room)) != entry.Name() {
			return nil, errors.New("saved stop pause marker is unreadable; verify stop-pauses before restarting")
		}
		rooms = append(rooms, marker.Room)
	}
	return rooms, nil
}

func (g *Gateway) removeStopPauseMarker(room string) error {
	err := os.Remove(g.stopPausePath(room))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
