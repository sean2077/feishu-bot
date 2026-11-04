// Package workspace maintains stable conversation directories and bounded file access.
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

var excluded = map[string]bool{".git": true, "node_modules": true, ".env": true, ".ssh": true, ".aws": true, ".azure": true, ".npmrc": true, ".pypirc": true, ".netrc": true, ".git-credentials": true, "state": true, "dist": true, ".claude": true}

type seedLimit struct {
	files int
	bytes int64
}

func seedTree(source, target string, limit *seedLimit) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := strings.ToLower(entry.Name())
		if excluded[name] || strings.HasPrefix(name, ".env.") {
			continue
		}
		src, dst := filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())
		st, err := os.Lstat(src)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if st.IsDir() {
			if err = os.MkdirAll(dst, 0700); err != nil {
				return err
			}
			if err = seedTree(src, dst, limit); err != nil {
				return err
			}
		} else if st.Mode().IsRegular() {
			limit.files++
			limit.bytes += st.Size()
			if limit.files > 20000 || limit.bytes > 512<<20 {
				return errors.New("workspace seed exceeds limit")
			}
			if err = copyFile(src, dst, st.Mode().Perm(), st.Size()); err != nil {
				return err
			}
		}
	}
	return nil
}
func copyFile(source, target string, mode os.FileMode, limit int64) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("source grew beyond file limit")
	}
	return closeErr
}
func inherited(w core.Workspace) (core.Workspace, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return w, err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return w, err
	}
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	override := strings.TrimSpace(dir) != ""
	if !override {
		dir = filepath.Join(home, ".claude")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return w, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return w, errors.New("Claude config directory must exist for inherited configuration")
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return w, errors.New("Claude config directory must be a directory")
	}
	w.Home = home
	w.ConfigDir = dir
	w.NativeConfig = "inherit"
	w.ConfigDirOverride = override
	return w, nil
}
func Prepare(c core.Config, room core.Room, _ core.Session, p core.Project) (core.Workspace, error) {
	root := filepath.Join(c.DataDir, "rooms", core.Hash(room.Key)[:32])
	if err := os.MkdirAll(root, 0700); err != nil {
		return core.Workspace{}, err
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return core.Workspace{}, err
	}
	w := core.Workspace{Root: root, Work: filepath.Join(root, "workspace"), Home: filepath.Join(root, "home"), Inbound: filepath.Join(root, "inbound"), NativeConfig: "isolated", ConfigDirOverride: true}
	if p.CWD != "" {
		work, err := filepath.EvalSymlinks(p.CWD)
		if err != nil {
			return w, err
		}
		st, err := os.Stat(work)
		if err != nil || !st.IsDir() {
			return w, errors.New("project cwd must be an existing directory")
		}
		if core.Inside(root, work) || core.Inside(work, root) {
			return w, errors.New("project cwd and conversation state must not overlap")
		}
		w.Work = work
	}
	for _, dir := range []string{w.Work, w.Home, w.Inbound} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return w, err
		}
	}
	w.ConfigDir = filepath.Join(w.Home, ".claude")
	if p.CWD == "" {
		marker := filepath.Join(root, "seeded")
		_, markerErr := os.Stat(marker)
		if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
			return w, markerErr
		}
		if errors.Is(markerErr, os.ErrNotExist) {
			if p.SeedDir != "" {
				source, e := filepath.EvalSymlinks(p.SeedDir)
				if e != nil {
					return w, e
				}
				if core.Inside(source, root) || core.Inside(root, source) {
					return w, errors.New("seedDir and state directory must not overlap")
				}
				if e = seedTree(source, w.Work, &seedLimit{}); e != nil {
					return w, e
				}
			}
			if p.TrustedConfigDir != "" {
				source, e := filepath.EvalSymlinks(p.TrustedConfigDir)
				if e != nil {
					return w, e
				}
				if core.Inside(source, root) || core.Inside(root, source) {
					return w, errors.New("trustedConfigDir and state directory must not overlap")
				}
				local := filepath.Join(w.Work, ".claude")
				if e = os.MkdirAll(local, 0700); e != nil {
					return w, e
				}
				if e = seedTree(source, local, &seedLimit{}); e != nil {
					return w, e
				}
			}
			if p.SeedDir != "" {
				if err = initGit(w); err != nil {
					return w, err
				}
			}
			if err = os.WriteFile(marker, []byte("1\n"), 0600); err != nil {
				return w, err
			}
		}
	}
	if p.NativeConfig == "inherit" {
		return inherited(w)
	}
	return w, nil
}
func initGit(w core.Workspace) error {
	template := filepath.Join(w.Root, "empty-git-template")
	if err := os.MkdirAll(template, 0700); err != nil {
		return err
	}
	null := "/dev/null"
	if runtime.GOOS == "windows" {
		null = "NUL"
	}
	env := []string{}
	for _, key := range []string{"PATH", "SystemRoot", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	env = append(env, "HOME="+w.Root, "USERPROFILE="+w.Root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+null, "GIT_TEMPLATE_DIR="+template)
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", "-A"}, {"-c", "core.hooksPath=" + template, "-c", "commit.gpgSign=false", "-c", "user.name=Feishu Bot", "-c", "user.email=feishu-bot@example.invalid", "commit", "--allow-empty", "-m", "Project source snapshot"}} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = w.Work
		cmd.Env = env
		err := cmd.Run()
		cancel()
		if err != nil {
			return errors.New("cannot initialize project source baseline")
		}
	}
	return nil
}
func TranslateFile(w core.Workspace, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(w.Work, path)
	}
	path = filepath.Clean(path)
	if !core.Inside(w.Work, path) {
		return "", errors.New("file outside worker workspace")
	}
	return path, nil
}
func TranslateOutput(w core.Workspace, path string) (string, string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(w.Work, path)
	}
	path = filepath.Clean(path)
	for _, root := range []string{w.Work, w.Inbound} {
		if root == "" || !core.Inside(root, path) {
			continue
		}
		if root == w.Inbound {
			actual, err := filepath.EvalSymlinks(root)
			expected := filepath.Join(w.Root, "inbound")
			if err != nil || !core.Inside(expected, actual) || !core.Inside(actual, expected) {
				return "", "", errors.New("conversation attachment directory was redirected")
			}
		}
		return root, path, nil
	}
	return "", "", errors.New("file outside workspace or current conversation attachments")
}

// Snapshot opens the source through an os.Root, preventing symlink traversal out
// of the authorized directory, and streams to a private, bounded export file.
func Snapshot(root, path, target string, limit int64) error {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if !core.Inside(root, path) {
		return errors.New("file outside allowed workspace")
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	in, err := r.Open(rel)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return errors.New("file exceeds export policy")
	}
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(target)
		}
	}()
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("file exceeds export policy")
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

// Lock fails closed for live and stale locks. Release is called only after all
// native descendants have been confirmed stopped.
func Lock(dir string) (func() error, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "gateway.lock")
	token := core.NewID()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("data directory is locked; verify its recorded PID before removing a stale gateway.lock")
	}
	err = json.NewEncoder(f).Encode(core.Args{"pid": os.Getpid(), "token": token, "startedAt": time.Now().UnixMilli()})
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	return func() error {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var v struct {
			Token string `json:"token"`
		}
		if err = json.Unmarshal(data, &v); err != nil {
			return err
		}
		if v.Token != token {
			return errors.New("data directory lock ownership changed")
		}
		return os.Remove(path)
	}, nil
}
