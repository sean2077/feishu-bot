package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

const adoptEndpointFileLimit = 64 << 10

// adoptEndpointDir holds one 0600 socket/token file per adopted native session.
// Endpoint files are outside SQLite, logs and audit; they exist only so a
// gateway restart can resume relay while the same local terminal still holds
// the session.
func (g *Gateway) adoptEndpointDir() string {
	return filepath.Join(g.config.DataDir, "endpoints")
}

func (g *Gateway) adoptEndpointPath(native string) (string, bool) {
	if !core.ValidThreadRoot(native) {
		return "", false
	}
	return filepath.Join(g.adoptEndpointDir(), native+".json"), true
}

type storedAdoptEndpoint struct {
	Socket        string `json:"socket"`
	Token         string `json:"token"`
	ConfigPath    string `json:"configPath,omitempty"`
	HolderPID     int    `json:"holderPid"`
	HolderStarted int64  `json:"holderStarted"`
}

func (g *Gateway) logEndpointError(event, native string, err error) {
	if g.logger == nil {
		return
	}
	g.logger(event, core.Args{"native": native, "error": core.Redact(err.Error(), g.secrets)})
}

// persistAdoptEndpoint writes the endpoint atomically with mode 0600. An
// incomplete endpoint is never persisted; the in-memory registration still
// works for this process.
func (g *Gateway) persistAdoptEndpoint(endpoint adoptEndpoint) {
	path, ok := g.adoptEndpointPath(endpoint.Native)
	if !ok || strings.TrimSpace(endpoint.Socket) == "" || strings.TrimSpace(endpoint.Token) == "" {
		return
	}
	record, found, err := g.store.AdoptedTopicByNative(endpoint.Native)
	if err != nil || !found || record.State != store.AdoptedOpen || !g.holderAlive(record) {
		return
	}
	data, err := json.Marshal(storedAdoptEndpoint{
		Socket:        endpoint.Socket,
		Token:         endpoint.Token,
		ConfigPath:    endpoint.ConfigPath,
		HolderPID:     record.HolderPID,
		HolderStarted: record.HolderStarted,
	})
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		g.logEndpointError("adopt_endpoint_persist_failed", endpoint.Native, err)
		return
	}
	temp, err := os.CreateTemp(dir, ".endpoint-*.tmp")
	if err != nil {
		g.logEndpointError("adopt_endpoint_persist_failed", endpoint.Native, err)
		return
	}
	tempName := temp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tempName)
		}
	}()
	if _, err = temp.Write(data); err != nil {
		_ = temp.Close()
		g.logEndpointError("adopt_endpoint_persist_failed", endpoint.Native, err)
		return
	}
	if err = temp.Close(); err != nil {
		g.logEndpointError("adopt_endpoint_persist_failed", endpoint.Native, err)
		return
	}
	if err = os.Rename(tempName, path); err != nil {
		g.logEndpointError("adopt_endpoint_persist_failed", endpoint.Native, err)
		return
	}
	renamed = true
}

func (g *Gateway) removeAdoptEndpointFile(native string) {
	path, ok := g.adoptEndpointPath(native)
	if !ok {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		g.logEndpointError("adopt_endpoint_remove_failed", native, err)
	}
}

func (g *Gateway) readAdoptEndpoint(native string) (adoptEndpoint, bool) {
	path, ok := g.adoptEndpointPath(native)
	if !ok {
		return adoptEndpoint{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return adoptEndpoint{}, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, adoptEndpointFileLimit+1))
	if err != nil || len(data) > adoptEndpointFileLimit {
		return adoptEndpoint{}, false
	}
	var stored storedAdoptEndpoint
	if json.Unmarshal(data, &stored) != nil {
		return adoptEndpoint{}, false
	}
	stored.Socket = strings.TrimSpace(stored.Socket)
	if stored.Socket == "" || strings.TrimSpace(stored.Token) == "" {
		return adoptEndpoint{}, false
	}
	return adoptEndpoint{
		Socket:        stored.Socket,
		Token:         stored.Token,
		ConfigPath:    stored.ConfigPath,
		Native:        native,
		HolderPID:     stored.HolderPID,
		HolderStarted: stored.HolderStarted,
		Restored:      true,
	}, true
}

func (g *Gateway) setRestoredAdoptEndpoint(room string, endpoint adoptEndpoint) {
	if room == "" {
		return
	}
	g.mu.Lock()
	if g.adoptEndpoints == nil {
		g.adoptEndpoints = map[string]adoptEndpoint{}
	}
	g.adoptEndpoints[room] = endpoint
	g.mu.Unlock()
}

// restoreAdoptEndpoints loads persisted endpoints only when the recorded local
// holder is still alive and the topic still selects the adopted session. Every
// other file is stale and is removed.
func (g *Gateway) restoreAdoptEndpoints() {
	dir := g.adoptEndpointDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			g.logEndpointError("adopt_endpoint_restore_failed", "", err)
		}
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".endpoint-") && strings.HasSuffix(name, ".tmp") {
			// A crash can leave a partial token file behind.
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		native := strings.TrimSuffix(name, ".json")
		path := filepath.Join(dir, name)
		if entry.Type()&os.ModeSymlink != 0 {
			_ = os.Remove(path)
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		endpoint, ok := g.readAdoptEndpoint(native)
		if !ok {
			_ = os.Remove(path)
			continue
		}
		record, found, err := g.store.AdoptedTopicByNative(native)
		if err != nil || !found || record.State != store.AdoptedOpen || record.Room == "" {
			_ = os.Remove(path)
			continue
		}
		current, found, err := g.store.CurrentSession(record.Room)
		if err != nil || !found || current.ID != record.Session {
			_ = os.Remove(path)
			continue
		}
		if !g.holderAlive(record) {
			_ = os.Remove(path)
			continue
		}
		if endpoint.HolderPID != record.HolderPID || endpoint.HolderStarted != record.HolderStarted {
			// The file was written by a different holder; the current holder
			// must re-register before its endpoint is trusted.
			_ = os.Remove(path)
			continue
		}
		g.setRestoredAdoptEndpoint(record.Room, endpoint)
		g.audit(record.Room, "adopt_endpoint_restored", core.Args{"native": native, "pid": record.HolderPID})
	}
}
