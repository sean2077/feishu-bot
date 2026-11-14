package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func adoptFixture(t *testing.T) (map[string]any, string) {
	t.Helper()
	v := fixture(t)
	v["projects"].([]any)[0].(map[string]any)["nativeConfig"] = "inherit"
	base := t.TempDir()
	return v, base
}

func parseAdoptFixture(t *testing.T, v map[string]any, base string) error {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse(data, base)
	return err
}

func TestAdoptRootsResolveAndEnableAdoption(t *testing.T) {
	v, base := adoptFixture(t)
	root := filepath.Join(base, "adopt")
	if err := os.MkdirAll(filepath.Join(root, "project"), 0700); err != nil {
		t.Fatal(err)
	}
	v["dms"].([]any)[0].(map[string]any)["adoptRoots"] = []any{"adopt"}
	if err := parseAdoptFixture(t, v, base); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(v)
	c, err := Parse(data, base)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.DMs[0].AdoptRoots) != 1 || c.DMs[0].AdoptRoots[0] != want {
		t.Fatalf("adoptRoots = %v, want %q", c.DMs[0].AdoptRoots, want)
	}
	if !c.AdoptEnabled("ou_example_owner") {
		t.Fatal("adoption was not enabled")
	}
	if !c.AdoptWorkAllowed("ou_example_owner", filepath.Join(want, "project")) {
		t.Fatal("descendant work directory was not allowed")
	}
	if _, ok := c.AdoptedTopicRoom("oc_example_dm", "ou_example_owner", "om_root"); !ok {
		t.Fatal("configured DM did not produce an adopted topic room")
	}
}

func TestAdoptRootsRequireInheritedProject(t *testing.T) {
	v := fixture(t)
	root := t.TempDir()
	v["dms"].([]any)[0].(map[string]any)["adoptRoots"] = []any{root}
	err := parseAdoptFixture(t, v, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "nativeConfig inherit") {
		t.Fatalf("isolated project accepted adoptRoots: %v", err)
	}
}

func TestAdoptRootsMustExistAndNotOverlapState(t *testing.T) {
	v, base := adoptFixture(t)
	v["dms"].([]any)[0].(map[string]any)["adoptRoots"] = []any{filepath.Join(base, "missing")}
	err := parseAdoptFixture(t, v, base)
	if err == nil || !strings.Contains(err.Error(), "existing directory") {
		t.Fatalf("missing root accepted: %v", err)
	}
	overlap := filepath.Join(base, "state", "rooms", "nested")
	if e := os.MkdirAll(overlap, 0700); e != nil {
		t.Fatal(e)
	}
	v["dms"].([]any)[0].(map[string]any)["adoptRoots"] = []any{overlap}
	err = parseAdoptFixture(t, v, base)
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("state overlap accepted: %v", err)
	}
	v["dms"].([]any)[0].(map[string]any)["adoptRoots"] = []any{filepath.Join(base, "state")}
	err = parseAdoptFixture(t, v, base)
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("state ancestor accepted: %v", err)
	}
}

func TestAdoptRootsRejectEmptyAndDuplicateEntries(t *testing.T) {
	v, base := adoptFixture(t)
	root := filepath.Join(base, "adopt")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	v["dms"].([]any)[0].(map[string]any)["adoptRoots"] = []any{""}
	if err := parseAdoptFixture(t, v, base); err == nil {
		t.Fatal("empty adopt root accepted")
	}
	v["dms"].([]any)[0].(map[string]any)["adoptRoots"] = []any{root, root}
	if err := parseAdoptFixture(t, v, base); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("duplicate adopt roots accepted: %v", err)
	}
}
