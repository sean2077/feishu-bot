package cli

import (
	"bytes"
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

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

const serviceName = "feishu-bot"

const daemonHelp = `Manage the feishu-bot background service.

Usage:
  feishu-bot daemon install [--config PATH] [--env-file PATH] [--work-dir DIR]
                            [--log-file PATH] [--log-max-size MB] [--log-max-backups N]
                            [--binary PATH] [--replace] [--no-start]
  feishu-bot daemon uninstall
  feishu-bot daemon start [--reclaim-stale-lock]
  feishu-bot daemon stop
  feishu-bot daemon restart
  feishu-bot daemon status [--json]
  feishu-bot daemon logs [-f|--follow] [-n N] [--log-file PATH]

install copies this executable into <work-dir>/bin, writes or validates
<work-dir>/service.env, runs the offline doctor checks, registers a logon
service (Windows Task Scheduler or Linux systemd), and starts it unless
--no-start is given. Credentials are never written into the service definition.
stop uses a control file and waits for gateway.lock; it never force-kills.
`

type serviceSpec struct {
	Name          string
	Dir           string
	Binary        string
	Config        string
	EnvFile       string
	LogFile       string
	LogMaxSize    int
	LogMaxBackups int
}

type serviceManager interface {
	Platform() string
	Check(ctx context.Context, spec serviceSpec, replace bool) error
	Install(ctx context.Context, spec serviceSpec, replace bool) error
	Uninstall(ctx context.Context, name string) error
	Start(ctx context.Context, name string) error
	Present(ctx context.Context, name string) (bool, error)
}

func newServiceManager() (serviceManager, error) {
	return platformServiceManager()
}

func defaultInstallPrecheck(ctx context.Context, c core.Config) error {
	report := RunDoctor(ctx, c, false, false)
	if report.Ready {
		return nil
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	return fmt.Errorf("installation precheck failed:\n%s", core.Redact(string(data), config.Secrets(c)))
}

// installPrecheck is replaceable in tests; production always uses the offline
// doctor checks and never sends a Feishu or model request.
var installPrecheck = defaultInstallPrecheck

func runDaemon(ctx context.Context, o Options, stdout, stderr io.Writer) error {
	switch o.Subcommand {
	case "install":
		mgr, err := newServiceManager()
		if err != nil {
			return err
		}
		return installWithManager(ctx, o, mgr, stdout, stderr)
	case "uninstall":
		return runDaemonUninstall(ctx, stdout)
	case "start":
		return runDaemonStart(ctx, o, stdout)
	case "stop":
		return runDaemonStop(ctx, stdout)
	case "restart":
		return runDaemonRestart(ctx, stdout)
	case "status":
		return runDaemonStatus(ctx, o, stdout)
	case "logs":
		return runDaemonLogs(ctx, o, stdout)
	}
	return errors.New("unknown daemon command")
}

func daemonMetaError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("service is not installed; run feishu-bot daemon install")
	}
	return err
}

func resolveConfigPath(o Options) (string, bool, error) {
	if o.ConfigExplicit {
		if _, err := os.Stat(o.ConfigPath); err == nil {
			return o.ConfigPath, true, nil
		}
		return o.ConfigPath, false, nil
	}
	if _, err := os.Stat(o.ConfigPath); err == nil {
		return o.ConfigPath, true, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return o.ConfigPath, false, nil
	}
	candidate := filepath.Join(home, ".feishu-bot", "config.json")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, true, nil
	}
	return o.ConfigPath, false, nil
}

