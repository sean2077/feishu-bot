package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

type Options struct {
	Command, Subcommand                string
	ConfigPath                         string
	ConfigExplicit                     bool
	Probe, ProbeFeishu                 bool
	VerifyChat                         string
	ActionArgs                         []string
	FeedbackValue, FeedbackSince       string
	FeedbackProject                    string
	ExplainChat, ExplainUser, ChatType string
	Mentioned, SenderBot               bool
	SessionID                          string

	AttachUser, AttachTitle, AttachSummaryFile string
	AttachNative                               string
	AttachHolderPID                            int
	AttachHolderSet                            bool
	TopicMessageID, TopicTextFile              string

	EnvFile  string
	LogFile  string
	WorkDir  string
	Binary   string
	Replace  bool
	NoStart  bool
	Reclaim  bool
	Follow   bool
	JSON     bool
	Service  bool
	LogSize  int
	LogKeep  int
	LogLines int
}

func Parse(args []string) (Options, error) {
	o := Options{Command: "help", ConfigPath: "config.json", LogSize: 10, LogKeep: 3, LogLines: 100}
	if len(args) == 0 {
		return o, nil
	}
	o.Command = args[0]
	if o.Command == "--help" || o.Command == "-h" {
		o.Command = "help"
	}
	if o.Command == "--version" || o.Command == "version" {
		o.Command = "version"
		if len(args) > 1 {
			return o, errors.New("unexpected version argument")
		}
		return o, nil
	}
	if o.Command == "action" {
		o.ActionArgs = args[1:]
		return o, nil
	}
	if o.Command == "context" {
		if len(args) > 1 {
			return o, errors.New("unexpected context argument")
		}
		return o, nil
	}
	if o.Command == "feedback" {
		if len(args) < 2 || args[1] == "--help" || args[1] == "-h" {
			o.Command = "help"
			o.Subcommand = "feedback"
			return o, nil
		}
		o.Subcommand = args[1]
		if o.Subcommand != "export" {
			return o, errors.New("unknown feedback command; use feishu-bot feedback --help")
		}
		return parseFeedback(args[2:], o)
	}
	if o.Command == "session" {
		if len(args) < 2 || args[1] == "--help" || args[1] == "-h" {
			o.Command = "help"
			o.Subcommand = "session"
			return o, nil
		}
		o.Subcommand = args[1]
		if o.Subcommand != "run" {
			return o, errors.New("unknown session command; use feishu-bot session --help")
		}
		return parseSession(args[2:], o)
	}
	if o.Command == "daemon" {
		if len(args) < 2 || args[1] == "--help" || args[1] == "-h" {
			o.Command = "help"
			o.Subcommand = "daemon"
			return o, nil
		}
		o.Subcommand = args[1]
		switch o.Subcommand {
		case "install", "uninstall", "start", "stop", "restart", "status", "logs":
		default:
			return o, errors.New("unknown daemon command; use feishu-bot daemon --help")
		}
		return parseDaemon(args[2:], o)
	}
	if o.Command == "config" {
		if len(args) < 2 || args[1] == "--help" || args[1] == "-h" {
			o.Command = "help"
			o.Subcommand = "config"
			return o, nil
		}
		o.Subcommand = args[1]
		switch o.Subcommand {
		case "path", "example", "format", "fmt", "explain":
		default:
			return o, errors.New("unknown config command; use feishu-bot config --help")
		}
		return parseConfig(args[2:], o)
	}
	if o.Command == "attach" {
		return parseAdoptFlags(args[1:], o, "attach")
	}
	if o.Command == "notify" {
		return parseAdoptFlags(args[1:], o, "notify")
	}
	if o.Command == "topic" {
		if len(args) < 2 || args[1] == "--help" || args[1] == "-h" {
			o.Command = "help"
			o.Subcommand = "topic"
			return o, nil
		}
		o.Subcommand = args[1]
		if o.Subcommand != "reply" {
			return o, errors.New("unknown topic command; use feishu-bot topic --help")
		}
		return parseTopicReply(args[2:], o)
	}
	switch o.Command {
	case "help", "init", "doctor", "run":
	default:
		return o, errors.New("unknown command; use --help")
	}
	if o.Command == "help" {
		return o, nil
	}
	return parseTop(args[1:], o)
}

func takeValue(args []string, index *int, arg, name string) (string, bool, error) {
	if arg == name {
		*index++
		if *index >= len(args) {
			return "", true, fmt.Errorf("%s requires a value", name)
		}
		value := args[*index]
		if strings.TrimSpace(value) == "" || strings.HasPrefix(value, "-") || strings.ContainsRune(value, 0) {
			return "", true, fmt.Errorf("%s requires a value", name)
		}
		return value, true, nil
	}
	if strings.HasPrefix(arg, name+"=") {
		value := strings.TrimPrefix(arg, name+"=")
		if strings.TrimSpace(value) == "" || strings.HasPrefix(value, "-") || strings.ContainsRune(value, 0) {
			return "", true, fmt.Errorf("%s requires a value", name)
		}
		return value, true, nil
	}
	return "", false, nil
}

