package gateway

import "github.com/sean2077/feishu-bot/internal/core"

// stampLocalTimes adds the human-readable form alongside the Unix-millisecond
// time. Both fields are derived from the same timestamp; callers must not let a
// model treat time as a local wall clock.
func stampLocalTimes(messages []core.Message) {
	for i := range messages {
		messages[i].LocalTime = core.LocalTime(messages[i].Time)
	}
}

func historyResult(values core.Args) core.Args {
	values["timezone"] = core.TimezoneName()
	return values
}
