package gateway

import "github.com/sean2077/feishu-bot/internal/core"

// Redact strings before JSON encoding so quotes and other JSON escapes cannot
// hide configured secret values from audit and card redaction.
func (g *Gateway) redactValue(value any) any {
	switch v := value.(type) {
	case string:
		return core.Redact(v, g.secrets)
	case core.Args:
		return g.redactArgs(v)
	case []string:
		copy := make([]string, len(v))
		for i, item := range v {
			copy[i] = core.Redact(item, g.secrets)
		}
		return copy
	case []any:
		copy := make([]any, len(v))
		for i, item := range v {
			copy[i] = g.redactValue(item)
		}
		return copy
	default:
		return value
	}
}
func (g *Gateway) redactArgs(input core.Args) core.Args {
	copy := core.Args{}
	for key, value := range input {
		copy[core.Redact(key, g.secrets)] = g.redactValue(value)
	}
	return copy
}
