package gateway

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sean2077/feishu-bot/internal/store"
)

var storeTemplate struct {
	once sync.Once
	dir  string
	path string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if storeTemplate.dir != "" {
		_ = os.RemoveAll(storeTemplate.dir)
	}
	os.Exit(code)
}

// templateStorePath builds one fully initialized SQLite database. Fixtures
// copy it instead of paying the whole idempotent schema on every open, which
// dominated race-enabled CI time; the copied schema and data are identical
// (the template contains no rows).
func templateStorePath(t *testing.T) string {
	t.Helper()
	storeTemplate.once.Do(func() {
		dir, err := os.MkdirTemp("", "feishu-bot-store-template-")
		if err != nil {
			storeTemplate.err = err
			return
		}
		storeTemplate.dir = dir
		path := filepath.Join(dir, "template.sqlite")
		st, err := store.Open(path)
		if err == nil {
			_, err = st.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		}
		if err == nil {
			err = st.Close()
		} else {
			_ = st.Close()
		}
		if err != nil {
			storeTemplate.err = err
			return
		}
		storeTemplate.path = path
	})
	if storeTemplate.err != nil {
		t.Fatal(storeTemplate.err)
	}
	return storeTemplate.path
}

func copyStoreTemplate(t *testing.T, dst string) error {
	t.Helper()
	data, err := os.ReadFile(templateStorePath(t))
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0600)
}
