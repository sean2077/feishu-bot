//go:build !windows

package native

// finalPath is only needed on Windows, where EvalSymlinks keeps drive aliases.
func finalPath(path string) (string, error) { return path, nil }
