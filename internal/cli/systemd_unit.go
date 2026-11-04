package cli

import (
	"fmt"
	"strings"
)

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, `%`, `%%`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return `"` + value + `"`
}

func systemdUnit(spec serviceSpec, user bool) string {
	execStart := systemdQuote(spec.Binary) +
		" run --service --config " + systemdQuote(spec.Config) +
		" --env-file " + systemdQuote(spec.EnvFile) +
		" --log-file " + systemdQuote(spec.LogFile) +
		fmt.Sprintf(" --log-max-size %d --log-max-backups %d", spec.LogMaxSize, spec.LogMaxBackups)
	wanted := "multi-user.target"
	if user {
		wanted = "default.target"
	}
	return `[Unit]
Description=feishu-bot native service
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
WorkingDirectory=` + systemdQuote(spec.Dir) + `
ExecStart=` + execStart + `
Restart=on-failure
RestartSec=5
TimeoutStopSec=60
KillMode=mixed

[Install]
WantedBy=` + wanted + `
`
}
