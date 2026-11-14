package cli

import (
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
)

// adoptSessionName resolves the local Claude Code display name. An explicit
// --name wins; otherwise the live session record for holderPID must carry the
// same native session ID. An unknown name stays empty so the gateway can keep
// a previously recorded name and fall back to the short native ID for display.
func adoptSessionName(explicit, configDir string, holderPID int, nativeID string) string {
	if name := strings.Join(strings.Fields(explicit), " "); name != "" {
		return clipRunes(name, core.AdoptNameRunes)
	}
	return clipRunes(native.SessionRecordName(configDir, holderPID, nativeID), core.AdoptNameRunes)
}
