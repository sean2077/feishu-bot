//go:build windows

package cli

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"unicode/utf16"
)

type windowsManager struct{}

func platformServiceManager() (serviceManager, error) {
	return windowsManager{}, nil
}

func (windowsManager) Platform() string { return "schtasks" }

func (m windowsManager) Check(ctx context.Context, spec serviceSpec, replace bool) error {
	present, err := m.Present(ctx, spec.Name)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	existing, err := queryTaskXML(ctx, spec.Name)
	if err != nil {
		return errors.New("existing scheduled task could not be inspected")
	}
	if taskOwned(existing, spec) {
		return nil
	}
	if replace {
		return nil
	}
	return errors.New("a task named feishu-bot already exists with a different definition; stop/remove it or rerun with --replace")
}

func (m windowsManager) Install(ctx context.Context, spec serviceSpec, replace bool) error {
	if err := m.Check(ctx, spec, replace); err != nil {
		return err
	}
	username, err := windowsUser()
	if err != nil {
		return err
	}
	path, err := writeTaskXML(taskXML(spec, username))
	if err != nil {
		return err
	}
	defer os.Remove(path)
	if _, err := runTool(ctx, "schtasks", "/Create", "/TN", spec.Name, "/XML", path, "/F"); err != nil {
		if detail := toolDetail(err); detail != "" {
			return fmt.Errorf("cannot register scheduled task: %s", detail)
		}
		return errors.New("cannot register scheduled task")
	}
	registered, err := queryTaskXML(ctx, spec.Name)
	if err != nil {
		return errors.New("scheduled task could not be verified")
	}
	if !taskOwned(registered, spec) {
		return errors.New("registered scheduled task does not match the requested service definition")
	}
	return nil
}

func (m windowsManager) Start(ctx context.Context, name string) error {
	// daemon start is an explicit request; enabling a task disabled by the user
	// is expected, but failures are ignored because an enabled task is fine.
	_, _ = runTool(ctx, "schtasks", "/Change", "/TN", name, "/ENABLE")
	if _, err := runTool(ctx, "schtasks", "/Run", "/TN", name); err != nil {
		if detail := toolDetail(err); detail != "" {
			return fmt.Errorf("cannot start scheduled task: %s", detail)
		}
		return errors.New("cannot start scheduled task")
	}
	return nil
}

func (m windowsManager) Uninstall(ctx context.Context, name string) error {
	present, err := m.Present(ctx, name)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if _, err := runTool(ctx, "schtasks", "/Delete", "/TN", name, "/F"); err != nil {
		return errors.New("cannot remove scheduled task")
	}
	return nil
}

func (m windowsManager) Present(ctx context.Context, name string) (bool, error) {
	_, err := runTool(ctx, "schtasks", "/Query", "/TN", name)
	if err == nil {
		return true, nil
	}
	var tool *toolError
	if errors.As(err, &tool) {
		return false, nil
	}
	return false, err
}

func windowsUser() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", errors.New("cannot determine the current Windows user")
	}
	name := current.Username
	if !strings.Contains(name, `\`) {
		if domain := os.Getenv("USERDOMAIN"); domain != "" {
			name = domain + `\` + name
		}
	}
	if strings.TrimSpace(name) == "" {
		return "", errors.New("cannot determine the current Windows user")
	}
	return name, nil
}

func taskXML(spec serviceSpec, username string) string {
	args := fmt.Sprintf(`run --service --config "%s" --env-file "%s" --log-file "%s" --log-max-size %d --log-max-backups %d`,
		spec.Config, spec.EnvFile, spec.LogFile, spec.LogMaxSize, spec.LogMaxBackups)
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>feishu-bot native service</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + xmlEscape(username) + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + xmlEscape(username) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>true</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>3</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscape(spec.Binary) + `</Command>
      <Arguments>` + xmlEscape(args) + `</Arguments>
      <WorkingDirectory>` + xmlEscape(spec.Dir) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>`
}

func xmlEscape(value string) string {
	var out strings.Builder
	_ = xml.EscapeText(&out, []byte(value))
	return out.String()
}

func writeTaskXML(text string) (string, error) {
	file, err := os.CreateTemp("", "feishu-bot-task-*.xml")
	if err != nil {
		return "", errors.New("cannot create a temporary task definition")
	}
	name := file.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	encoded := utf16.Encode([]rune(text))
	data := make([]byte, 0, 2+len(encoded)*2)
	data = append(data, 0xFF, 0xFE)
	for _, unit := range encoded {
		data = append(data, byte(unit), byte(unit>>8))
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return "", errors.New("cannot write the temporary task definition")
	}
	if err := file.Close(); err != nil {
		return "", errors.New("cannot close the temporary task definition")
	}
	ok = true
	return name, nil
}

func queryTaskXML(ctx context.Context, name string) (string, error) {
	output, err := runTool(ctx, "schtasks", "/Query", "/TN", name, "/XML")
	if err != nil {
		return "", err
	}
	return output, nil
}

func taskOwned(text string, spec serviceSpec) bool {
	command := taskField(text, "Command")
	arguments := taskField(text, "Arguments")
	return command != "" && samePath(command, spec.Binary) &&
		strings.Contains(arguments, "--service") &&
		strings.Contains(arguments, "--config") &&
		strings.Contains(arguments, "--env-file") &&
		strings.Contains(arguments, "--log-file")
}

func taskField(text, local string) string {
	// schtasks output and generated files may declare UTF-16; the caller
	// supplies decoded text, so skip the declaration before tokenizing.
	if index := strings.Index(text, "<Task"); index >= 0 {
		text = text[index:]
	}
	decoder := xml.NewDecoder(strings.NewReader(text))
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == local {
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return ""
			}
			return value
		}
	}
}
