# feishu-bot

- Feishu + Claude Code only. Keep native streaming input, compaction and background work; no planner or business cron.
- Linux / Windows run the same native service. No container backend, image preparation or per-chat model proxy.
- Start sessions on demand, reuse hot processes, reclaim only confirmed idle processes without discarding history. Never evict background work, monitors, approvals or unconfirmed inputs.
- Use native permission modes and rules; preserve scoped maintainer approvals. Do not call same-account folders a security sandbox.
- Validate with `npm run check`. Record live Feishu/company model acceptance separately from fixtures and Windows CI.
- Preserve chat ownership on session, callback, attachment and output paths. Never blindly replay uncertain external writes.
