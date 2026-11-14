package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

const (
	attachControlPoll = 250 * time.Millisecond
	attachRequestTTL  = 5 * time.Minute
)

type attachGateway interface {
	Attach(context.Context, core.AttachRequest) core.AttachResult
}

// watchAttach consumes local adoption requests. Like the stop control file it
// relies on the data directory's same-account boundary; there is no socket or
// remote endpoint. Each request is renamed before processing so a crash can
// never replay it silently.
func watchAttach(ctx context.Context, gateway attachGateway, dataDir string, log core.Logger) {
	dir := filepath.Join(dataDir, "control", "attach")
	recoverProcessing(dir, log)
	ticker := time.NewTicker(attachControlPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			processAttachDir(ctx, gateway, dir, log)
		}
	}
}

func processAttachDir(ctx context.Context, gateway attachGateway, dir string, log core.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && log != nil {
			log("attach_control_read_failed", core.Args{})
		}
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if strings.HasSuffix(name, ".result.json") {
			// A caller that timed out may never read its result. Keep recent
			// results for the normal wait window, then discard them.
			if time.Since(info.ModTime()) > time.Hour {
				_ = os.Remove(path)
			}
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		processAttachFile(ctx, gateway, dir, path, log)
	}
}

func processAttachFile(ctx context.Context, gateway attachGateway, dir, path string, log core.Logger) {
	name := filepath.Base(path)
	id := strings.TrimSuffix(name, ".json")
	processing := path + ".processing"
	if err := os.Rename(path, processing); err != nil {
		return
	}
	defer os.Remove(processing)
	file, err := os.Open(processing)
	if err != nil {
		return
	}
	data, err := readLimited(file, 64<<10)
	file.Close()
	if err != nil {
		writeAttachResult(dir, id, core.AttachResult{Error: "认领请求无法读取"}, log)
		return
	}
	var req core.AttachRequest
	if json.Unmarshal(data, &req) != nil || req.ID != id {
		writeAttachResult(dir, id, core.AttachResult{Error: "认领请求格式无效"}, log)
		return
	}
	if req.At > 0 && time.Since(time.UnixMilli(req.At)) > attachRequestTTL {
		writeAttachResult(dir, id, core.AttachResult{Error: "认领请求已过期，请重新发起"}, log)
		return
	}
	attachCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	result := gateway.Attach(attachCtx, req)
	cancel()
	if result.Error == "" && !result.OK {
		result.Error = "认领未确认"
	}
	writeAttachResult(dir, id, result, log)
}

func recoverProcessing(dir string, log core.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".processing") {
			continue
		}
		id := strings.TrimSuffix(name, ".json.processing")
		if id == name {
			continue
		}
		resultPath := filepath.Join(dir, id+".result.json")
		if _, err := os.Lstat(resultPath); errors.Is(err, os.ErrNotExist) {
			writeAttachResult(dir, id, core.AttachResult{
				State: core.AdoptionUncertain,
				Error: "上一次认领处理中断，结果未确认；请检查飞书是否已出现话题，不会自动重发。",
			}, log)
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func readLimited(file *os.File, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("control request exceeds limit")
	}
	return data, nil
}

func writeAttachResult(dir, id string, result core.AttachResult, log core.Logger) {
	if dir == "" || id == "" {
		return
	}
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	file, err := os.CreateTemp(dir, ".attach-result-*.tmp")
	if err != nil {
		if log != nil {
			log("attach_result_write_failed", core.Args{})
		}
		return
	}
	name := file.Name()
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(name)
		if log != nil {
			log("attach_result_write_failed", core.Args{})
		}
		return
	}
	if err = os.Rename(name, filepath.Join(dir, id+".result.json")); err != nil {
		_ = os.Remove(name)
		if log != nil {
			log("attach_result_write_failed", core.Args{})
		}
	}
}
