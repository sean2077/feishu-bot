# feishu-bot

- Keep Feishu and Claude Code as the only channel/runtime. Preserve native streaming input, compaction and background work.
- The gateway owns delivery, identity, approvals and isolation, not planning or cron.
- Validate with `npm run check`; Linux Docker has a separate smoke test. Never describe fake providers or Linux CI as live Feishu/company-gateway/Windows desktop acceptance.
- Never put service credentials in a production worker, run untrusted checkout hooks on the host, or replay uncertain external writes automatically.
- Preserve chat ownership on every session, callback, attachment and output path.