func installWithManager(ctx context.Context, o Options, mgr serviceManager, stdout, stderr io.Writer) error {
	configPath, found, err := resolveConfigPath(o)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("configuration is missing; run feishu-bot init --config %s first", configPath)
	}
	c, err := config.Load(configPath)
	if err != nil {
		return err
	}
	workDir := o.WorkDir
	if workDir == "" {
		workDir = filepath.Dir(configPath)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return errors.New("invalid work directory")
	}
	if err := os.MkdirAll(workDir, 0700); err != nil {
		return errors.New("cannot create work directory")
	}
	envPath := filepath.Join(workDir, "service.env")
	values, commitEnv, err := prepareEnvFile(c, o.EnvFile, envPath)
	if err != nil {
		return err
	}
	for key, value := range values {
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("cannot apply environment variable %s", key)
		}
	}
	if err := installPrecheck(ctx, c); err != nil {
		return err
	}

	binarySource := o.Binary
	if binarySource == "" {
		binarySource, err = os.Executable()
		if err != nil {
			return errors.New("cannot locate the current executable")
		}
	}
	binaryPath := filepath.Join(workDir, "bin", executableName())
	logPath := o.LogFile
	if logPath == "" {
		logPath = filepath.Join(workDir, "logs", "service.log")
	}
	logPath, err = filepath.Abs(logPath)
	if err != nil {
		return errors.New("invalid log file path")
	}
	logSize := o.LogSize
	if logSize <= 0 {
		logSize = 10
	}
	logBackups := o.LogKeep
	if logBackups < 0 {
		logBackups = 0
	}
	spec := serviceSpec{Name: serviceName, Dir: workDir, Binary: binaryPath, Config: configPath, EnvFile: envPath, LogFile: logPath, LogMaxSize: logSize, LogMaxBackups: logBackups}

	// Check ownership before stopping or replacing anything.
	if err := mgr.Check(ctx, spec, o.Replace); err != nil {
		return err
	}
	// Only now replace service.env: a failed precheck or ownership check must
	// leave the installed service's environment untouched.
	if err := commitEnv(); err != nil {
		return err
	}

	oldMeta, oldErr := loadDaemonMeta()
	hadMeta := oldErr == nil
	dataDir := c.DataDir
	if hadMeta && oldMeta.DataDir != "" {
		dataDir = oldMeta.DataDir
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "gateway.lock")); statErr == nil {
		if _, err := stopRunning(ctx, dataDir); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return errors.New("cannot inspect gateway.lock")
	}

	if !samePath(binarySource, binaryPath) {
		if err := copyExecutable(binarySource, binaryPath); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return errors.New("cannot create log directory")
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return errors.New("log file is not writable")
	}
	logFile.Close()

	if err := mgr.Install(ctx, spec, o.Replace); err != nil {
		return err
	}
	meta := daemonMeta{
		Schema:        daemonMetaSchema,
		Version:       Version,
		Name:          serviceName,
		Platform:      mgr.Platform(),
		WorkDir:       workDir,
		BinaryPath:    binaryPath,
		ConfigPath:    configPath,
		EnvFile:       envPath,
		DataDir:       c.DataDir,
		LogFile:       logPath,
		LogMaxSize:    int64(logSize) * 1024 * 1024,
		LogMaxBackups: logBackups,
		InstalledAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := saveDaemonMeta(meta); err != nil {
		return err
	}

	started := false
	if !o.NoStart {
		if err := mgr.Start(ctx, serviceName); err != nil {
			return err
		}
		if err := waitForLock(ctx, c.DataDir, 30*time.Second); err != nil {
			return fmt.Errorf("service did not become ready: %w (see %s)", err, logPath)
		}
		started = true
	}
	status := "installed (not started)"
	if started {
		status = "running"
	}
	_, _ = fmt.Fprintf(stdout, "Installed %s\n  WorkDir: %s\n  Binary: %s\n  Config: %s\n  EnvFile: %s\n  LogFile: %s\n  Service: %s\n  Status: %s\n", serviceName, workDir, binaryPath, configPath, envPath, logPath, mgr.Platform(), status)
	return nil
}

// prepareEnvFile reads and validates the service environment. It returns a
// commit function that writes the target; callers run it only after every
// installation check has passed.
func prepareEnvFile(c core.Config, source, target string) (map[string]string, func() error, error) {
	var data []byte
	var err error
	writeTarget := false
	if source != "" {
		data, err = os.ReadFile(source)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot read environment file %s", source)
		}
		writeTarget = !samePath(source, target)
	} else if existing, readErr := os.ReadFile(target); readErr == nil {
		data = existing
	} else if errors.Is(readErr, os.ErrNotExist) {
		var missing []string
		data, missing, err = buildEnvFile(c)
		if err != nil {
			return nil, nil, err
		}
		if len(missing) > 0 {
			return nil, nil, fmt.Errorf("missing credentials: %s; provide --env-file with KEY=VALUE lines", strings.Join(missing, ", "))
		}
		writeTarget = true
	} else {
		return nil, nil, errors.New("cannot read environment file")
	}
	values, err := parseEnvFile(data)
	if err != nil {
		return nil, nil, err
	}
	if missing := missingCredentials(c, values); len(missing) > 0 {
		return nil, nil, fmt.Errorf("environment file is missing: %s", strings.Join(missing, ", "))
	}
	commit := func() error {
		if writeTarget {
			if err := writePrivate(target, data); err != nil {
				return err
			}
		}
		_ = os.Chmod(target, 0600)
		return nil
	}
	return values, commit, nil
}

func writePrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("cannot create environment directory")
	}
	tmp, err := os.CreateTemp(dir, ".feishu-bot-env-*")
	if err != nil {
		return errors.New("cannot write environment file")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return errors.New("cannot secure environment file")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("cannot write environment file")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("cannot flush environment file")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("cannot close environment file")
	}
	if err := os.Rename(name, path); err != nil {
		return errors.New("cannot replace environment file")
	}
	return nil
}

func copyExecutable(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return errors.New("cannot read the current executable")
	}
	defer in.Close()
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("cannot create binary directory")
	}
	tmp, err := os.CreateTemp(dir, ".feishu-bot-bin-*")
	if err != nil {
		return errors.New("cannot create installed binary")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return errors.New("cannot copy the executable")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("cannot flush the executable")
	}
	if err := tmp.Chmod(0755); err != nil {
		tmp.Close()
		return errors.New("cannot mark the executable")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("cannot close the executable")
	}
	if err := os.Rename(name, target); err != nil {
		return errors.New("cannot replace the installed binary; stop the service first")
	}
	return nil
}

func samePath(a, b string) bool {
	aa, err := filepath.Abs(a)
	if err != nil {
		aa = filepath.Clean(a)
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		bb = filepath.Clean(b)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}

func executableName() string {
	if runtime.GOOS == "windows" {
		return "feishu-bot.exe"
	}
	return "feishu-bot"
}

func waitForLock(ctx context.Context, dataDir string, timeout time.Duration) error {
	path := filepath.Join(dataDir, "gateway.lock")
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for gateway.lock")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func runDaemonStart(ctx context.Context, o Options, stdout io.Writer) error {
	meta, err := loadDaemonMeta()
	if err != nil {
		return daemonMetaError(err)
	}
	lockPath := filepath.Join(meta.DataDir, "gateway.lock")
	if _, statErr := os.Stat(lockPath); statErr == nil {
		lock, err := workspace.ReadLock(meta.DataDir)
		if err != nil {
			return errors.New("gateway.lock is unreadable; inspect it before starting")
		}
		if process.Alive(lock.PID) {
			_, _ = fmt.Fprintf(stdout, "%s is already running (PID %d).\n", meta.Name, lock.PID)
			return nil
		}
		if !o.Reclaim {
			return fmt.Errorf("gateway.lock records PID %d, which is not running; inspect it or use --reclaim-stale-lock", lock.PID)
		}
		if err := reclaimStaleLock(meta.DataDir); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return errors.New("cannot read gateway.lock")
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	present, err := mgr.Present(ctx, meta.Name)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("service definition %s is missing; re-run feishu-bot daemon install", meta.Name)
	}
	if err := mgr.Start(ctx, meta.Name); err != nil {
		return err
	}
	if err := waitForLock(ctx, meta.DataDir, 30*time.Second); err != nil {
		return fmt.Errorf("service did not become ready: %w (see %s)", err, meta.LogFile)
	}
	_, _ = fmt.Fprintf(stdout, "Started %s.\n", meta.Name)
	return nil
}

func runDaemonStop(ctx context.Context, stdout io.Writer) error {
	meta, err := loadDaemonMeta()
	if err != nil {
		return daemonMetaError(err)
	}
	already, err := stopRunning(ctx, meta.DataDir)
	if err != nil {
		return err
	}
	if already {
		_, _ = fmt.Fprintln(stdout, "Service is not running.")
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "Stopped %s.\n", meta.Name)
	return nil
}

func runDaemonRestart(ctx context.Context, stdout io.Writer) error {
	meta, err := loadDaemonMeta()
	if err != nil {
		return daemonMetaError(err)
	}
	if _, err := stopRunning(ctx, meta.DataDir); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	present, err := mgr.Present(ctx, meta.Name)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("service definition %s is missing; re-run feishu-bot daemon install", meta.Name)
	}
	if err := mgr.Start(ctx, meta.Name); err != nil {
		return err
	}
	if err := waitForLock(ctx, meta.DataDir, 30*time.Second); err != nil {
		return fmt.Errorf("service did not become ready: %w (see %s)", err, meta.LogFile)
	}
	_, _ = fmt.Fprintf(stdout, "Restarted %s.\n", meta.Name)
	return nil
}

