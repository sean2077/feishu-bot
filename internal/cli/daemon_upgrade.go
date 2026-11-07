package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// serviceDefinition is kept privately with the file backups until installation
// commits. It contains the exact prior definition, including foreign definitions
// that the administrator explicitly chose to replace.
type serviceDefinition struct {
	Present    bool        `json:"present"`
	Data       []byte      `json:"data,omitempty"`
	Mode       os.FileMode `json:"mode,omitempty"`
	Enablement string      `json:"enablement,omitempty"`
}

type installFileBackup struct {
	Target  string      `json:"target"`
	Backup  string      `json:"backup,omitempty"`
	Mode    os.FileMode `json:"mode"`
	Present bool        `json:"present"`
}

type installBackup struct {
	Directory  string              `json:"-"`
	Service    serviceDefinition   `json:"service"`
	Files      []installFileBackup `json:"files"`
	changed    bool
	committed  bool
	wasRunning bool
}

// stageExecutable does all source reads, validation and disk allocation before
// the running installation is stopped. Publishing the result only needs rename.
func stageExecutable(source, target string) (string, error) {
	return stageInstallFile(source, target, 0755, true)
}

func stageInstallFile(source, target string, mode os.FileMode, nonempty bool) (string, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", errors.New("cannot read the executable source")
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() || (nonempty && info.Size() == 0) {
		return "", errors.New("executable source must be a nonempty regular file")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return "", errors.New("cannot create binary directory")
	}
	out, err := os.CreateTemp(filepath.Dir(target), ".feishu-bot-bin-*")
	if err != nil {
		return "", errors.New("cannot stage executable")
	}
	name := out.Name()
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return "", errors.New("cannot copy executable")
	}
	if err := out.Chmod(mode); err != nil {
		return "", err
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

func prepareInstallBackup(ctx context.Context, dir string, mgr serviceManager, paths ...string) (*installBackup, error) {
	definition, err := mgr.Snapshot(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("cannot save existing service definition: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	backupDir, err := os.MkdirTemp(dir, ".feishu-bot-upgrade-*")
	if err != nil {
		return nil, errors.New("cannot prepare installation recovery files")
	}
	b := &installBackup{Directory: backupDir, Service: definition}
	ok := false
	defer func() {
		if !ok {
			b.cleanup()
		}
	}()
	for i, path := range paths {
		entry := installFileBackup{Target: path}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			b.Files = append(b.Files, entry)
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("cannot back up installation file %s", path)
		}
		entry.Present, entry.Mode = true, info.Mode().Perm()
		entry.Backup = filepath.Join(backupDir, fmt.Sprintf("file-%d", i))
		if err := copyPrivateBackup(path, entry.Backup); err != nil {
			return nil, err
		}
		b.Files = append(b.Files, entry)
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writePrivate(filepath.Join(backupDir, "recovery.json"), data); err != nil {
		return nil, err
	}
	ok = true
	return b, nil
}

func copyPrivateBackup(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

func (b *installBackup) rollback(ctx context.Context, mgr serviceManager) error {
	var failures []error
	for _, entry := range b.Files {
		if !entry.Present {
			if err := os.Remove(entry.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, err)
			}
			continue
		}
		// Keep the recovery copy intact if a later restoration step fails.
		staged, err := stageInstallFile(entry.Backup, entry.Target, entry.Mode, false)
		if err == nil {
			err = os.Rename(staged, entry.Target)
			_ = os.Remove(staged)
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	if err := mgr.Restore(ctx, serviceName, b.Service); err != nil {
		failures = append(failures, err)
	}
	if len(failures) == 0 && b.wasRunning && b.Service.Present {
		if err := mgr.Start(ctx, serviceName); err != nil {
			failures = append(failures, fmt.Errorf("previous installation restored but restart failed: %w", err))
		}
	}
	return errors.Join(failures...)
}

func (b *installBackup) cleanup() { _ = os.RemoveAll(b.Directory) }
