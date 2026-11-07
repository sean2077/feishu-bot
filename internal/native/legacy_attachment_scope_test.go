package native

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChatScopeNeverClaimsLegacyTopicUploads(t *testing.T) {
	for _, mode := range []string{"isolated", "inherit"} {
		w, _, _ := windowsScopeFixture(t, mode)
		if _, err := w.fileScope(); err != nil {
			t.Fatal(err)
		}
		legacy := filepath.Join(w.workspace.Inbound, "topics", "topic-a")
		if err := os.MkdirAll(legacy, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacy, "secret.txt"), []byte("topic upload"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			tool  string
			input args
		}{
			{"Read", args{"file_path": filepath.Join(legacy, "secret.txt")}},
			{"Grep", args{"path": legacy, "pattern": "topic"}},
			{"Glob", args{"path": legacy, "pattern": "*"}},
			{"Grep", args{"path": w.workspace.Inbound, "pattern": "topic"}},
			{"Glob", args{"path": w.workspace.Inbound, "pattern": "**/*"}},
		} {
			if !hookDenied(t, w, tc.tool, tc.input, "") {
				t.Fatalf("%s %s claimed legacy topic uploads", mode, tc.tool)
			}
		}
		fresh := &worker{workspace: w.workspace}
		if _, err := fresh.fileScope(); err == nil {
			t.Fatal("fresh native scope accepted an incomplete topic migration")
		}
	}
}
