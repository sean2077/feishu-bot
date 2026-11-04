//go:build windows

package cli

import (
	"os"
	"strings"
	"testing"
	"unicode/utf16"
)

func sampleSpec() serviceSpec {
	return serviceSpec{
		Name: "feishu-bot", Dir: `C:\work & dir`,
		Binary: `C:\work & dir\bin\feishu-bot.exe`, Config: `C:\work & dir\config.json`,
		EnvFile: `C:\work & dir\service.env`, LogFile: `C:\work & dir\logs\service.log`,
		LogMaxSize: 10, LogMaxBackups: 3,
	}
}

func TestTaskXML(t *testing.T) {
	spec := sampleSpec()
	text := taskXML(spec, `DOMAIN\user`)
	for _, want := range []string{
		`<Command>C:\work &amp; dir\bin\feishu-bot.exe</Command>`,
		`run --service --config `,
		`--env-file `,
		`--log-file `,
		`--log-max-size 10 --log-max-backups 3`,
		`<LogonTrigger>`,
		`<LogonType>InteractiveToken</LogonType>`,
		`<RunLevel>LeastPrivilege</RunLevel>`,
		`<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>`,
		`<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`,
		`<Hidden>true</Hidden>`,
		`DOMAIN\user`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("task XML missing %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"wscript", "powershell", ".vbs", ".ps1", "cmd.exe"} {
		if strings.Contains(strings.ToLower(text), bad) {
			t.Fatalf("task XML contains a launcher script reference %q", bad)
		}
	}
	if got := taskField(text, "Command"); got != spec.Binary {
		t.Fatalf("decoded command = %q", got)
	}
	arguments := taskField(text, "Arguments")
	for _, want := range []string{"--service", `--config "C:\work & dir\config.json"`, `--env-file "C:\work & dir\service.env"`, `--log-file "C:\work & dir\logs\service.log"`} {
		if !strings.Contains(arguments, want) {
			t.Fatalf("decoded arguments %q missing %q", arguments, want)
		}
	}
	if !taskOwned(text, spec) {
		t.Fatal("own task was not recognized")
	}
	other := spec
	other.Binary = `C:\other\feishu-bot.exe`
	if taskOwned(text, other) {
		t.Fatal("a task with a different binary was treated as owned")
	}
}

func TestWriteTaskXMLUsesUTF16(t *testing.T) {
	path, err := writeTaskXML(taskXML(sampleSpec(), `DOMAIN\user`))
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xFE {
		t.Fatal("task XML is missing the UTF-16LE BOM")
	}
	units := make([]uint16, 0, (len(data)-2)/2)
	for i := 2; i+1 < len(data); i += 2 {
		units = append(units, uint16(data[i])|uint16(data[i+1])<<8)
	}
	decoded := string(utf16.Decode(units))
	if !strings.Contains(decoded, `<Task version="1.4"`) {
		t.Fatal("task XML was not encoded as UTF-16LE")
	}
}
