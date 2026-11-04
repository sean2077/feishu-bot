package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

type Options struct {
	Command, ConfigPath string
	Probe, ProbeFeishu  bool
	ActionArgs          []string
}

func Parse(args []string) (Options, error) {
	o := Options{Command: "help", ConfigPath: "config.json"}
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
	switch o.Command {
	case "help", "init", "doctor", "run":
	default:
		return o, errors.New("unknown command; use --help")
	}
	configSeen := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--config" || strings.HasPrefix(arg, "--config="):
			if configSeen {
				return o, errors.New("--config may only be specified once")
			}
			configSeen = true
			value := ""
			if arg == "--config" {
				i++
				if i < len(args) {
					value = args[i]
				}
			} else {
				value = strings.TrimPrefix(arg, "--config=")
			}
			if strings.TrimSpace(value) == "" || strings.HasPrefix(value, "-") || strings.ContainsRune(value, 0) {
				return o, errors.New("--config requires a file path")
			}
			o.ConfigPath = value
		case arg == "--probe" && o.Command == "doctor" && !o.Probe:
			o.Probe = true
		case arg == "--probe-feishu" && o.Command == "doctor" && !o.ProbeFeishu:
			o.ProbeFeishu = true
		case arg == "--help" || arg == "-h":
			o.Command = "help"
			o.Probe = false
			o.ProbeFeishu = false
			return o, nil
		default:
			return o, fmt.Errorf("unexpected option; --probe and --probe-feishu are only supported by doctor")
		}
	}
	abs, err := filepath.Abs(o.ConfigPath)
	o.ConfigPath = abs
	return o, err
}
