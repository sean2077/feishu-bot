package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sean2077/feishu-bot/internal/config"
)

const configHelp = `Manage the feishu-bot JSON configuration.

Usage:
  feishu-bot config path [--config PATH]
  feishu-bot config example
  feishu-bot config format [--config PATH]
  feishu-bot config explain --chat CHAT_ID --user USER_ID [--chat-type group|p2p]
                           [--mentioned] [--bot] [--json] [--config PATH]

path resolves --config, then ./config.json, then ~/.feishu-bot/config.json.
example prints an annotated starter configuration.
format rewrites valid JSON with two-space indentation.
explain evaluates the runtime authorization/trigger policy without network or state writes.
`

func runConfigCommand(ctx context.Context, o Options, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch o.Subcommand {
	case "explain":
		return runConfigExplain(ctx, o, stdout)
	case "example":
		_, err := io.WriteString(stdout, config.Example)
		return err
	case "path":
		path, found, err := resolveConfigPath(o)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, path); err != nil {
			return err
		}
		if !found {
			_, _ = fmt.Fprintf(stderr, "configuration not found at %s\n", path)
		}
		return nil
	case "format", "fmt":
		path, found, err := resolveConfigPath(o)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("configuration is missing: %s", path)
		}
		return formatConfig(path)
	}
	return errors.New("unknown config command")
}

func formatConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read configuration")
	}
	if !json.Valid(data) {
		return errors.New("configuration is not valid JSON")
	}
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return errors.New("configuration could not be formatted")
	}
	out.WriteByte('\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return errors.New("cannot write configuration")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return errors.New("cannot secure configuration")
	}
	if _, err := tmp.Write(out.Bytes()); err != nil {
		tmp.Close()
		return errors.New("cannot write configuration")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("cannot flush configuration")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("cannot close configuration")
	}
	if err := os.Rename(name, path); err != nil {
		return errors.New("cannot replace configuration")
	}
	return nil
}