type daemonStatus struct {
	Installed      bool   `json:"installed"`
	Running        bool   `json:"running"`
	PID            int    `json:"pid,omitempty"`
	StartedAt      int64  `json:"startedAt,omitempty"`
	StaleLock      bool   `json:"staleLock,omitempty"`
	StopRequested  bool   `json:"stopRequested"`
	ServicePresent bool   `json:"servicePresent"`
	Platform       string `json:"platform,omitempty"`
	WorkDir        string `json:"workDir,omitempty"`
	Config         string `json:"config,omitempty"`
	EnvFile        string `json:"envFile,omitempty"`
	LogFile        string `json:"logFile,omitempty"`
	InstalledAt    string `json:"installedAt,omitempty"`
	Version        string `json:"version,omitempty"`
	Error          string `json:"error,omitempty"`
}

func runDaemonStatus(ctx context.Context, o Options, stdout io.Writer) error {
	var report daemonStatus
	meta, err := loadDaemonMeta()
	if err == nil {
		report.Installed = true
		report.Platform = meta.Platform
		report.WorkDir = meta.WorkDir
		report.Config = meta.ConfigPath
		report.EnvFile = meta.EnvFile
		report.LogFile = meta.LogFile
		report.InstalledAt = meta.InstalledAt
		report.Version = meta.Version
		if info, statErr := os.Stat(stopRequestPath(meta.DataDir)); statErr == nil && info.Mode().IsRegular() {
			report.StopRequested = true
		}
		lock, lockErr := workspace.ReadLock(meta.DataDir)
		if lockErr == nil {
			report.PID = lock.PID
			report.StartedAt = lock.StartedAt
			if process.Alive(lock.PID) {
				report.Running = true
			} else {
				report.StaleLock = true
			}
		} else if !errors.Is(lockErr, os.ErrNotExist) {
			report.Error = "gateway.lock is unreadable"
		}
		if mgr, mgrErr := newServiceManager(); mgrErr == nil {
			if present, presentErr := mgr.Present(ctx, meta.Name); presentErr == nil {
				report.ServicePresent = present
			} else {
				report.Error = "service definition could not be checked"
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if mgr, mgrErr := newServiceManager(); mgrErr == nil {
		if present, presentErr := mgr.Present(ctx, serviceName); presentErr == nil {
			report.ServicePresent = present
		}
	}
	if o.JSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	_, _ = fmt.Fprintln(stdout, "feishu-bot daemon status")
	_, _ = fmt.Fprintln(stdout)
	status := "Not installed"
	if report.Installed {
		switch {
		case report.Running && report.StopRequested:
			status = fmt.Sprintf("Stopping (PID %d)", report.PID)
		case report.Running:
			status = fmt.Sprintf("Running (PID %d)", report.PID)
		case report.StaleLock:
			status = fmt.Sprintf("Stale lock (PID %d)", report.PID)
		case report.StopRequested:
			status = "Stopping"
		default:
			status = "Stopped"
		}
	} else if report.ServicePresent {
		status = "Service definition without daemon metadata"
	}
	_, _ = fmt.Fprintf(stdout, "  Status:    %s\n", status)
	if report.Installed {
		_, _ = fmt.Fprintf(stdout, "  Platform:  %s\n  Config:    %s\n  EnvFile:   %s\n  LogFile:   %s\n  WorkDir:   %s\n  Installed: %s\n  Version:   %s\n",
			report.Platform, report.Config, report.EnvFile, report.LogFile, report.WorkDir, report.InstalledAt, report.Version)
	}
	if report.Installed && !report.ServicePresent {
		_, _ = fmt.Fprintln(stdout, "  Warning:   service definition is missing; re-run feishu-bot daemon install")
	}
	if !report.Installed && report.ServicePresent {
		_, _ = fmt.Fprintln(stdout, "  Warning:   a service definition exists without daemon metadata; re-run daemon install or inspect it")
	}
	if report.StaleLock {
		_, _ = fmt.Fprintln(stdout, "  Hint:      verify no native descendants remain, then run feishu-bot daemon start --reclaim-stale-lock")
	}
	if report.Error != "" {
		_, _ = fmt.Fprintf(stdout, "  Warning:   %s\n", report.Error)
	}
	return nil
}

func runDaemonLogs(ctx context.Context, o Options, stdout io.Writer) error {
	path := o.LogFile
	if path == "" {
		meta, err := loadDaemonMeta()
		if err != nil {
			return daemonMetaError(err)
		}
		path = meta.LogFile
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("no log file is configured")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("log file is not available: %s", path)
	}
	lines, err := tailLines(path, o.LogLines)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return err
		}
	}
	if !o.Follow {
		return nil
	}
	return followLog(ctx, path, stdout)
}

func runDaemonUninstall(ctx context.Context, stdout io.Writer) error {
	meta, err := loadDaemonMeta()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if mgr, mgrErr := newServiceManager(); mgrErr == nil {
				if present, _ := mgr.Present(ctx, serviceName); present {
					return errors.New("a feishu-bot service exists without daemon metadata; verify it before removing")
				}
			}
			_, _ = fmt.Fprintln(stdout, "Service is not installed.")
			return nil
		}
		return err
	}
	if _, err := os.Stat(filepath.Join(meta.DataDir, "gateway.lock")); err == nil {
		if _, err := stopRunning(ctx, meta.DataDir); err != nil {
			return err
		}
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	if err := mgr.Uninstall(ctx, meta.Name); err != nil {
		return err
	}
	path, err := daemonMetaPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot remove daemon metadata")
	}
	_, _ = fmt.Fprintf(stdout, "Uninstalled %s. Configuration, service.env, logs and state were kept.\n", meta.Name)
	return nil
}