func mark(seen map[string]bool, name string) error {
	if seen[name] {
		return fmt.Errorf("%s may only be specified once", name)
	}
	seen[name] = true
	return nil
}

func resolveOptions(o *Options) (Options, error) {
	abs, err := filepath.Abs(o.ConfigPath)
	if err != nil {
		return *o, err
	}
	o.ConfigPath = abs
	for name, value := range map[string]*string{"--env-file": &o.EnvFile, "--log-file": &o.LogFile, "--work-dir": &o.WorkDir, "--binary": &o.Binary} {
		if *value == "" {
			continue
		}
		abs, err := filepath.Abs(*value)
		if err != nil {
			return *o, fmt.Errorf("%s path is invalid", name)
		}
		*value = abs
	}
	return *o, nil
}

func parseTop(args []string, o Options) (Options, error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--help" || arg == "-h":
			o.Command = "help"
			o.Probe = false
			o.ProbeFeishu = false
			o.VerifyChat = ""
			return o, nil
		case arg == "--config" || strings.HasPrefix(arg, "--config="):
			if err := mark(seen, "--config"); err != nil {
				return o, errors.New("--config may only be specified once")
			}
			value, _, err := takeValue(args, &i, arg, "--config")
			if err != nil {
				return o, errors.New("--config requires a file path")
			}
			o.ConfigPath = value
			o.ConfigExplicit = true
		case arg == "--env-file" || strings.HasPrefix(arg, "--env-file="):
			if o.Command != "doctor" && o.Command != "run" {
				return o, errors.New("--env-file is only supported by doctor, run and daemon install")
			}
			if err := mark(seen, "--env-file"); err != nil {
				return o, err
			}
			value, _, err := takeValue(args, &i, arg, "--env-file")
			if err != nil {
				return o, err
			}
			o.EnvFile = value
		case arg == "--probe" && o.Command == "doctor":
			if o.Probe {
				return o, errors.New("--probe may only be specified once")
			}
			o.Probe = true
		case arg == "--probe-feishu" && o.Command == "doctor":
			if o.ProbeFeishu {
				return o, errors.New("--probe-feishu may only be specified once")
			}
			o.ProbeFeishu = true
		case (arg == "--verify-chat" || strings.HasPrefix(arg, "--verify-chat=")) && o.Command == "doctor":
			if err := mark(seen, "--verify-chat"); err != nil {
				return o, err
			}
			value, _, err := takeValue(args, &i, arg, "--verify-chat")
			if err != nil {
				return o, err
			}
			o.VerifyChat = value
		case (arg == "--log-file" || strings.HasPrefix(arg, "--log-file=")) && o.Command == "run":
			if err := mark(seen, "--log-file"); err != nil {
				return o, err
			}
			value, _, err := takeValue(args, &i, arg, "--log-file")
			if err != nil {
				return o, err
			}
			o.LogFile = value
		case (arg == "--log-max-size" || strings.HasPrefix(arg, "--log-max-size=")) && o.Command == "run":
			if err := mark(seen, "--log-max-size"); err != nil {
				return o, err
			}
			value, _, err := takeValue(args, &i, arg, "--log-max-size")
			if err != nil {
				return o, err
			}
			number, err := strconv.Atoi(value)
			if err != nil || number <= 0 {
				return o, errors.New("--log-max-size requires a positive number of MB")
			}
			o.LogSize = number
		case (arg == "--log-max-backups" || strings.HasPrefix(arg, "--log-max-backups=")) && o.Command == "run":
			if err := mark(seen, "--log-max-backups"); err != nil {
				return o, err
			}
			value, _, err := takeValue(args, &i, arg, "--log-max-backups")
			if err != nil {
				return o, err
			}
			number, err := strconv.Atoi(value)
			if err != nil || number < 0 {
				return o, errors.New("--log-max-backups requires a non-negative number")
			}
			o.LogKeep = number
		case arg == "--service" && o.Command == "run":
			if o.Service {
				return o, errors.New("--service may only be specified once")
			}
			o.Service = true
		default:
			return o, fmt.Errorf("unexpected option; --probe, --probe-feishu and --verify-chat are only supported by doctor")
		}
	}
	if o.LogFile == "" && (seen["--log-max-size"] || seen["--log-max-backups"]) {
		return o, errors.New("--log-max-size and --log-max-backups require --log-file")
	}
	if o.Service && o.LogFile == "" {
		return o, errors.New("--service requires --log-file")
	}
	return resolveOptions(&o)
}

