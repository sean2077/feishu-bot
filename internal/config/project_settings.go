package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
)

const maxProjectSettingsBytes = 1 << 20

// ProjectSettings is the administrator-maintained native settings snapshot
// loaded from projects[].settingsFile. Only env, permissions.allow/ask/deny and
// an optional autoMode object are accepted; rule contents and autoMode fields
// are never included in errors.
type ProjectSettings struct {
	Env         map[string]string
	Permissions map[string][]string
	AutoMode    json.RawMessage
}

// ProjectSettingsError reports an invalid project settings file without
// carrying environment values or rule contents.
type ProjectSettingsError struct {
	Project string
	Path    string
	Field   string
	Reason  string
}

func (e *ProjectSettingsError) Error() string {
	// Parentheses keep the field from looking like a "SECRET: value" pair to
	// generic log redaction, which would mangle the reason.
	return fmt.Sprintf("project %s: settings file %s (%s) %s", e.Project, e.Path, e.Field, e.Reason)
}

// LoadProjectSettings reads and validates the configured settings file. The
// file is read on every call so a new native process sees the current snapshot;
// callers must not cache the result across process launches.
func LoadProjectSettings(c core.Config, p core.Project) (ProjectSettings, error) {
	settings := ProjectSettings{}
	if p.SettingsFile == "" {
		return settings, nil
	}
	fail := func(field, reason string) (ProjectSettings, error) {
		return ProjectSettings{}, &ProjectSettingsError{Project: p.ID, Path: p.SettingsFile, Field: field, Reason: reason}
	}
	file, err := os.Open(p.SettingsFile)
	if err != nil {
		return fail("file", "cannot be read")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fail("file", "must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxProjectSettingsBytes+1))
	if err != nil {
		return fail("file", "cannot be read")
	}
	if len(data) > maxProjectSettingsBytes {
		return fail("file", "exceeds 1 MiB")
	}
	if !utf8.Valid(data) {
		return fail("file", "must be UTF-8 JSON")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return fail("file", "must be a JSON object")
	}
	for _, key := range sortedKeys(top) {
		if key != "env" && key != "permissions" && key != "autoMode" {
			return fail(key, "unknown field")
		}
	}
	if raw, ok := top["autoMode"]; ok {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return fail("autoMode", "must be an object")
		}
		// autoMode is administrator-trusted classifier guidance. Validate only
		// that it is an object and pass all subfields through unchanged.
		settings.AutoMode = append(json.RawMessage(nil), raw...)
	}
	derived := derivedProjectEnv(&c)
	projectEnv := map[string]string{}
	for name := range p.Env {
		projectEnv[strings.ToLower(name)] = name
	}
	projectPassthrough := map[string]string{}
	for _, name := range p.EnvPassthrough {
		projectPassthrough[strings.ToLower(name)] = name
	}
	if raw, ok := top["env"]; ok {
		var env map[string]json.RawMessage
		if err := json.Unmarshal(raw, &env); err != nil || env == nil {
			return fail("env", "must be an object")
		}
		settings.Env = map[string]string{}
		seen := map[string]string{}
		for _, name := range sortedKeys(env) {
			field := "env." + name
			if !envNamePattern.MatchString(name) {
				return fail(field, "name must match ^[A-Za-z_][A-Za-z0-9_]*$")
			}
			if reason, ok := reservedProjectEnv(name, derived); ok {
				return fail(field, reason)
			}
			var value string
			if err := json.Unmarshal(env[name], &value); err != nil {
				return fail(field, "value must be a string")
			}
			if !nonempty(value) {
				return fail(field, "value must be nonempty and must not contain NUL")
			}
			lower := strings.ToLower(name)
			if previous, ok := seen[lower]; ok {
				return fail(field, "name duplicates "+previous+" ignoring case")
			}
			if previous, ok := projectEnv[lower]; ok {
				return fail(field, "name is also configured in projects[].env as "+previous)
			}
			if previous, ok := projectPassthrough[lower]; ok {
				return fail(field, "name is also configured in projects[].envPassthrough as "+previous)
			}
			seen[lower] = name
			settings.Env[name] = value
		}
	}
	if raw, ok := top["permissions"]; ok {
		var permissions map[string]json.RawMessage
		if err := json.Unmarshal(raw, &permissions); err != nil || permissions == nil {
			return fail("permissions", "must be an object")
		}
		settings.Permissions = map[string][]string{}
		for _, key := range sortedKeys(permissions) {
			field := "permissions." + key
			if key != "allow" && key != "ask" && key != "deny" {
				return fail(field, "unknown field")
			}
			var rules []json.RawMessage
			if err := json.Unmarshal(permissions[key], &rules); err != nil || rules == nil {
				return fail(field, "must be an array of nonempty strings")
			}
			values := make([]string, 0, len(rules))
			for i, rawRule := range rules {
				var rule string
				if err := json.Unmarshal(rawRule, &rule); err != nil {
					return fail(fmt.Sprintf("%s[%d]", field, i), "must be a string")
				}
				if !nonempty(rule) {
					return fail(fmt.Sprintf("%s[%d]", field, i), "must be a nonempty string without NUL")
				}
				values = append(values, rule)
			}
			settings.Permissions[key] = values
		}
	}
	return settings, nil
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
