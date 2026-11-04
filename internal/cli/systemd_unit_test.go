package cli

import (
	"strings"
	"testing"
)

func TestSystemdUnit(t *testing.T) {
	spec := serviceSpec{
		Name: "feishu-bot", Dir: "/opt/feishu bot",
		Binary: "/opt/feishu bot/bin/feishu-bot", Config: "/opt/feishu bot/config.json",
		EnvFile: "/opt/feishu bot/service.env", LogFile: "/opt/feishu bot/logs/service.log",
		LogMaxSize: 10, LogMaxBackups: 3,
	}
	unit := systemdUnit(spec, false)
	for _, want := range []string{
		`ExecStart="/opt/feishu bot/bin/feishu-bot" run --service --config "/opt/feishu bot/config.json"`,
		`--env-file "/opt/feishu bot/service.env"`,
		`--log-file "/opt/feishu bot/logs/service.log" --log-max-size 10 --log-max-backups 3`,
		`WorkingDirectory="/opt/feishu bot"`,
		`Restart=on-failure`,
		`TimeoutStopSec=60`,
		`WantedBy=multi-user.target`,
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
	user := systemdUnit(spec, true)
	if !strings.Contains(user, "WantedBy=default.target") {
		t.Fatalf("user unit has wrong target:\n%s", user)
	}
}

func TestSystemdQuote(t *testing.T) {
	if got, want := systemdQuote(`a b"c\d% e`), `"a b\"c\\d%% e"`; got != want {
		t.Fatalf("quote = %q, want %q", got, want)
	}
}
