package cli

import (
	"context"
	"errors"
	"io"
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
	r.Exports = scanExportMetadata(ctx, exports, 10000)
	return r
}

// ReadDir's batch size bounds allocation before the entry limit is reached.
// filepath.WalkDir would first allocate the entire largest directory. os.Root
// also keeps a concurrently changed directory from redirecting this metadata
// scan outside the export tree.
func scanExportMetadata(ctx context.Context, path string, limit int) DiskFootprint {
	r := DiskFootprint{Status: "observed"}
	root, err := os.OpenRoot(path)
	if err != nil { r.Status = "unavailable"; return r }
	defer root.Close()
	pending := []string{"."}
	visited := 0
	for len(pending)>0 {
		if ctx.Err()!=nil { r.Status="partial"; return r }
		directory := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		dir, err := root.Open(directory)
		if err != nil { r.Skipped++; continue }
		for {
			entries, readErr := dir.ReadDir(128)
			for _, entry := range entries {
				visited++
				if visited>limit || ctx.Err()!=nil { _ = dir.Close(); r.Status="partial"; return r }
				relative := filepath.Join(directory, entry.Name())
				info, err := root.Lstat(relative)
				if err!=nil || info.Mode()&fs.ModeSymlink!=0 { r.Skipped++; continue }
				if info.IsDir() { pending=append(pending,relative); continue }
				if !info.Mode().IsRegular() { r.Skipped++; continue }
				r.Bytes+=info.Size(); r.Files++
			}
			if readErr!=nil {
				if !errors.Is(readErr,io.EOF) { r.Skipped++ }
				break
			}
		}
		_ = dir.Close()
	}
	if r.Skipped>0 { r.Status="partial" }
	return r
}
