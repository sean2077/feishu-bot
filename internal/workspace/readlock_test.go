package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadLock(t *testing.T) {
	dir := t.TempDir()
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := ReadLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.PID != os.Getpid() || info.Token == "" || info.StartedAt == 0 {
		t.Fatalf("unexpected lock info: %#v", info)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLock(dir); !os.IsNotExist(err) {
		t.Fatal("released lock was still readable")
	}
}

func TestReadLockRejectsInvalidContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.lock")
	if err := os.WriteFile(path, []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLock(dir); err == nil {
		t.Fatal("accepted invalid lock JSON")
	}
	if err := os.WriteFile(path, []byte(`{"pid":0,"token":"x","startedAt":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLock(dir); err == nil {
		t.Fatal("accepted a lock without a valid PID")
	}
}
