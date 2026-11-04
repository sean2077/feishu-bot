package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

const maxEnvFileBytes = 1 << 20

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// runtimeEnvAllowlist contains non-secret process settings that make the
// installed service match the environment used for installation. HOME and
// USERPROFILE are deliberately excluded; the service manager provides them.
var runtimeEnvAllowlist = []string{
	"TZ", "PATH", "LANG", "LC_ALL",
	"CLAUDE_CONFIG_DIR",
	"CLAUDE_CODE_GIT_BASH_PATH",
	"CLAUDE_CODE_AUTO_COMPACT_WINDOW",
	"CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE",
}

// parseEnvFile reads KEY=VALUE lines. It never includes values in errors.
func parseEnvFile(data []byte) (map[string]string, error) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if len(data) > maxEnvFileBytes {
		return nil, errors.New("environment file exceeds 1 MiB")
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), maxEnvFileBytes)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSuffix(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			return nil, fmt.Errorf("environment file line %d must use KEY=VALUE", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !envKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("environment file line %d has an invalid variable name", line)
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("environment file line %d has an invalid value", line)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("environment file could not be read")
	}
	return values, nil
}

// LoadEnvFile applies KEY=VALUE lines to the current process. Process
// environment variables not mentioned in the file are left untouched.
func LoadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read environment file %s", path)
	}
	values, err := parseEnvFile(data)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := os.Setenv(key, values[key]); err != nil {
			return fmt.Errorf("cannot apply environment variable %s", key)
		}
	}
	return nil
}

// credentialNames returns the source environment variable names required by
// the configuration. Action env maps are target-name -> source-name, matching
// internal/gateway/actions.go.
func credentialNames(c core.Config) []string {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	add(c.AppSecretEnv)
	add(c.Model.TokenEnv)
	for _, project := range c.Projects {
		for _, action := range project.Actions {
			for _, source := range action.Env {
				add(source)
			}
		}
	}
	for _, source := range c.Model.ExtraHeaders {
		if strings.HasPrefix(source, "env:") {
			add(strings.TrimPrefix(source, "env:"))
		}
	}
	sort.Strings(names)
	return names
}

func missingCredentials(c core.Config, values map[string]string) []string {
	var missing []string
	for _, name := range credentialNames(c) {
		if strings.TrimSpace(values[name]) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// buildEnvFile captures required credentials and allowlisted runtime settings
// from the current process. It returns the rendered file and any missing
// credential names; callers must not write the file when names are missing.
func buildEnvFile(c core.Config) ([]byte, []string, error) {
	values := map[string]string{}
	var missing []string
	for _, name := range credentialNames(c) {
		value, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(value) == "" {
			missing = append(missing, name)
			continue
		}
		values[name] = value
	}
	for _, name := range runtimeEnvAllowlist {
		if _, exists := values[name]; exists {
			continue
		}
		if value, ok := os.LookupEnv(name); ok && value != "" {
			values[name] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, key := range keys {
		value := values[key]
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, missing, fmt.Errorf("environment variable %s contains an unsupported newline or NUL", key)
		}
		out.WriteString(key)
		out.WriteByte('=')
		out.WriteString(value)
		out.WriteByte('\n')
	}
	return []byte(out.String()), missing, nil
}
