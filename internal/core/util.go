package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// SamePath compares two paths as the platform's filesystem normally does:
// case-insensitively on Windows and macOS, exactly elsewhere.
func SamePath(a, b string) bool {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}

func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func Hash(value string) string { s := sha256.Sum256([]byte(value)); return hex.EncodeToString(s[:]) }
func Inside(root, path string) bool {
	r, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(r, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

var bearerPattern = regexp.MustCompile(`(?i)(Bearer\s+)[^\s"']+`)
var secretPattern = regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret|password)\s*[=:]\s*)[^\s,;"'}]+`)

func Redact(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	value = bearerPattern.ReplaceAllString(value, "${1}[REDACTED]")
	return secretPattern.ReplaceAllString(value, "${1}[REDACTED]")
}
