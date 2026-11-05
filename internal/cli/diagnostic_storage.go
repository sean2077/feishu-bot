package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

type DiskFootprint struct {
	Status  string `json:"status"`
	Bytes   int64  `json:"bytes"`
	Files   int    `json:"files"`
	Skipped int    `json:"skipped,omitempty"`
}

type StorageDiagnostic struct {
	Scope    string        `json:"scope"`
	Database DiskFootprint `json:"database"`
	WAL      DiskFootprint `json:"wal"`
	SHM      DiskFootprint `json:"shm"`
	Exports  DiskFootprint `json:"exports"`
}

func diskFile(path string) DiskFootprint {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return DiskFootprint{Status: "absent"}
	}
	if err != nil || !info.Mode().IsRegular() {
		return DiskFootprint{Status: "unavailable"}
	}
	return DiskFootprint{Status: "observed", Bytes: info.Size(), Files: 1}
}

// inspectStorage reads filesystem metadata only. It deliberately does not open
// the deployment database: even a nominally read-only SQLite connection can
// create shared-memory files for a live WAL database. Symlinks are not followed.
func inspectStorage(ctx context.Context, dir string) StorageDiagnostic {
	r := StorageDiagnostic{Scope: "Filesystem metadata only; no production SQLite connection, checkpoint, row count or deletion. Export scan is bounded to 10000 entries and does not follow symlinks."}
	unknown := DiskFootprint{Status: "unavailable"}
	if ctx.Err() != nil {
		r.Database, r.WAL, r.SHM, r.Exports = unknown, unknown, unknown, unknown
		return r
	}
	root, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		absent := DiskFootprint{Status: "absent"}
		r.Database, r.WAL, r.SHM, r.Exports = absent, absent, absent, absent
		return r
	}
	if err != nil || !root.IsDir() || root.Mode()&fs.ModeSymlink != 0 {
		r.Database, r.WAL, r.SHM, r.Exports = unknown, unknown, unknown, unknown
		return r
	}
	r.Database = diskFile(filepath.Join(dir, "gateway.sqlite"))
	r.WAL = diskFile(filepath.Join(dir, "gateway.sqlite-wal"))
	r.SHM = diskFile(filepath.Join(dir, "gateway.sqlite-shm"))
	exports := filepath.Join(dir, "exports")
	info, err := os.Lstat(exports)
	if errors.Is(err, fs.ErrNotExist) {
		r.Exports.Status = "absent"
		return r
	}
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		r.Exports = unknown
		return r
	}
	r.Exports.Status = "observed"
	entries := 0
	err = filepath.WalkDir(exports, func(path string, entry fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		entries++
		if entries > 10000 {
			return errors.New("export scan limit")
		}
		if walkErr != nil {
			r.Exports.Skipped++
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			r.Exports.Skipped++
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			r.Exports.Skipped++
			return nil
		}
		r.Exports.Bytes += info.Size()
		r.Exports.Files++
		return nil
	})
	if err != nil || r.Exports.Skipped > 0 {
		r.Exports.Status = "partial"
	}
	return r
}
