//go:build windows

package native

import (
	"strings"

	"golang.org/x/sys/windows"
)

// finalPath resolves an existing path through an open handle. Unlike
// EvalSymlinks this removes subst and mapped-drive aliases, short names and
// reparse points, returning the path the file system actually uses.
func finalPath(path string) (string, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, 1024)
	n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0 /* VOLUME_NAME_DOS */)
	if err != nil {
		return "", err
	}
	if int(n) > len(buffer) {
		buffer = make([]uint16, n)
		if n, err = windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0 /* VOLUME_NAME_DOS */); err != nil {
			return "", err
		}
	}
	resolved := windows.UTF16ToString(buffer[:n])
	switch {
	case strings.HasPrefix(resolved, `\?\UNC\`):
		return `\` + resolved[len(`\?\UNC\`):], nil
	case strings.HasPrefix(resolved, `\?\`):
		return resolved[len(`\?\`):], nil
	}
	return resolved, nil
}
