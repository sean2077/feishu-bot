package cli

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRotatingWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	writer, err := NewRotatingWriter(path, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{"1234", "5678", "abcd", "efgh", "ijkl", "mnop", "qrst"} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got := read(path); got != "qrst" {
		t.Fatalf("current log = %q", got)
	}
	if got := read(path + ".1"); got != "ijklmnop" {
		t.Fatalf("backup .1 = %q", got)
	}
	if got := read(path + ".2"); got != "abcdefgh" {
		t.Fatalf("backup .2 = %q", got)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("unexpected third backup")
	}
}

func TestRotatingWriterZeroBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	writer, err := NewRotatingWriter(path, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("efgh")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "efgh" {
		t.Fatalf("current log = %q", data)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("unexpected backup with zero backup count")
	}
}

func TestRotatingWriterConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	writer, err := NewRotatingWriter(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := 0; j < 64; j++ {
				if _, err := writer.Write([]byte("line\n")); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	group.Wait()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(8*64*5) {
		t.Fatalf("log size = %d", info.Size())
	}
}
