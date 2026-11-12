// Package config loads the existing JSON configuration without changing its identity or defaults.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/sean2077/feishu-bot/internal/core"
)

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$`)
var headerPattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
var parameterPattern = regexp.MustCompile(`\{\{([^}]+)\}\}`)
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ProjectEnvError reports an invalid projects[].env or envPassthrough entry.
// It never carries a value, so it is safe to print before secret redaction.
type ProjectEnvError struct {
	Project  string
	Variable string
	Reason   string
}

func (e *ProjectEnvError) Error() string {
	// Parentheses keep the variable name from looking like a "SECRET: value"
	// pair to generic log redaction, which would mangle the reason.
	return fmt.Sprintf("project %s: environment variable %s (%s)", e.Project, e.Variable, e.Reason)
}

// Reserved names keep the deployment boundary (native HOME/config, model
// credentials, bridge channels and action relay) out of project control.
var reservedProjectEnvNames = map[string]bool{
	"home": true, "userprofile": true, "homedrive": true, "homepath": true,
	"claude_config_dir": true, "claudecode": true, "node_options": true, "debug": true,
}

// "claude_code_" also reserves CLAUDE_CODE_AUTO_MODE_SERVER, which the
// gateway sets for auto projects to force the local safety classifier.
var reservedProjectEnvPrefixes = []string{"anthropic_", "claude_code_", "claude_agent_sdk", "feishu_action_", "feishu_request_"}

func derivedProjectEnv(c *core.Config) map[string]string {
	derived := map[string]string{}
	add := func(name, reason string) {
		name = strings.ToLower(name)
		if name == "" {
			return
		}
		if _, exists := derived[name]; !exists {
			derived[name] = reason
		}
	}
	add(c.AppSecretEnv, "name is reserved by appSecretEnv")
	add(c.Model.TokenEnv, "name is reserved by model.tokenEnv")
	for _, source := range c.Model.ExtraHeaders {
		if strings.HasPrefix(source, "env:") {
			add(strings.TrimPrefix(source, "env:"), "name is reserved by model.extraHeaders")
		}
	}
	// Action credentials reach only the fixed-operation subprocess, never the
	// native agent. A project passthrough with the same source name would
	// bypass that boundary, so every referenced source is reserved for every
	// project.
	for _, p := range c.Projects {
		for _, a := range p.Actions {
			for _, source := range a.Env {
				add(source, "name is reserved because it is used by fixed action "+p.ID+"/"+a.ID)
			}
		}
	}
	return derived
}

func reservedProjectEnv(name string, derived map[string]string) (string, bool) {
	lower := strings.ToLower(name)
	if reservedProjectEnvNames[lower] {
		return "name is reserved by the deployment", true
	}
	for _, prefix := range reservedProjectEnvPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return "name is reserved by the deployment", true
		}
	}
	reason, ok := derived[lower]
	return reason, ok
}

// validateProjectEnvironment rejects unsafe or ambiguous definitions at load
// time. Errors include the project and variable name only, never the value.
func validateProjectEnvironment(p *core.Project, derived map[string]string) error {
	fail := func(name, reason string) error {
		return &ProjectEnvError{Project: p.ID, Variable: name, Reason: reason}
	}
	envNames := make([]string, 0, len(p.Env))
	for name := range p.Env {
		envNames = append(envNames, name)
	}
	sort.Strings(envNames)
	seenEnv := map[string]string{}
	for _, name := range envNames {
		if !envNamePattern.MatchString(name) {
			return fail(name, "name must match ^[A-Za-z_][A-Za-z0-9_]*$")
		}
		if reason, ok := reservedProjectEnv(name, derived); ok {
			return fail(name, reason)
		}
		if !nonempty(p.Env[name]) {
			return fail(name, "value must be nonempty and must not contain NUL")
		}
		lower := strings.ToLower(name)
		if previous, ok := seenEnv[lower]; ok {
			return fail(name, "name duplicates "+previous+" ignoring case")
		}
		seenEnv[lower] = name
	}
	seenPassthrough := map[string]string{}
	for _, name := range p.EnvPassthrough {
		if !envNamePattern.MatchString(name) {
			return fail(name, "name must match ^[A-Za-z_][A-Za-z0-9_]*$")
		}
		if reason, ok := reservedProjectEnv(name, derived); ok {
			return fail(name, reason)
		}
		lower := strings.ToLower(name)
		if previous, ok := seenPassthrough[lower]; ok {
			return fail(name, "passthrough name duplicates "+previous+" ignoring case")
		}
		if previous, ok := seenEnv[lower]; ok {
			return fail(name, "name is also configured literally as "+previous)
		}
		seenPassthrough[lower] = name
	}
	return nil
}

func Defaults() core.Config {
	return core.Config{AppSecretEnv: "FEISHU_APP_SECRET", CardTitle: "飞书助手", DataDir: "state",
		Runtime: core.RuntimeConfig{MaxWorkers: 8, IdleMS: 900000},
		Model:   core.ModelConfig{TokenEnv: "MODEL_AUTH_TOKEN", ExtraHeaders: map[string]string{}},
		Limits:  core.Limits{BackgroundMS: 900000, TextChars: 20000, InboxPerRoom: 64, AttachmentBytes: 20 << 20, ApprovalMS: 300000, UpdateMS: 1000, MessagesPerMinute: 30, ImageBatchMaxImages: 8, ImageBatchBytes: 40 << 20}}
}
func Load(path string) (core.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return core.Config{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return core.Config{}, err
	}
	return Parse(data, filepath.Dir(abs))
}
func Parse(data []byte, baseDir string) (core.Config, error) {
	// Match the former JSON.parse/TypeScript schema: only fields which used
	// null-coalescing defaults accept null. Permission flags and individual
	// resource limits must still reject it instead of retaining Go zero values.
	var shape any
	if err := json.Unmarshal(data, &shape); err != nil {
		return core.Config{}, errors.New("invalid JSON configuration")
	}
	legacyDefaults(shape)
	if err := checkValues(shape); err != nil {
		return core.Config{}, err
	}
	// JSON numbers such as 9e5 or 900000.0 were integers in the old parser.
	// Canonicalizing their representation keeps those configurations valid;
	// the typed decoder and range checks still reject fractions/unsafe limits.
	data, _ = json.Marshal(shape)
	// Normalize only the two explicitly supported legacy aliases before strict decoding.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return core.Config{}, errors.New("invalid JSON configuration")
	}
	for _, key := range []string{"model", "runtime", "limits"} {
		if v, ok := raw[key]; ok && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return core.Config{}, fmt.Errorf("%s must be an object", key)
		}
	}
	var runtimeMap map[string]json.RawMessage
	if v, ok := raw["runtime"]; ok {
		if err := json.Unmarshal(v, &runtimeMap); err != nil {
			return core.Config{}, errors.New("runtime must be an object")
		}
	}
	if v, ok := runtimeMap["mode"]; ok {
		var mode string
		if json.Unmarshal(v, &mode) != nil || mode != "local" {
			return core.Config{}, errors.New("container runtime is unsupported; remove runtime.mode/image/memory/cpus")
		}
		delete(runtimeMap, "mode")
		raw["runtime"], _ = json.Marshal(runtimeMap)
	}
	if err := nonemptyFields(runtimeMap, "claudeExecutable"); err != nil {
		return core.Config{}, err
	}
	if err := nonemptyFields(raw, "dataDir"); err != nil {
		return core.Config{}, err
	}
	var modelMap map[string]json.RawMessage
	if err := json.Unmarshal(raw["model"], &modelMap); err != nil || modelMap == nil {
		return core.Config{}, errors.New("model must be an object")
	}
	if err := nonemptyFields(modelMap, "smallModel"); err != nil {
		return core.Config{}, err
	}
	var projects []map[string]json.RawMessage
	if err := json.Unmarshal(raw["projects"], &projects); err != nil || len(projects) == 0 {
		return core.Config{}, errors.New("at least one project is required")
	}
	for _, p := range projects {
		if p == nil {
			return core.Config{}, errors.New("project must be an object")
		}
		if err := nonemptyFields(p, "cwd", "seedDir", "trustedConfigDir", "settingsFile", "appendSystemPromptFile"); err != nil {
			return core.Config{}, err
		}
		mode := "acceptEdits"
		if v, ok := p["autoApproveWorkspace"]; ok {
			if string(v) != "false" {
				return core.Config{}, errors.New("autoApproveWorkspace is unsupported; use permissionMode and allowedTools")
			}
			mode = "default"
			delete(p, "autoApproveWorkspace")
		}
		setDefault(p, "permissionMode", mode)
		setDefault(p, "nativeConfig", "isolated")
		var actions []map[string]json.RawMessage
		if v, ok := p["actions"]; ok {
			if json.Unmarshal(v, &actions) != nil || string(v) == "null" {
				return core.Config{}, errors.New("actions must be an array")
			}
		}
		for _, a := range actions {
			if a == nil {
				return core.Config{}, errors.New("action must be an object")
			}
			if err := nonemptyFields(a, "cwd"); err != nil {
				return core.Config{}, err
			}
			setDefault(a, "timeoutMs", 60000)
			setDefault(a, "approval", "card")
			var params map[string]map[string]json.RawMessage
			if v, ok := a["parameters"]; ok {
				if json.Unmarshal(v, &params) != nil || string(v) == "null" {
					return core.Config{}, errors.New("parameters must be an object")
				}
			}
			for _, param := range params {
				if param == nil {
					return core.Config{}, errors.New("parameter must be an object")
				}
				if err := nonemptyFields(param, "pattern", "description"); err != nil {
					return core.Config{}, err
				}
				setDefault(param, "maxLength", 2048)
			}
			if params != nil {
				a["parameters"], _ = json.Marshal(params)
			}
		}
		if actions != nil {
			p["actions"], _ = json.Marshal(actions)
		}
	}
	raw["projects"], _ = json.Marshal(projects)
	normalized, _ := json.Marshal(raw)
	if err := exactFields(normalized, reflect.TypeOf(core.Config{}), "config"); err != nil {
		return core.Config{}, err
	}
	c := Defaults()
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		// Decoder errors can echo user-provided field names or numeric values
		// before the deployment's secrets are available for redaction.
		return core.Config{}, errors.New("invalid configuration field or value type")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return core.Config{}, errors.New("unexpected trailing configuration")
	}
	if err := validate(&c, baseDir); err != nil {
		return core.Config{}, err
	}
	return c, nil
}

// legacyDefaults removes only nulls which the previous schema treated as an
// omitted field. In particular, false and zero are never replaced by defaults.
func legacyDefaults(value any) {
	c, ok := value.(map[string]any)
	if !ok {
		return
	}
	omit := func(m map[string]any, keys ...string) {
		for _, key := range keys {
			if v, present := m[key]; present && v == nil {
				delete(m, key)
			}
		}
	}
	omit(c, "runtime", "limits", "groups", "dms", "appSecretEnv")
	if m, ok := c["model"].(map[string]any); ok {
		omit(m, "tokenEnv", "extraHeaders")
	}
	if projects, ok := c["projects"].([]any); ok {
		for _, value := range projects {
			p, ok := value.(map[string]any)
			if !ok {
				continue
			}
			omit(p, "allowedTools", "actions")
			if actions, ok := p["actions"].([]any); ok {
				for _, value := range actions {
					if a, ok := value.(map[string]any); ok {
						omit(a, "args", "env", "vars", "parameters")
					}
				}
			}
		}
	}
	if groups, ok := c["groups"].([]any); ok {
		for _, value := range groups {
			if g, ok := value.(map[string]any); ok {
				omit(g, "members")
			}
		}
	}
}

// encoding/json deliberately matches struct fields without regard to case.
// Configuration permissions must instead use the exact documented field names,
// as the previous strict TypeScript schema did. Map keys remain user-defined.
func exactFields(raw json.RawMessage, typ reflect.Type, at string) error {
	switch typ.Kind() {
	case reflect.Struct:
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return nil
		} // typed decoding reports this
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = field.Type
			}
		}
		for name, value := range values {
			child, ok := fields[name]
			if !ok {
				return fmt.Errorf("%s: unknown configuration field; names are case-sensitive", at)
			}
			if err := exactFields(value, child, at+"."+name); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return nil
		}
		for _, value := range values {
			if err := exactFields(value, typ.Elem(), at+"[]"); err != nil {
				return err
			}
		}
	case reflect.Ptr:
		return exactFields(raw, typ.Elem(), at)
	case reflect.Map:
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return nil
		}
		for _, value := range values {
			if err := exactFields(value, typ.Elem(), at+".*"); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkValues(value any) error {
	switch v := value.(type) {
	case nil:
		return errors.New("configuration fields cannot be null; omit optional fields")
	case map[string]any:
		for _, child := range v {
			if err := checkValues(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := checkValues(child); err != nil {
				return err
			}
		}
	}
	return nil
}
func nonemptyFields(m map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		if raw, ok := m[key]; ok {
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return fmt.Errorf("%s must be a nonempty string", key)
			}
			if strings.ContainsRune(value, 0) {
				return fmt.Errorf("%s contains a NUL control character; write it as an escaped sequence in the JSON string instead of a raw character", key)
			}
			if !nonempty(value) {
				return fmt.Errorf("%s must be a nonempty string", key)
			}
		}
	}
	return nil
}
func setDefault(m map[string]json.RawMessage, key string, value any) {
	if _, ok := m[key]; !ok {
		m[key], _ = json.Marshal(value)
	}
}
func nonempty(s string) bool { return strings.TrimSpace(s) != "" && !strings.ContainsRune(s, 0) }
func absolute(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	p, _ := filepath.Abs(filepath.Join(base, path))
	return p
}
func validate(c *core.Config, base string) error {
	if err := validateProfiles(c); err != nil {
		return err
	}
	if c.CardTitle == "" {
		c.CardTitle = "飞书助手"
	} else {
		title := strings.TrimSpace(c.CardTitle)
		if title == "" {
			return errors.New("cardTitle must be 1-40 characters")
		}
		if len([]rune(title)) > 40 {
			return errors.New("cardTitle must be 1-40 characters")
		}
		for _, r := range title {
			if unicode.IsControl(r) {
				return errors.New("cardTitle contains control characters")
			}
		}
		c.CardTitle = title
	}
	for k, v := range map[string]string{"tenantId": c.TenantID, "appId": c.AppID, "appSecretEnv": c.AppSecretEnv, "model.tokenEnv": c.Model.TokenEnv} {
		if !idPattern.MatchString(v) {
			return fmt.Errorf("%s: invalid id", k)
		}
	}
	if !nonempty(c.DataDir) || !nonempty(c.Model.Model) {
		return errors.New("dataDir and model.model must be nonempty")
	}
	c.DataDir = absolute(base, c.DataDir)
	if c.Feedback.Export.Enabled && !nonempty(c.Feedback.Export.Dir) {
		return errors.New("feedback.export.dir is required when feedback.export.enabled is true")
	}
	if c.Feedback.Export.Dir != "" {
		if !nonempty(c.Feedback.Export.Dir) {
			return errors.New("invalid feedback.export.dir")
		}
		c.Feedback.Export.Dir = absolute(base, c.Feedback.Export.Dir)
	}
	if c.Runtime.ClaudeExecutable != "" {
		c.Runtime.ClaudeExecutable = absolute(base, c.Runtime.ClaudeExecutable)
	}
	u, err := url.Parse(c.Model.BaseURL)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("invalid model.baseUrl")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && c.Model.AllowHTTP) {
		return errors.New("model gateway must use HTTPS")
	}
	if port := u.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return errors.New("invalid model.baseUrl port")
		}
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("model.baseUrl must not contain credentials, query or fragment")
	}
	c.Model.BaseURL = strings.TrimSuffix(strings.TrimSuffix(u.String(), "/"), "/v1")
	if c.Model.SmallModel == "" {
		c.Model.SmallModel = c.Model.Model
	}
	for key, v := range c.Model.ExtraHeaders {
		if !headerPattern.MatchString(key) || !nonempty(v) || strings.ContainsAny(v, "\r\n") {
			return errors.New("invalid model header")
		}
	}
	projects := map[string]bool{}
	derivedEnv := derivedProjectEnv(c)
	for i := range c.Projects {
		p := &c.Projects[i]
		if !idPattern.MatchString(p.ID) || projects[p.ID] {
			return errors.New("invalid or duplicate project id")
		}
		projects[p.ID] = true
		if len(p.Maintainers) == 0 {
			return errors.New("explicit maintainers required")
		}
		maintainers := map[string]bool{}
		for _, m := range p.Maintainers {
			if !nonempty(m) || m == "*" {
				return errors.New("invalid maintainer id")
			}
			maintainers[m] = true
		}
		owners := map[string]bool{}
		for _, owner := range p.Owners {
			if !nonempty(owner) || owner == "*" || owners[owner] {
				return errors.New("invalid project owner")
			}
			if !maintainers[owner] {
				return errors.New("project owners must be maintainers")
			}
			owners[owner] = true
		}
		if p.NativeConfig != "isolated" && p.NativeConfig != "inherit" {
			return errors.New("nativeConfig must be isolated or inherit")
		}
		if p.ModelProfile != "" && p.ModelProfile != "default" {
			if _, ok := c.ModelProfiles[p.ModelProfile]; !ok {
				return errors.New("project modelProfile must name a configured profile or default")
			}
		}
		if p.PermissionMode != "default" && p.PermissionMode != "acceptEdits" && p.PermissionMode != "bypassPermissions" && p.PermissionMode != "auto" {
			return errors.New("invalid permissionMode")
		}
		switch p.DisplayWorkspace {
		case "", "full", "basename", "project", "hidden":
		default:
			return errors.New("displayWorkspace must be full, basename, project or hidden")
		}
		if err := validateProjectEnvironment(p, derivedEnv); err != nil {
			return err
		}
		if p.CWD != "" && (p.SeedDir != "" || p.TrustedConfigDir != "") {
			return errors.New("project.cwd cannot be combined with seedDir or trustedConfigDir")
		}
		for _, path := range []*string{&p.CWD, &p.SeedDir, &p.TrustedConfigDir, &p.SettingsFile, &p.AppendSystemPromptFile} {
			if *path != "" {
				if !nonempty(*path) {
					return errors.New("invalid project directory")
				}
				*path = absolute(base, *path)
			}
		}
		if p.AppendSystemPrompt != "" && p.AppendSystemPromptFile != "" {
			return errors.New("appendSystemPrompt and appendSystemPromptFile are mutually exclusive")
		}
		if p.AppendSystemPrompt != "" {
			if reason := projectPromptTextReason(p.AppendSystemPrompt); reason != "" {
				return errors.New("invalid appendSystemPrompt: " + reason)
			}
		}
		if p.AppendSystemPromptFile != "" && core.Inside(filepath.Join(c.DataDir, "rooms"), p.AppendSystemPromptFile) {
			return errors.New("appendSystemPromptFile must not be inside the dataDir room tree")
		}
		for _, tool := range p.AllowedTools {
			if !nonempty(tool) {
				return errors.New("invalid allowed tool")
			}
		}
		actions := map[string]bool{}
		for j := range p.Actions {
			a := &p.Actions[j]
			if !idPattern.MatchString(a.ID) || actions[a.ID] {
				return errors.New("invalid or duplicate action id")
			}
			actions[a.ID] = true
			if !nonempty(a.Description) || !filepath.IsAbs(a.Executable) || !nonempty(a.Executable) {
				return errors.New("action needs a description and absolute trusted executable")
			}
			approvers := map[string]bool{}
			for _, approver := range a.Approvers {
				if !nonempty(approver) || approver == "*" || approvers[approver] {
					return errors.New("invalid action approver")
				}
				approvers[approver] = true
			}
			if a.TimeoutMS < 100 || a.TimeoutMS > 600000 {
				return errors.New("invalid action timeoutMs")
			}
			if a.Approval != "card" && a.Approval != "none" {
				return errors.New("invalid action approval")
			}
			if a.CWD != "" {
				// Legacy action cwd was relative to the service process, unlike
				// project paths. Do not silently redirect an existing operation.
				a.CWD = absolute("", a.CWD)
			}
			for _, m := range []map[string]string{a.Env, a.Vars} {
				for k, v := range m {
					if !nonempty(k) || !nonempty(v) || strings.Contains(k, "=") {
						return errors.New("invalid action environment")
					}
					if strings.HasPrefix(strings.ToUpper(k), "FEISHU_REQUEST_") {
						return errors.New("action environment cannot set FEISHU_REQUEST_*")
					}
					if strings.HasPrefix(strings.ToUpper(k), "FEISHU_ACTION_") {
						return errors.New("action environment cannot set FEISHU_ACTION_*")
					}
				}
			}
			for name, p := range a.Parameters {
				if !idPattern.MatchString(name) || p.MaxLength < 1 || p.MaxLength > 32768 {
					return errors.New("invalid action parameter")
				}
				for _, v := range p.Enum {
					if !nonempty(v) {
						return errors.New("parameter enum values must be nonempty strings")
					}
				}
				if len(p.Pattern) > 256 {
					return errors.New("parameter pattern too long")
				}
				if p.Pattern != "" {
					if _, err := regexp.Compile(p.Pattern); err != nil {
						return fmt.Errorf("parameter %s: unsupported regular expression", name)
					}
				}
			}
			for _, arg := range a.Args {
				if !nonempty(arg) {
					return errors.New("invalid action argument")
				}
				for _, m := range parameterPattern.FindAllStringSubmatch(arg, -1) {
					if _, ok := a.Parameters[m[1]]; !ok {
						return errors.New("undefined action parameter")
					}
				}
			}
		}
		if p.PreContext != nil {
			pc := p.PreContext
			if pc.Action == "" || pc.Parameter == "" {
				return errors.New("preContext requires action and parameter")
			}
			var spec *core.ActionSpec
			for i := range p.Actions {
				if p.Actions[i].ID == pc.Action {
					spec = &p.Actions[i]
					break
				}
			}
			if spec == nil {
				return errors.New("preContext action is not configured in this project")
			}
			if !spec.ReadOnly {
				return errors.New("preContext action must be readOnly")
			}
			param, ok := spec.Parameters[pc.Parameter]
			if !ok {
				return errors.New("preContext parameter is not declared by the action")
			}
			if param.WorkspaceFile {
				return errors.New("preContext parameter cannot be a workspace file")
			}
			for _, declared := range spec.Parameters {
				if declared.WorkspaceFile {
					return errors.New("preContext action cannot declare workspace file parameters")
				}
			}
			if pc.TimeoutMS == 0 {
				pc.TimeoutMS = 5000
			}
			if pc.TimeoutMS < 100 || pc.TimeoutMS > spec.TimeoutMS {
				return errors.New("preContext timeoutMs must be within the action timeoutMs")
			}
			if pc.MaxChars == 0 {
				pc.MaxChars = 6000
			}
			if pc.MaxChars < 1 || pc.MaxChars > 20000 {
				return errors.New("preContext maxChars must be 1..20000")
			}
			for _, arg := range spec.Args {
				for _, m := range parameterPattern.FindAllStringSubmatch(arg, -1) {
					if m[1] != pc.Parameter {
						return errors.New("preContext action may only template its configured parameter")
					}
				}
			}
			templated := false
			for i, arg := range spec.Args {
				if arg != "{{"+pc.Parameter+"}}" {
					continue
				}
				if i == 0 || spec.Args[i-1] != "--" {
					return errors.New("preContext parameter must follow a literal -- argument")
				}
				templated = true
			}
			if !templated {
				return errors.New("preContext action must pass its parameter as one argv after --")
			}
		}
	}
	seen := map[string]bool{}
	for i := range c.Groups {
		g := &c.Groups[i]
		if (g.ChatID != core.AnyGroupChatID && !idPattern.MatchString(g.ChatID)) || seen[g.ChatID] || !projects[g.ProjectID] {
			return errors.New("invalid group binding")
		}
		seen[g.ChatID] = true
		if g.AllowOtherBots && g.ChatID == core.AnyGroupChatID {
			return errors.New("allowOtherBots requires an explicit group")
		}
		if len(g.OtherBots) > 0 && !g.AllowOtherBots {
			return errors.New("otherBots requires allowOtherBots")
		}
		botApps, botNames := map[string]bool{}, map[string]bool{}
		for j, b := range g.OtherBots {
			if !strings.HasPrefix(b.AppID, "cli_") || !idPattern.MatchString(b.AppID) || botApps[b.AppID] {
				return fmt.Errorf("groups[%d].otherBots[%d] has an invalid or duplicate appId", i, j)
			}
			name := strings.TrimSpace(b.Name)
			if name == "" || len([]rune(name)) > 80 || strings.IndexFunc(name, unicode.IsControl) >= 0 || botNames[strings.ToLower(name)] {
				return fmt.Errorf("groups[%d].otherBots[%d] has an invalid or duplicate name", i, j)
			}
			if b.OpenID != "" && (!strings.HasPrefix(b.OpenID, "ou_") || !idPattern.MatchString(b.OpenID)) {
				return fmt.Errorf("groups[%d].otherBots[%d] has an invalid openId", i, j)
			}
			botApps[b.AppID] = true
			botNames[strings.ToLower(name)] = true
			g.OtherBots[j].Name = name
		}
		if !core.ValidSessionScope(g.SessionScope) {
			return errors.New("invalid group sessionScope")
		}
		switch g.TopicHistory {
		case "":
			g.TopicHistory = "topic"
		case "topic":
		case "chat":
			if g.SessionScope != core.SessionScopeThread {
				return errors.New("topicHistory chat requires sessionScope thread")
			}
		default:
			return errors.New("topicHistory must be topic or chat")
		}
		if len(g.ThreadWorkspaces) > 0 && (g.SessionScope != core.SessionScopeThread || g.ChatID == core.AnyGroupChatID) {
			return errors.New("threadWorkspaces requires an explicit group with sessionScope thread")
		}
		for root, path := range g.ThreadWorkspaces {
			if !core.ValidThreadRoot(root) || !nonempty(path) {
				return errors.New("invalid thread workspace binding")
			}
			g.ThreadWorkspaces[root] = absolute(base, path)
		}
		for _, m := range g.Members {
			if !idPattern.MatchString(m) {
				return errors.New("invalid group member")
			}
		}
	}
	seen = map[string]bool{}
	for _, dm := range c.DMs {
		if !idPattern.MatchString(dm.UserID) || seen[dm.UserID] || !projects[dm.ProjectID] {
			return errors.New("invalid DM binding")
		}
		seen[dm.UserID] = true
		if len(dm.HistoryChats) > 32 {
			return errors.New("too many DM historyChats entries")
		}
		names := map[string]bool{}
		for _, h := range dm.HistoryChats {
			if h.ChatID == core.AnyGroupChatID || !idPattern.MatchString(h.ChatID) {
				return errors.New("invalid DM historyChats target")
			}
			if _, ok := c.Group(h.ChatID); !ok {
				return errors.New("DM historyChats target must be an explicit group binding")
			}
			if h.Name == "" || len([]rune(h.Name)) > 80 || strings.TrimSpace(h.Name) != h.Name {
				return errors.New("invalid DM historyChats name")
			}
			for _, r := range h.Name {
				if r < 0x20 || r == 0x7f {
					return errors.New("invalid DM historyChats name")
				}
			}
			if names[h.ChatID] || names[h.Name] {
				return errors.New("duplicate DM historyChats target or name")
			}
			names[h.ChatID] = true
			names[h.Name] = true
		}
	}
	checks := []struct {
		name        string
		v, min, max int64
	}{
		{"maxWorkers", int64(c.Runtime.MaxWorkers), 1, 128}, {"idleMs", c.Runtime.IdleMS, 0, 86400000},
		{"maxWorkersPerChat", int64(c.Runtime.MaxWorkersPerChat), 0, int64(c.Runtime.MaxWorkers)},
		{"maxRssMiB", c.Runtime.MaxRSSMiB, 0, 1048576}, {"idleMinMs", c.Runtime.IdleMinMS, 0, c.Runtime.IdleMS},
		{"backgroundMessages", int64(c.Limits.BackgroundMessages), 0, 200}, {"backgroundMs", c.Limits.BackgroundMS, 0, 86400000},
		{"textChars", int64(c.Limits.TextChars), 100, 200000}, {"inboxPerRoom", int64(c.Limits.InboxPerRoom), 1, 64},
		{"attachmentBytes", c.Limits.AttachmentBytes, 1024, 100 << 20}, {"approvalMs", c.Limits.ApprovalMS, 1000, 3600000},
		{"updateMs", c.Limits.UpdateMS, 250, 30000}, {"messagesPerMinute", int64(c.Limits.MessagesPerMinute), 1, 1000},
		{"imageBatchMs", c.Limits.ImageBatchMS, 0, 5000}, {"imageBatchMaxImages", int64(c.Limits.ImageBatchMaxImages), 2, 12}, {"imageBatchBytes", c.Limits.ImageBatchBytes, 1, 100 << 20}}
	for _, v := range checks {
		if v.v < v.min || v.v > v.max {
			return fmt.Errorf("%s: invalid integer", v.name)
		}
	}
	return nil
}
func ModelHeaders(c core.Config) (map[string]string, error) {
	h := map[string]string{}
	for name, source := range c.Model.ExtraHeaders {
		v := source
		if strings.HasPrefix(v, "env:") {
			v = os.Getenv(strings.TrimPrefix(v, "env:"))
		}
		if v == "" || strings.ContainsAny(v, "\r\n\x00") {
			return nil, fmt.Errorf("missing or invalid configured model header: %s", name)
		}
		h[name] = v
	}
	return h, nil
}
func Secrets(c core.Config) []string {
	result := []string{os.Getenv(c.AppSecretEnv), os.Getenv(c.Model.TokenEnv)}
	for _, p := range c.Projects {
		for _, name := range p.EnvPassthrough {
			// Passthrough values are credentials. Very short values are not
			// useful redaction markers: replacing a common short word
			// throughout diagnostics hides the events they describe.
			if value := os.Getenv(name); len(value) >= 8 {
				result = append(result, value)
			}
		}
		for _, a := range p.Actions {
			for _, name := range a.Env {
				result = append(result, os.Getenv(name))
			}
		}
	}
	for _, v := range c.Model.ExtraHeaders {
		if strings.HasPrefix(v, "env:") {
			v = os.Getenv(v[4:])
		}
		result = append(result, v)
	}
	return result
}
