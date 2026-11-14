package native

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Transcript is the local Claude Code session record needed to adopt a
// running terminal session: where it ran and the model it last used.
type Transcript struct {
	Path  string
	CWD   string
	Model string
}

const transcriptScanLimit = 16 << 20

func validSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

// EncodeProjectDir mirrors Claude Code's project directory encoding: every
// ASCII alphanumeric byte is kept, every other character becomes '-'.
func EncodeProjectDir(cwd string) string {
	var out strings.Builder
	for _, ch := range cwd {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' {
			out.WriteRune(ch)
		} else {
			out.WriteByte('-')
		}
	}
	return out.String()
}

// LocateTranscript finds the single JSONL transcript for nativeID below the
// Claude configuration directory. The startup cwd is read from the first
// record that carries it and must agree with the encoded project directory;
// ambiguity or disagreement is rejected rather than guessed.
func LocateTranscript(configDir, nativeID string) (Transcript, error) {
	if !validSessionID(nativeID) {
		return Transcript{}, errors.New("invalid native session ID")
	}
	projects := filepath.Join(configDir, "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		return Transcript{}, errors.New("cannot read the Claude projects directory")
	}
	matches := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(projects, entry.Name(), nativeID+".jsonl")
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() {
			matches = append(matches, path)
		}
	}
	if len(matches) == 0 {
		return Transcript{}, errors.New("local Claude Code transcript was not found")
	}
	if len(matches) > 1 {
		return Transcript{}, errors.New("local Claude Code transcript has multiple matches")
	}
	path := matches[0]
	cwd, err := firstCWD(path)
	if err != nil {
		return Transcript{}, err
	}
	if !filepath.IsAbs(cwd) {
		return Transcript{}, errors.New("local Claude Code transcript has no absolute working directory")
	}
	expected := EncodeProjectDir(cwd)
	actual := filepath.Base(filepath.Dir(path))
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		if !strings.EqualFold(actual, expected) {
			return Transcript{}, errors.New("local Claude Code transcript directory does not match its working directory")
		}
	} else if actual != expected {
		return Transcript{}, errors.New("local Claude Code transcript directory does not match its working directory")
	}
	return Transcript{Path: path, CWD: cwd, Model: lastModel(path)}, nil
}

func firstCWD(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot read the local Claude Code transcript")
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, transcriptScanLimit))
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		var record struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) == nil && record.CWD != "" {
			return record.CWD, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", errors.New("cannot read the local Claude Code transcript")
	}
	return "", errors.New("local Claude Code transcript has no working directory")
}

func lastModel(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 {
		return ""
	}
	const tail = 4 << 20
	offset := int64(0)
	partial := false
	if info.Size() > tail {
		offset = info.Size() - tail
		partial = true
	}
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, tail))
	if err != nil {
		return ""
	}
	lines := bytes.Split(data, []byte{'\n'})
	if partial && len(lines) > 0 {
		lines = lines[1:]
	}
	model := ""
	for i := len(lines) - 1; i >= 0; i-- {
		var record struct {
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(lines[i], &record) == nil && record.Message.Model != "" {
			model = record.Message.Model
			break
		}
	}
	return model
}
