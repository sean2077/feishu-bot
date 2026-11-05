package native

import "regexp"

// TestedVersion identifies the native CLI used by the protocol regression
// suite. Matching it is version evidence, not proof of a deployment's tools,
// model endpoint, permissions or live Feishu delivery.
const TestedVersion = protocolVersion

type VersionCompatibility struct {
	Actual string `json:"actual,omitempty"`
	Tested string `json:"tested"`
	Status string `json:"status"`
}

var nativeVersionPattern = regexp.MustCompile(`(?m)^([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?) \(Claude Code\)\r?$`)

// Compatibility accepts the native --version identity, without echoing any
// other stdout/stderr. Unknown releases remain usable but are not labelled as
// protocol-tested. An SDK or arbitrary command that merely mentions Claude
// Code is not a native CLI identity.
func Compatibility(output string) VersionCompatibility {
	r := VersionCompatibility{Tested: TestedVersion, Status: "unavailable"}
	matches := nativeVersionPattern.FindAllStringSubmatch(output, -1)
	if len(matches) != 1 {
		return r
	}
	r.Actual = matches[0][1]
	r.Status = "untested"
	if r.Actual == TestedVersion {
		r.Status = "tested"
	}
	return r
}
