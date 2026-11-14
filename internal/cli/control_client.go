package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

func controlDir(dataDir string) string {
	return filepath.Join(dataDir, "control", attachRequestDir)
}

func writeControlRequest(dataDir, id string, value any) (string, error) {
	dir := controlDir(dataDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", errors.New("cannot create the control directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", errors.New("control path is not a directory")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("cannot encode the control request")
	}
	file, err := os.CreateTemp(dir, ".control-*.tmp")
	if err != nil {
		return "", errors.New("cannot write the control request")
	}
	name := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(name)
	}
	if err = file.Chmod(0600); err != nil {
		cleanup()
		return "", errors.New("cannot secure the control request")
	}
	if _, err = file.Write(data); err != nil {
		cleanup()
		return "", errors.New("cannot write the control request")
	}
	if err = file.Sync(); err != nil {
		cleanup()
		return "", errors.New("cannot persist the control request")
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(name)
		return "", errors.New("cannot close the control request")
	}
	final := filepath.Join(dir, id+".json")
	if err = os.Rename(name, final); err != nil {
		_ = os.Remove(name)
		return "", errors.New("cannot publish the control request")
	}
	return final, nil
}

func waitControlResult(ctx context.Context, dataDir, id string, target any) (bool, error) {
	dir := controlDir(dataDir)
	resultPath := filepath.Join(dir, id+".result.json")
	requestPath := filepath.Join(dir, id+".json")
	deadline := time.Now().Add(attachWait)
	for {
		if info, err := os.Lstat(resultPath); err == nil && info.Mode().IsRegular() {
			file, err := os.Open(resultPath)
			if err != nil {
				return false, errors.New("cannot read the control result")
			}
			data, err := io.ReadAll(io.LimitReader(file, attachResultLimit+1))
			file.Close()
			_ = os.Remove(resultPath)
			_ = os.Remove(requestPath)
			if err != nil || len(data) > attachResultLimit {
				return false, errors.New("invalid control result")
			}
			if err = json.Unmarshal(data, target); err != nil {
				return false, errors.New("invalid control result")
			}
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(attachResultPollTime):
		}
	}
}

func withdrawControlRequest(dataDir, id string) bool {
	path := filepath.Join(controlDir(dataDir), id+".json")
	withdrawn := path + ".withdrawn"
	if err := os.Rename(path, withdrawn); err != nil {
		return false
	}
	_ = os.Remove(withdrawn)
	return true
}
