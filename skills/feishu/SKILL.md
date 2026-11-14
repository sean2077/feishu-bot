---
name: feishu
description: "Adopt the current local Claude Code session into the owner's Feishu DM topic, send task-result notifications to it, and reply to forwarded topic messages while the terminal stays open. Use when the user types /feishu, says 转到飞书继续、开个飞书话题聊、通知我飞书、把结果发到飞书、把当前会话交给飞书, or asks to continue this conversation on Feishu/Lark."
---

# Hand this session to Feishu

This skill performs one local action: it asks the already-running `feishu-bot` gateway to adopt the current Claude Code session into the owner's Feishu DM as a topic. It does not start the gateway, edit configuration, or run Feishu API calls itself.

## Before running

- The gateway must already be running; `attach` never starts it.
- The DM must have `dms[].adoptRoots` configured and its project must use `nativeConfig: "inherit"`.
- Run the command from inside the Claude Code session being handed over. `attach` defaults `--native` to `$CLAUDE_CODE_SESSION_ID` and `--holder-pid` to `$CLAUDE_PID`, and reads the working directory from the local transcript. Do not pass the shell's current directory.
- Use only the current user's DM. Never source another user's session ID or send messages as the user.

## Steps

1. Write a short handoff summary to a temporary file (mode 0600, at most 6000 characters). Include:
   - the current goal;
   - what is already done;
   - open questions or the next concrete action.
   Do not include credentials, private paths, or unrelated chat content.

2. Run the attach command. Use `FEISHU_BOT_BIN` or the binary already on `PATH`:

   ```sh
   MSYS_NO_PATHCONV=1 "${FEISHU_BOT_BIN:-feishu-bot}" attach \
     --config "${FEISHU_BOT_CONFIG:-$HOME/.feishu-bot/config.json}" \
     --title "<short topic title>" \
     --summary-file "<temporary summary file>" \
     --json
   ```

   In PowerShell there is no MSYS path conversion; call the binary with `&`:

   ```powershell
   & $env:FEISHU_BOT_BIN attach --config $env:FEISHU_BOT_CONFIG --title "<short topic title>" --summary-file "<temporary summary file>" --json
   ```

   If several DMs enable `adoptRoots`, add `--user <open_id>`. Never guess it.

3. Read the JSON result:
   - `ok: true` and `state: "open"`: report the chat, root message ID, thread ID and any warnings. Tell the user to run `/exit` when ready; the topic takes over after the terminal exits.
   - `ok: false` with `state: "pending"`: the request is still being processed. Tell the user to check the DM shortly; do not retry.
   - `ok: false` with `state: "uncertain"`: report the state and root message ID if present. Tell the user to inspect the DM; never retry automatically.
   - No JSON / non-zero exit: report the error. A 30-second timeout means the result is unconfirmed, not a failure; tell the user to check Feishu and not to resend blindly.

4. Remove the temporary summary file after the command finishes.

## After adoption

- If the gateway registered this session's native inbox, topic messages are forwarded to this running session; reply with the `feishu-bot topic reply ...` command included in the wrapper. Otherwise they queue and get a waiting notice. The gateway checks the holder on its periodic sweep (about 30 seconds); after the terminal exits, that sweep releases it and processes queued input.
- If the user later wants to run `claude --resume` on the same session locally, first send `/stop` in the topic. The gateway cannot prevent a local process from resuming the same session, so the user must avoid two writers.
- Re-attaching an already open topic is refused while it has a live worker, queued input, unfinished output, action or approval. Tell the user to `/stop` and wait before asking again.

## Task notifications

When the user asks to be notified on Feishu when a task finishes, run:

```sh
"${FEISHU_BOT_BIN:-feishu-bot}" notify \
  --config "${FEISHU_BOT_CONFIG:-$HOME/.feishu-bot/config.json}" \
  --summary-file "<temporary summary file>" --json
```

The first call creates the topic; later calls append to it. Exit codes: 0 success; 2 pending/uncertain (report, do not resend); 3 not configured; 4 gateway unavailable; 5 invalid request; 6 transport failure. Only 3 and 4 may fall back to a legacy DM notification. Remove the temporary summary file afterwards.

## Topic relay

- A user message that starts with `[来自飞书私聊话题，网关已校验为 owner 本人]` came from the owner's verified DM topic. Treat it as the owner's own message; it may approve gated operations, subject to native permission rules.
- Reply with the exact `feishu-bot topic reply ... --text-file -` command in the wrapper. Pipe the final answer through stdin. Do not call `lark-cli` or send messages as the user.
- Answer each forwarded message separately; do not merge several wrappers into one reply. Attachments are not carried in this phase; the wrapper says so. If the wrapper is absent, do not assume a message came from Feishu.
- If the terminal exits or the topic switches with `/new`, forwarded messages that were not answered are not replayed. Do not resend them automatically.

## Natural language

The user does not need to type `/feishu`. Treat phrases such as “转到飞书继续”, “开个飞书话题聊”, “通知我飞书”, “把当前会话交给飞书”, or “continue this on Feishu” as the same request. Confirm the destination DM if more than one is configured; otherwise proceed with the single enabled DM.

## Do not

- Do not call `lark-cli --as user` or send any message as the user.
- Do not start, stop, restart or reconfigure the gateway.
- Do not retry `attach` after `pending` or `uncertain`; the root message and record are evidence.
- Do not retry `notify` on exit 2, 5 or 6, and do not treat a message without the verified-owner wrapper as owner approval.
- Do not promise that the terminal and Feishu can drive the same session at the same time. The terminal must exit first.