package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type artifactIdentity struct {
	Path, Version, Commit, SHA256 string
}

func readArtifactIdentity(t *testing.T, prefix string) artifactIdentity {
	t.Helper()
	identity := artifactIdentity{Path: os.Getenv(prefix + "_PATH"), Version: os.Getenv(prefix + "_VERSION"), Commit: os.Getenv(prefix + "_COMMIT"), SHA256: os.Getenv(prefix + "_SHA256")}
	if identity.Path == "" {
		configured := prefix != "FEISHU_ARTIFACT"
		for _, candidate := range []string{"FEISHU_ARTIFACT", "FEISHU_PREVIOUS_ARTIFACT"} {
			for _, field := range []string{"PATH", "VERSION", "COMMIT", "SHA256", "BUILDINFO"} {
				configured = configured || os.Getenv(candidate+"_"+field) != ""
			}
		}
		if configured {
			t.Fatal("explicit artifact journey is missing " + prefix + "_PATH")
		}
		t.Skip("set FEISHU_ARTIFACT_PATH and its identities to exercise the actual artifact")
	}
	manifest := os.Getenv(prefix + "_BUILDINFO")
	commit, commitErr := hex.DecodeString(identity.Commit)
	digest, digestErr := hex.DecodeString(identity.SHA256)
	if identity.Version == "" || len(commit) != 20 || commitErr != nil || len(digest) != sha256.Size || digestErr != nil || manifest == "" {
		t.Fatal("artifact requires exact version, commit, SHA256 and BUILDINFO identity", prefix)
	}
	data, err := os.ReadFile(manifest)
	var info struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		SHA256  string `json:"binary_sha256"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
	}
	if err != nil || len(data) > 64<<10 || json.Unmarshal(data, &info) != nil || info.Version != identity.Version || info.Commit != identity.Commit || info.SHA256 != identity.SHA256 || info.OS != runtime.GOOS || info.Arch != runtime.GOARCH {
		t.Fatal("artifact BUILDINFO differs from expected source, version, hash or native platform", err)
	}
	file, err := os.Open(identity.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		t.Fatal("artifact is not a regular executable", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(identity.SHA256) {
		t.Fatal("actual executable checksum differs from verified BUILDINFO")
	}
	t.Logf("Actual artifact identity: file=%s version=%s commit=%s sha256=%s", filepath.Base(identity.Path), identity.Version, identity.Commit, identity.SHA256)
	return identity
}

func installArtifactBinary(t *testing.T, identity artifactIdentity, destination string) {
	t.Helper()
	input, err := os.Open(identity.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	pending := destination + ".pending"
	output, err := os.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(pending)
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil || hex.EncodeToString(hash.Sum(nil)) != identity.SHA256 {
		t.Fatal("installed fixture binary differs from verified artifact", copyErr, closeErr)
	}
	// Windows cannot rename over an existing executable. The previous service
	// is already confirmed stopped before replacement in the upgrade journey.
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Rename(pending, destination); err != nil {
		t.Fatal(err)
	}
}
