package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/gateway"
)

func parseExplainOption(args []string, i *int, o *Options, seen map[string]bool) (bool, error) {
	arg := args[*i]
	for name, dst := range map[string]*string{"--chat": &o.ExplainChat, "--user": &o.ExplainUser, "--chat-type": &o.ChatType} {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			if err := mark(seen, name); err != nil {
				return true, err
			}
			value, _, err := takeValue(args, i, arg, name)
			if err != nil {
				return true, err
			}
			*dst = value
			return true, nil
		}
	}
	for name, dst := range map[string]*bool{"--mentioned": &o.Mentioned, "--bot": &o.SenderBot, "--json": &o.JSON} {
		if arg == name {
			if err := mark(seen, name); err != nil {
				return true, err
			}
			*dst = true
			return true, nil
		}
	}
	return false, nil
}

// explainRoute calls the runtime's room, author and trigger policies. It never
// constructs a transport, opens the state database or prepares a workspace.
func explainRoute(c core.Config, o Options) core.Args {
	m := core.Message{ID: "offline-config-explain", ChatID: o.ExplainChat, ChatType: o.ChatType, SenderID: o.ExplainUser, Mentioned: o.Mentioned, SenderIsBot: o.SenderBot}
	dm := ""
	if m.ChatType == "p2p" {
		dm = m.SenderID
	}
	r, bound := c.RoomFor(m.ChatID, dm)
	_, admitted := gateway.Route(c, m)
	binding := "none"
	if bound {
		binding = "default_group"
		if dm != "" {
			binding = "dm"
		} else {
			for _, g := range c.Groups {
				if g.ChatID == m.ChatID {
					binding = "explicit_group"
					break
				}
			}
		}
	}
	authorized := bound && r.Authorizes(m.SenderID)
	trigger := admitted && gateway.Triggers(r, m)
	maintainer := false
	for _, id := range r.Maintainers {
		maintainer = maintainer || id == m.SenderID
	}
	manager := admitted && (maintainer || r.DMUser == m.SenderID)
	reason := "admitted"
	switch {
	case m.SenderIsBot:
		reason = "bot_sender"
	case !bound:
		reason = "no_binding"
	case !authorized:
		reason = "author_not_allowed"
	case !trigger:
		reason = "mention_required"
	}
	view := core.Args{"scope": "offline_config_policy", "chat": m.ChatID, "chatType": m.ChatType, "user": m.SenderID, "binding": binding, "authorAuthorized": authorized, "eventAdmitted": admitted, "triggersNative": trigger, "canManageSession": manager, "canApproveTools": admitted && maintainer, "reason": reason, "liveMembershipVerified": false}
	if bound {
		view["project"] = r.ProjectID
		view["roomKey"] = r.Key
		view["allowAllMembers"] = r.AllowAllMembers
		view["mentionOptional"] = r.MentionOptional
		if r.DMUser == "" {
			view["sessionScope"] = r.SessionScope
		}
		if p, ok := c.Project(r.ProjectID); ok {
			view["permissionMode"] = p.PermissionMode
			view["nativeConfig"] = p.NativeConfig
			view["projectEnv"] = sortedNames(p.Env)
			view["projectEnvPassthrough"] = sortedPassthrough(p.EnvPassthrough)
			view["projectEnvPassthroughMissing"] = missingPassthrough(p.EnvPassthrough)
		}
	}
	return view
}

func sortedNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedPassthrough(values []string) []string {
	names := append([]string{}, values...)
	sort.Strings(names)
	return names
}

func missingPassthrough(values []string) []string {
	missing := []string{}
	for _, name := range sortedPassthrough(values) {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

func listNames(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	return strings.Join(values, ", ")
}

func runConfigExplain(ctx context.Context, o Options, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, found, err := resolveConfigPath(o)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("configuration is missing")
	}
	c, err := config.Load(path)
	if err != nil {
		var envErr *config.ProjectEnvError
		if errors.As(err, &envErr) {
			// This error carries only a project and variable name, never a value.
			return fmt.Errorf("configuration is invalid: %w", envErr)
		}
		return errors.New("configuration cannot be loaded or validated; use doctor for configuration diagnostics")
	}
	view := explainRoute(c, o)
	if o.JSON {
		data, err := json.MarshalIndent(view, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(data))
		return err
	}
	envDetail := ""
	if names, ok := view["projectEnv"].([]string); ok {
		passthrough, _ := view["projectEnvPassthrough"].([]string)
		missing, _ := view["projectEnvPassthroughMissing"].([]string)
		envDetail = fmt.Sprintf("Project env names: %s\nProject env passthrough: %s\nMissing passthrough: %s\n", listNames(names), listNames(passthrough), listNames(missing))
	}
	_, err = fmt.Fprintf(out, "Configuration policy (offline)\nBinding: %v\nProject: %v\nAuthor authorized: %v\nEvent admitted: %v\nTriggers native input: %v\nManage session: %v\nApprove tools: %v\nReason: %v\n%sThis evaluates configured policy only; it does not verify live membership, credentials or Feishu delivery.\n", view["binding"], view["project"], view["authorAuthorized"], view["eventAdmitted"], view["triggersNative"], view["canManageSession"], view["canApproveTools"], view["reason"], envDetail)
	return err
}
