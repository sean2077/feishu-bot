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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
)

var excluded = map[string]bool{".git": true, "node_modules": true, ".env": true, ".ssh": true, ".aws": true, ".azure": true, ".npmrc": true, ".pypirc": true, ".netrc": true, ".git-credentials": true, "state": true, "dist": true, ".claude": true}

type seedLimit struct {
	files int
	bytes int64
}

func seedTree(source, target string, limit *seedLimit) error {
	return seedTreeContext(context.Background(), source, target, limit)
}
func seedTreeContext(ctx context.Context, source, target string, limit *seedLimit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	in, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer out.Close()
	return seedDirectory(ctx, in, out, ".", limit, 0)
}
func seedDirectory(ctx context.Context, source, target *os.Root, path string, limit *seedLimit, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 256 {
		return errors.New("workspace seed directory nesting exceeds limit")
	}
	dir, err := source.Open(path)
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.ToLower(entry.Name())
		if excluded[name] || strings.HasPrefix(name, ".env.") {
			continue
		}
		relative := filepath.Join(path, entry.Name())
		st, err := source.Lstat(relative)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if st.IsDir() {
			if err = target.MkdirAll(relative, 0700); err != nil {
				return err
			}
			if err = seedDirectory(ctx, source, target, relative, limit, depth+1); err != nil {
				return err
			}
		} else if st.Mode().IsRegular() {
			limit.files++
			limit.bytes += st.Size()
			if limit.files > 20000 || limit.bytes > 512<<20 {
				return errors.New("workspace seed exceeds limit")
			}
			if err = copySeedFile(ctx, source, target, relative, st.Mode().Perm(), st.Size()); err != nil {
				return err
			}
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func copySeedFile(ctx context.Context, source, target *os.Root, path string, mode os.FileMode, limit int64) error {
	in, err := source.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("seed source is no longer a regular file")
	}
	// Both roots stay bound to open directory handles. Existing destination links
	// and source replacements cannot redirect a retry outside those directories.
	out, err := target.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, contextReader{ctx: ctx, reader: io.LimitReader(in, limit+1)})
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("source grew beyond file limit")
	}
	return closeErr
}
func physicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}
func ownDirectory(path, expected string) error {
	actual, err := physicalDirectory(path)
	if err != nil {
		return err
	}
	if !core.Inside(expected, actual) || !core.Inside(actual, expected) {
		return errors.New("conversation storage directory was redirected")
	}
	return nil
}

func inherited(w core.Workspace) (core.Workspace, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return w, err
	}
	home, err = physicalDirectory(home)
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
func Prepare(c core.Config, room core.Room, session core.Session, p core.Project) (core.Workspace, error) {
	return PrepareContext(context.Background(), c, room, session, p)
}

// PrepareContext preserves the durable room layout while allowing shutdown to
// cancel seed copies and Git initialization before a native session is started.
func PrepareContext(ctx context.Context, c core.Config, room core.Room, _ core.Session, p core.Project) (core.Workspace, error) {
	if err := ctx.Err(); err != nil {
		return core.Workspace{}, err
	}
	rooms := filepath.Join(c.DataDir, "rooms")
	if err := os.MkdirAll(rooms, 0700); err != nil {
		return core.Workspace{}, err
	}
	rooms, err := physicalDirectory(rooms)
	if err != nil {
		return core.Workspace{}, err
	}
	root := filepath.Join(rooms, core.Hash(room.Key)[:32])
	if err = os.MkdirAll(root, 0700); err != nil {
		return core.Workspace{}, err
	}
	if err = ownDirectory(root, root); err != nil {
		return core.Workspace{}, err
	}
	w := core.Workspace{Root: root, Work: filepath.Join(root, "workspace"), Home: filepath.Join(root, "home"), Inbound: filepath.Join(root, "inbound"), NativeConfig: "isolated", ConfigDirOverride: true}
	if p.CWD != "" {
		work, err := physicalDirectory(p.CWD)
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
		if err = ctx.Err(); err != nil {
			return w, err
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			return w, err
		}
		if dir != w.Work || p.CWD == "" {
			if err = ownDirectory(dir, dir); err != nil {
				return w, err
			}
		}
	}
	w.ConfigDir = filepath.Join(w.Home, ".claude")
	if p.CWD == "" {
		marker := filepath.Join(root, "seeded")
		markerInfo, markerErr := os.Lstat(marker)
		if markerErr == nil && !markerInfo.Mode().IsRegular() {
			return w, errors.New("invalid workspace seed marker")
		}

		if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
			return w, markerErr
		}
		if errors.Is(markerErr, os.ErrNotExist) {
			if p.SeedDir != "" {
				source, e := physicalDirectory(p.SeedDir)
				if e != nil {
					return w, e
				}
				if core.Inside(source, root) || core.Inside(root, source) {
					return w, errors.New("seedDir and state directory must not overlap")
				}
				if e = seedTreeContext(ctx, source, w.Work, &seedLimit{}); e != nil {
					return w, e
				}
			}
			if p.TrustedConfigDir != "" {
				source, e := physicalDirectory(p.TrustedConfigDir)
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
				if e = ownDirectory(local, local); e != nil {
					return w, e
				}
				if e = seedTreeContext(ctx, source, local, &seedLimit{}); e != nil {
					return w, e
				}
			}
			if p.SeedDir != "" {
				if err = initGit(ctx, w); err != nil {
					return w, err
				}
			}
			if err = ctx.Err(); err != nil {
				return w, err
			}
			markerFile, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return w, err
			}
			_, writeErr := markerFile.WriteString("1\n")
			closeErr := markerFile.Close()
			if writeErr != nil {
				return w, writeErr
			}
			if closeErr != nil {
				return w, closeErr
			}
		}
	}
	if err = ctx.Err(); err != nil {
		return w, err
	}
	if p.NativeConfig == "inherit" {
		return inherited(w)
	}
	return w, nil
}
func initGit(parent context.Context, w core.Workspace) error {
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
		ctx, cancel := context.WithTimeout(parent, time.Minute)
		cmd := exec.Command("git", args...)
		cmd.Dir = w.Work
		cmd.Env = env
		err := process.Run(ctx, cmd)
		cancel()
		if err != nil {
			if errors.Is(err, process.ErrUnconfirmed) || parent.Err() != nil {
				return err
			}
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

type LockInfo struct {
	PID       int    `json:"pid"`
	Token     string `json:"token"`
	StartedAt int64  `json:"startedAt"`
}

// ReadLock inspects gateway.lock without removing or modifying it.
func ReadLock(dir string) (LockInfo, error) {
	data, err := os.ReadFile(filepath.Join(dir, "gateway.lock"))
	if err != nil {
		return LockInfo{}, err
	}
	var info LockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return LockInfo{}, err
	}
	if info.PID <= 0 {
		return LockInfo{}, errors.New("gateway.lock has no valid PID")
	}
	return info, nil
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
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("data directory is locked; verify its recorded PID before removing a stale gateway.lock")
		}
		return nil, fmt.Errorf("cannot acquire data directory lock: %w", err)
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
	var releaseOnce sync.Once
	var releaseErr error
	release := func() error {
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
	}
	return func() error { releaseOnce.Do(func() { releaseErr = release() }); return releaseErr }, nil
}
