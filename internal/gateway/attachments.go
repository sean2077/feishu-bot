package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

// attachmentDiskName belongs only to the atomic publication scheme. Older
// in-place downloads, including interrupted ones, never qualify as cache hits.
// Display names stay in Resource/Attachment and are not filesystem components.
func attachmentDiskName(messageID string, resource core.Resource) string {
	ext := filepath.Ext(strings.ReplaceAll(resource.Name, "\\", "/"))
	if len(ext) < 2 || len(ext) > 17 || strings.IndexFunc(ext, func(r rune) bool {
		return r != '.' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}) >= 0 {
		ext = ""
	}
	return "a-" + core.Hash(messageID+"\x00"+resource.Key) + strings.ToLower(ext)
}

// attachmentFile verifies that a published file is usable without following a
// replacement symlink. Before publication, sync also completes the local write.
func attachmentFile(path string, limit int64, syncFile bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("attachment is not a regular file within the byte limit")
	}
	flags := os.O_RDONLY
	if syncFile {
		// Windows FlushFileBuffers requires a handle opened for writing.
		flags = os.O_RDWR
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() > limit {
		return errors.New("attachment changed while checking its published file")
	}
	if syncFile {
		return file.Sync()
	}
	return nil
}

// downloadAttachment is shared by incoming messages and fetch_attachment.
// Only a fully written private file can acquire the stable published name.
// Cancellation cleans this attempt's temporary file, never an earlier result.
func (g *Gateway) downloadAttachment(ctx context.Context, w core.Workspace, messageID string, resource core.Resource) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	limit := min(g.config.Limits.AttachmentBytes, 100<<20)
	if limit <= 0 {
		return "", errors.New("attachment byte limit must be positive")
	}
	path := filepath.Join(w.Inbound, attachmentDiskName(messageID, resource))
	root, _, err := workspace.TranslateOutput(w, path)
	if err != nil || root != w.Inbound {
		return "", errors.New("attachment directory is no longer owned by this conversation")
	}
	unlock, err := g.attachmentLocks.lockContext(ctx, path)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = attachmentFile(path, limit, false); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("published attachment is unavailable: %w", err)
	}
	partial := path + ".part-" + core.NewID()
	defer os.Remove(partial)
	err = providerVoid(ctx, func() error {
		err := g.transport.Download(ctx, messageID, resource, partial, limit)
		if ctx.Err() != nil {
			// A provider may finish after its caller has stopped waiting.
			_ = os.Remove(partial)
			return ctx.Err()
		}
		return err
	})
	if err != nil {
		return "", err
	}
	if err = attachmentFile(partial, limit, true); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = os.Link(partial, path); err != nil {
		if _, exists := os.Lstat(path); errors.Is(exists, os.ErrNotExist) {
			// The per-target lock also protects this fallback on file systems
			// without hard links. No reader receives the private download name.
			err = os.Rename(partial, path)
		}
		if err != nil {
			if verify := attachmentFile(path, limit, false); verify != nil {
				return "", fmt.Errorf("attachment could not be published: %w", err)
			}
		}
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	return path, nil
}
