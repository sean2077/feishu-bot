package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const daemonMetaSchema = 1

type daemonMeta struct {
	Schema        int    `json:"schema"`
	Version       string `json:"version"`
	Name          string `json:"name"`
	Platform      string `json:"platform"`
	WorkDir       string `json:"work_dir"`
	BinaryPath    string `json:"binary_path"`
	ConfigPath    string `json:"config_path"`
	EnvFile       string `json:"env_file"`
	DataDir       string `json:"data_dir"`
	LogFile       string `json:"log_file"`
	LogMaxSize    int64  `json:"log_max_size"`
	LogMaxBackups int    `json:"log_max_backups"`
	InstalledAt   string `json:"installed_at"`
}

// daemonHome is the per-user metadata directory. FEISHU_BOT_HOME is an
// explicit override used by tests and isolated deployments.
func daemonHome() (string, error) {
	if value := strings.TrimSpace(os.Getenv("FEISHU_BOT_HOME")); value != "" {
		dir, err := filepath.Abs(value)
		if err != nil {
			return "", errors.New("invalid FEISHU_BOT_HOME")
		}
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot determine user home directory")
	}
	return filepath.Join(home, ".feishu-bot"), nil
}

func daemonMetaPath() (string, error) {
	dir, err := daemonHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.json"), nil
}

func loadDaemonMeta() (daemonMeta, error) {
	path, err := daemonMetaPath()
	if err != nil {
		return daemonMeta{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return daemonMeta{}, err
	}
	var meta daemonMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return daemonMeta{}, errors.New("daemon metadata is invalid; re-run feishu-bot daemon install")
	}
	if meta.Schema != daemonMetaSchema || meta.Name == "" || meta.ConfigPath == "" || meta.DataDir == "" {
		return daemonMeta{}, errors.New("daemon metadata is incomplete or from an unsupported version; re-run feishu-bot daemon install")
	}
	return meta, nil
}

func saveDaemonMeta(meta daemonMeta) error {
	path, err := daemonMetaPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("cannot create daemon metadata directory")
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return errors.New("cannot encode daemon metadata")
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon-*")
	if err != nil {
		return errors.New("cannot write daemon metadata")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return errors.New("cannot secure daemon metadata")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("cannot write daemon metadata")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("cannot flush daemon metadata")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("cannot close daemon metadata")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cannot replace daemon metadata: %w", err)
	}
	return nil
}