func parseAdoptFlags(args []string, o Options, sub string) (Options, error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" {
			o.Command = "help"
			o.Subcommand = sub
			return o, nil
		}
		matched := false
		for name, dst := range map[string]*string{"--config": &o.ConfigPath, "--user": &o.AttachUser, "--title": &o.AttachTitle, "--native": &o.AttachNative} {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				if err := mark(seen, name); err != nil {
					return o, err
				}
				value, _, err := takeValue(args, &i, arg, name)
				if err != nil {
					return o, err
				}
				*dst = value
				if name == "--config" {
					o.ConfigExplicit = true
				}
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		switch {
		case arg == "--summary-file" || strings.HasPrefix(arg, "--summary-file="):
			if err := mark(seen, "--summary-file"); err != nil {
				return o, err
			}
			value := strings.TrimPrefix(arg, "--summary-file=")
			if arg == "--summary-file" {
				i++
				if i >= len(args) {
					return o, errors.New("--summary-file requires a path or -")
				}
				value = args[i]
			}
			if value == "" || strings.ContainsRune(value, 0) || value != "-" && strings.TrimSpace(value) == "" {
				return o, errors.New("--summary-file requires a path or -")
			}
			o.AttachSummaryFile = value
		case arg == "--holder-pid" || strings.HasPrefix(arg, "--holder-pid="):
			if err := mark(seen, "--holder-pid"); err != nil {
				return o, err
			}
			value, _, err := takeValue(args, &i, arg, "--holder-pid")
			if err != nil {
				return o, err
			}
			pid, err := strconv.Atoi(value)
			if err != nil || pid < 0 {
				return o, errors.New("--holder-pid requires a non-negative process ID")
			}
			o.AttachHolderPID = pid
			o.AttachHolderSet = true
		case arg == "--json":
			if o.JSON {
				return o, errors.New("--json may only be specified once")
			}
			o.JSON = true
		default:
			return o, fmt.Errorf("unexpected option; see feishu-bot %s --help", sub)
		}
	}
	return resolveOptions(&o)
}

func parseAttach(args []string, o Options) (Options, error) {
	return parseAdoptFlags(args, o, "attach")
}

func parseTopicReply(args []string, o Options) (Options, error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" {
			o.Command = "help"
			o.Subcommand = "topic"
			return o, nil
		}
		matched := false
		for name, dst := range map[string]*string{"--config": &o.ConfigPath, "--native": &o.AttachNative, "--message-id": &o.TopicMessageID} {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				if err := mark(seen, name); err != nil {
					return o, err
				}
				value, _, err := takeValue(args, &i, arg, name)
				if err != nil {
					return o, err
				}
				*dst = value
				if name == "--config" {
					o.ConfigExplicit = true
				}
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		switch {
		case arg == "--text-file" || strings.HasPrefix(arg, "--text-file="):
			if err := mark(seen, "--text-file"); err != nil {
				return o, err
			}
			value := strings.TrimPrefix(arg, "--text-file=")
			if arg == "--text-file" {
				i++
				if i >= len(args) {
					return o, errors.New("--text-file requires a path or -")
				}
				value = args[i]
			}
			if value == "" || strings.ContainsRune(value, 0) || value != "-" && strings.TrimSpace(value) == "" {
				return o, errors.New("--text-file requires a path or -")
			}
			o.TopicTextFile = value
		case arg == "--json":
			if o.JSON {
				return o, errors.New("--json may only be specified once")
			}
			o.JSON = true
		default:
			return o, errors.New("unexpected option; see feishu-bot topic reply --help")
		}
	}
	return resolveOptions(&o)
}