func tailLines(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open log file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New("cannot inspect log file")
	}
	size := info.Size()
	if size == 0 {
		return nil, nil
	}
	const chunk = 64 << 10
	var data []byte
	for pos := size; pos > 0; {
		start := pos - chunk
		if start < 0 {
			start = 0
		}
		buf := make([]byte, pos-start)
		if _, err := file.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, errors.New("cannot read log file")
		}
		data = append(buf, data...)
		pos = start
		if bytes.Count(data, []byte{'\n'}) > n {
			break
		}
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func followLog(ctx context.Context, path string, out io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.New("cannot open log file")
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return errors.New("cannot follow log file")
	}
	var offset int64
	offset, _ = file.Seek(0, io.SeekCurrent)
	buf := make([]byte, 32<<10)
	for {
		n, readErr := file.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				return err
			}
			offset += int64(n)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return errors.New("cannot read log file")
		}
		current, statErr := os.Stat(path)
		info, infoErr := file.Stat()
		if statErr == nil && infoErr == nil && !os.SameFile(info, current) {
			_ = file.Close()
			file, err = os.Open(path)
			if err != nil {
				return errors.New("log file was rotated and could not be reopened")
			}
			offset = 0
			continue
		}
		if statErr == nil && current.Size() < offset {
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return errors.New("cannot rewind truncated log file")
			}
			offset = 0
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

type toolError struct {
	Name   string
	Code   int
	Output string
}

func (e *toolError) Error() string {
	return fmt.Sprintf("%s failed (exit %d)", e.Name, e.Code)
}

func toolDetail(err error) string {
	var tool *toolError
	if errors.As(err, &tool) {
		detail := strings.TrimSpace(tool.Output)
		if len(detail) > 2000 {
			detail = detail[:2000]
		}
		return detail
	}
	return ""
}

type boundedOutput struct {
	buf bytes.Buffer
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	const limit = 64 << 10
	if b.buf.Len() < limit {
		remaining := limit - b.buf.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return n, nil
}

func (b *boundedOutput) String() string { return b.buf.String() }

func runTool(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := process.Run(ctx, cmd)
	if err != nil {
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		return out.String(), &toolError{Name: name, Code: code, Output: out.String()}
	}
	return out.String(), nil
}
