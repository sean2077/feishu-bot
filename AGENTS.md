# feishu-bot

- Feishu + Claude Code. Preserve native streaming input, compaction and background work; do not add a second planner or business scheduler.
- Linux and Windows use the same native service. Start sessions on demand and reuse warm processes; reclaim only confirmed idle processes without discarding history.
- Group triggers require an allowed chat, allowed author and explicit bot mention. DMs require an allowed user. Reject silently before context, downloads or reactions.
- Background pre-injection defaults to zero. History, quotes, pagination and attachments must enforce current chat and author scope; never turn history access into a cross-chat tool.
- Preserve the native session across process reclamation, restart, stop and compact. Only explicit new-session operations rotate it. Never silently replace a failed resume.
- Compact is a native command at a turn boundary, not a summarization prompt. No gateway automatic compaction. Preserve approvals while a control command is waiting.
- Keep reasoning and answers in one reply card, omit tool traces, bind input origins, and mark Done only after successful final delivery. Signatures and redacted thinking are not display content.
- Preserve ownership on sessions, callbacks, attachments and outputs. Do not replay uncertain external writes or evict active background work and approvals.
- Use native permission rules and scoped maintainer approvals. Same-account directories are not a security sandbox.
- Keep documentation, examples, prompts, PR descriptions and comments independent of any individual's environment or private discussions. Use fictional identifiers and reserved example domains; never copy deployment credentials, internal endpoints or personal paths.
- Document existing capabilities and limitations. Support different usage scenarios through configuration, not a general-purpose platform framework.
- Run relevant tests and `npm run check` when available. Separate simulated-provider evidence, platform CI and live-service acceptance; report only what actually ran.