func parseDaemon(args []string, o Options) (Options, error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" {
			o.Command = "help"
			o.Subcommand = "daemon"
			return o, nil
		}
		switch o.Subcommand {
		case "install":
			switch {
			case arg == "--config" || strings.HasPrefix(arg, "--config="):
				if err := mark(seen, "--config"); err != nil {
					return o, errors.New("--config may only be specified once")
				}
				value, _, err := takeValue(args, &i, arg, "--config")
				if err != nil {
					return o, errors.New("--config requires a file path")
				}
				o.ConfigPath = value
				o.ConfigExplicit = true
			case arg == "--env-file" || strings.HasPrefix(arg, "--env-file="):
				if err := mark(seen, "--env-file"); err != nil {
					return o, err
				}
				value, _, err := takeValue(args, &i, arg, "--env-file")
				if err != nil {
					return o, err
				}
				o.EnvFile = value
			case arg == "--work-dir" || strings.HasPrefix(arg, "--work-dir="):
				if err := mark(seen, "--work-dir"); err != nil {
					return o, err
				}
				value, _, err := takeValue(args, &i, arg, "--work-dir")
				if err != nil {
					return o, err
				}
				o.WorkDir = value
			case arg == "--log-file" || strings.HasPrefix(arg, "--log-file="):
				if err := mark(seen, "--log-file"); err != nil {
					return o, err
				}
				value, _, err := takeValue(args, &i, arg, "--log-file")
				if err != nil {
					return o, err
				}
				o.LogFile = value
			case arg == "--log-max-size" || strings.HasPrefix(arg, "--log-max-size="):
				if err := mark(seen, "--log-max-size"); err != nil {
					return o, err
				}
				value, _, err := takeValue(args, &i, arg, "--log-max-size")
				if err != nil {
					return o, err
				}
				number, err := strconv.Atoi(value)
				if err != nil || number <= 0 {
					return o, errors.New("--log-max-size requires a positive number of MB")
				}
				o.LogSize = number
			case arg == "--log-max-backups" || strings.HasPrefix(arg, "--log-max-backups="):
				if err := mark(seen, "--log-max-backups"); err != nil {
					return o, err
				}
				value, _, err := takeValue(args, &i, arg, "--log-max-backups")
				if err != nil {
					return o, err
				}
				number, err := strconv.Atoi(value)
				if err != nil || number < 0 {
					return o, errors.New("--log-max-backups requires a non-negative number")
				}
				o.LogKeep = number
			case arg == "--binary" || strings.HasPrefix(arg, "--binary="):
				if err := mark(seen, "--binary"); err != nil {
					return o, err
				}
				value, _, err := takeValue(args, &i, arg, "--binary")
				if err != nil {
					return o, err
				}
				o.Binary = value
			case arg == "--replace":
				if o.Replace {
					return o, errors.New("--replace may only be specified once")
				}
				o.Replace = true
			case arg == "--no-start":
				if o.NoStart {
					return o, errors.New("--no-start may only be specified once")
				}
				o.NoStart = true
			default:
				return o, errors.New("unexpected option; see feishu-bot daemon --help")
			}
		case "start":
			if arg == "--reclaim-stale-lock" {
				if o.Reclaim {
					return o, errors.New("--reclaim-stale-lock may only be specified once")
				}
				o.Reclaim = true
				continue
			}
			return o, errors.New("unexpected option; see feishu-bot daemon --help")
		case "status":
			if arg == "--json" {
				if o.JSON {
					return o, errors.New("--json may only be specified once")
				}
				o.JSON = true
				continue
			}
			return o, errors.New("unexpected option; see feishu-bot daemon --help")
		case "logs":
			switch {
			case arg == "-f" || arg == "--follow":
				if o.Follow {
					return o, errors.New("--follow may only be specified once")
				}
				o.Follow = true
			case arg == "-n" || strings.HasPrefix(arg, "-n="):
				if err := mark(seen, "-n"); err != nil {
					return o, err
				}
				value, _, err := takeValue(args, &i, arg, "-n")
				if err != nil {
					return o, err
				}
				number, err := strconv.Atoi(value)
				if err != nil || number < 0 {
					return o, errors.New("-n requires a non-negative number")
				}
				o.LogLines = number
			case arg == "--log-file" || strings.HasPrefix(arg, "--log-file="):
				if err := mark(seen, "--log-file"); err != nil {
					return o, err
				}
				value, _, err := takeValue(args, &i, arg, "--log-file")
				if err != nil {
					return o, err
				}
				o.LogFile = value
			default:
				return o, errors.New("unexpected option; see feishu-bot daemon --help")
			}
		default:
			return o, errors.New("unexpected option; see feishu-bot daemon --help")
		}
	}
	return resolveOptions(&o)
}

func parseConfig(args []string, o Options) (Options, error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" {
			o.Command = "help"
			o.Subcommand = "config"
			return o, nil
		}
		if o.Subcommand == "example" {
			return o, errors.New("unexpected option; see feishu-bot config --help")
		}
		if o.Subcommand == "explain" {
			matched, err := parseExplainOption(args, &i, &o, seen)
			if err != nil {
				return o, err
			}
			if matched {
				continue
			}
		}
		if arg == "--config" || strings.HasPrefix(arg, "--config=") {
			if err := mark(seen, "--config"); err != nil {
				return o, errors.New("--config may only be specified once")
			}
			value, _, err := takeValue(args, &i, arg, "--config")
			if err != nil {
				return o, errors.New("--config requires a file path")
			}
			o.ConfigPath = value
			o.ConfigExplicit = true
			continue
		}
		return o, errors.New("unexpected option; see feishu-bot config --help")
	}
	if o.Subcommand == "explain" {
		if o.ExplainChat == "" || o.ExplainUser == "" {
			return o, errors.New("config explain requires --chat and --user")
		}
		if o.ChatType == "" {
			o.ChatType = "group"
		}
		if o.ChatType != "group" && o.ChatType != "p2p" {
			return o, errors.New("--chat-type must be group or p2p")
		}
	}
	return resolveOptions(&o)
}
