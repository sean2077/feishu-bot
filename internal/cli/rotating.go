package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingWriter appends to a file and rotates it into .1, .2, ... when the
// configured size would be exceeded. Write errors are returned to the logger
// and never terminate the service.
type RotatingWriter struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	backups int
	file    *os.File
	size    int64
}

func NewRotatingWriter(path string, maxSize int64, backups int) (*RotatingWriter, error) {
	if backups < 0 {
		backups = 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, errors.New("cannot create log directory")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, errors.New("cannot open log file")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, errors.New("cannot inspect log file")
	}
	return &RotatingWriter{path: path, maxSize: maxSize, backups: backups, file: file, size: info.Size()}, nil
}

func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.maxSize > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			// Rotation failures must not drop the current write. Keep the
			// existing file open when possible and try to append.
			if w.file == nil {
				if reopenErr := w.open(); reopenErr != nil {
					return 0, err
				}
			}
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return errors.New("cannot open log file")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return errors.New("cannot inspect log file")
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *RotatingWriter) rotate() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			w.file = nil
			return errors.New("cannot close log file for rotation")
		}
		w.file = nil
	}
	var firstErr error
	if w.backups > 0 {
		_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.backups))
		for i := w.backups - 1; i >= 1; i-- {
			old := fmt.Sprintf("%s.%d", w.path, i)
			next := fmt.Sprintf("%s.%d", w.path, i+1)
			if err := os.Rename(old, next); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
				firstErr = errors.New("cannot rotate log backups")
			}
		}
		if w.backups == 1 {
			_ = os.Remove(w.path + ".1")
		}
		if err := os.Rename(w.path, w.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = errors.New("cannot rotate log file")
		}
	} else {
		if err := os.Remove(w.path); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = errors.New("cannot remove log file during rotation")
		}
	}
	if err := w.open(); err != nil {
		return err
	}
	return firstErr
}

func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
