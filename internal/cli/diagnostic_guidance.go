package cli

import "strings"

func diagnosticRemedy(name string) string {
	switch {
	case name == "configuration values":
		return "Replace the named template fields in the configuration; config example prints the supported shape."
	case name == "chat bindings":
		return "Configure an authorized group and its member allowlist, or an authorized DM user; keep unrelated chats unbound."
	case strings.HasPrefix(name, "credential "):
		return "Set the named variable in the service environment or an explicit --env-file; do not put its value in chat or command arguments."
	case strings.HasPrefix(name, "project ") && strings.Contains(name, " credential "):
		return "Provide the declared projects[].envPassthrough variable in the service environment or an explicit --env-file. An existing service.env is not updated from the current environment; pass --env-file to add new variables, then reinstall."
	case strings.HasPrefix(name, "project ") && strings.Contains(name, " env "):
		return "Move the credential to projects[].envPassthrough and keep only non-secret literals in projects[].env; diagnostics never print values."
	case name == "Git":
		return "Install Git and make it visible to the service account. On Windows, use Git for Windows."
	case name == "Claude Code":
		return "Install native Claude Code or correct runtime.claudeExecutable, then rerun doctor under the service account."
	case name == "configured Git Bash":
		return "Set CLAUDE_CODE_GIT_BASH_PATH to Git for Windows bash.exe in the service environment."
	case name == "data directory / SQLite":
		return "Check service-account access and free disk space for dataDir. Preserve the existing database and native history."
	case name == "feedback export directory":
		return "Create feedback.export.dir as a real directory writable by the service account; do not use a symbolic link. Feedback retention stays paused until a scheduled archive succeeds."
	case name == "Feishu identity probe":
		return "Check app ID, secret variable and app availability first; distinguish feishuAuth from feishuIdentity. Verify DNS, proxy and CA trust for network failures."
	case name == "model endpoint basic probe":
		return "Check the Messages API origin, configured model, credential variable and headers. HTTP success alone does not verify streaming or native tools."
	case name == "model endpoint headers":
		return "Resolve the configured header environment variables and replace template values without printing credentials."
	default:
		return "Check the named configured path and service-account access, then rerun the same local check."
	}
}

func diagnosticNextSteps(r DoctorReport, probe, probeFeishu bool) []string {
	steps := []string{}
	if r.Verification["local"] != "passed" {
		steps = append(steps, "Resolve failed local checks using their remedy fields, then rerun doctor with the same config and service environment.")
	}
	if r.Native.Status == "untested" {
		steps = append(steps, "The actual Claude Code version differs from the tested fixture. Run the native protocol regression suite for this candidate before upgrading production; a version check does not execute that suite.")
	}
	if !probeFeishu {
		steps = append(steps, "Explicitly run doctor --probe-feishu to verify application authentication and bot identity.")
	} else if r.Verification["feishuIdentity"] != "passed" {
		steps = append(steps, "Resolve the Feishu authentication/identity failure before testing incoming messages.")
	}
	if !probe {
		steps = append(steps, "Optionally run doctor --probe for a potentially billable basic Messages response; streaming and tools remain unverified.")
	}
	steps = append(steps,
		"Confirm the intended group/DM and member bindings, then run the service in the authorized test chat described in docs/first-connection.md.",
		"In that test chat, verify a mentioned message, streaming final answer, question/approval callback, attachment, stop, reconnect and original-session resume. Record live acceptance separately from this report.")
	return steps
}
