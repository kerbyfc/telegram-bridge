# telegram-bridge

Sessions of Claude Code and OpenCode talk to you through a Telegram forum group with topics: every
session gets its own topic. The agent's finished replies go to the topic, and your words written in
the topic arrive in the session as a new turn, even when you are away from the computer.

There is one binary, `telegram-bridge` (module `github.com/kerbyfc/telegram-bridge`,
`cmd/telegram-bridge`): it is the service, the holder of a session's connection, the thin client and
the installer. The protocol and the boundaries between the parts are in [`contract.md`](contract.md).
The contract is also the way to connect another harness: a client that implements the session side of
it gets the same topics, delivery marks and commands without touching the service.

| Part | What it does |
|---|---|
| service `telegram-bridge serve` | one process per machine under launchd (on Windows, the Task Scheduler): polls the Bot API around the clock, keeps topics, sessions and the owner's inbox, listens on a local socket `<state_root>/bridge.sock` (a named pipe on Windows) |
| Claude Code mod — [kerbyfc/claude-telegram-bridge](https://github.com/kerbyfc/claude-telegram-bridge) | the client plugin inside the session process: turn boundaries, finished replies and the context percentage go to the service; your word is submitted as a prompt; a command from the topic runs as a client command. `telegram-bridge link` holds the connection for it |
| OpenCode plugin — [kerbyfc/opencode-telegram-bridge](https://github.com/kerbyfc/opencode-telegram-bridge) | the same role in the OpenCode server; its own parts are the `/away` and `/here` commands, the `question` tool hook and the `TG: <topic name>` status line at the bottom of the window |
| `telegram-bridge doctor` | a check of the whole chain in one command |

Without the mod (for OpenCode, the plugin) there is no channel: your words and the agent's replies
travel only through its connection.

## Away mode

| Where | Turn on | Turn off |
|---|---|---|
| Claude Code session | `/away [topic name]` | `/here` |
| OpenCode session | `/away [topic name]` | `/here` |
| the session's topic in Telegram | `/away [topic name]` | `/here` |

- **The mode lives in the service**, on the session record: a short call does not turn it off. A
  session that ended holds no mode, and gets it back when it returns; only your word takes it away for
  good. The service reads the topic all the time, so `/away` from the
  topic works even when no session is in the mode; it applies to one session — the session of that
  topic.
- **Topic.** The name is the argument, otherwise the chat's own name as the session adapter reports
  it, otherwise the topic already bound to the session, otherwise `default_topic_name` from the
  settings. The chat's name is followed: a renamed chat renames its bound topic — while the mode is on
  and after `/here` too — by the same rules as any other name. A name you type, in the terminal or in
  the topic, is yours: chat renames never change it. A chat without a name falls back to the bound
  topic and `default_topic_name`. Longer than 128 characters is cut with an ellipsis, the suffix stays
  at the end. A topic and a session are one to one: a name taken by a live session gives the second
  session the lowest free suffix `#2`, `#3` — two chats with the same name get `X` and `X#2`. A deleted
  topic is created again on the first "topic not found" error.
- **Topic icon.** There are two: `topic_icon_on` while the mode is on, `topic_icon_off` after `/here`,
  after the session ends and on a new topic without the mode. The service never removes an icon.
- **A session that ended** has its topic switched to the off icon and closed. The topic keeps its name,
  and the name is free again: a new session with that name gets it without a suffix, in a new topic,
  and two closed topics may carry the same name. If the session returns (`claude --resume`), the topic
  is reopened and gets the icon of its mode; it is renamed only if the name is taken, to the lowest free
  suffix. A word written to the closed topic is held, with the same 😴 and the same reply
  as for any session outside the mode. A refusal from Telegram goes to the log and changes nothing else.
- **Only finished replies reach the topic**, each as one message; a long one is split at Telegram's
  limit, in order. Tool output, tool input and thinking never reach the topic; an interrupted reply
  is not sent. While a turn runs, the topic shows "typing".
- **Your word** is a message in the topic without a slash. The service puts it into the session's
  inbox; the session adapter submits it as a turn marked as coming from the user. A running turn is
  not steered; a standing session is woken with a new turn. The fate of the word is marked on your
  message: 👀 the bridge took it, 🫡 the agent took it, 😴 held. An inbox entry is removed only after
  a confirmed submission, and it survives a disconnect and a service restart.
- **Commands** `/away` and `/here` are executed by the service; anything else with a slash is a client
  command, and the session adapter runs it as if typed in the terminal. There is no allow-list and no
  completion.
- **Input is your text only.** A message not from `owner_id`, from a bot, without an author, a sticker,
  a photo, a service message never reach a session and are not named in the topic; the trace is a log
  line. Reactions are not read. A reply reaches the session as an ordinary message, and its delivery
  line names the original message in full, along with the quoted fragment, if any.
- **No turn is held waiting.** A turn with a question ends, the session stands, and your answer wakes
  it. In away mode a question with options arrives in the topic as a message with numbered options:
  reply with an ordinary message, and the answer comes back to the session. A question without
  options is an ordinary reply, and you answer it the same way.
- **Topic header.** The pinned line `<x%>` carries the context percentage from the mod
  (OpenCode has no ready number, so its line carries the name alone); the 🤖 mark means the session is
  in away mode.
- **The status line** of Claude Code shows the topic name while the mode is on — see "Contribution to
  the status line" below.

## Questions with options

The `PreToolUse` hook on `AskUserQuestion` calls `telegram-bridge hook ask`. Add the hook to your
Claude Code settings:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "AskUserQuestion",
        "hooks": [{ "type": "command", "command": "telegram-bridge hook ask", "timeout": 120 }]
      }
    ]
  }
}
```

- The client's payload arrives on standard input, the decision goes to standard output: every
  question is sent to the topic (the service numbers the options), and the tool call ends with the
  refusal "the question was sent to the owner in the topic; the answer will come as a new message".
  The answer comes back as an ordinary word from you.
- **The mode lives in the service, so its absence means "the owner is at the computer".** No service,
  no settings, an unreadable payload — the question stays in the terminal, the tool call is not taken
  away, and a line in the error stream names why it did not go to the topic. Exit code `2` is kept for
  a real channel failure: the service refused or is silent on a live connection — then the call is
  blocked and the model gets the reason.
- Invalid questions with a readable session are checked against the mode of the service: the mode is
  on — a refusal with code `2`, because a dialog in the terminal has nobody to see it; the mode is off
  or unknown — the question stays in the terminal.
- The subcommand takes the service settings from `DM_TELEGRAM_CONFIG` — the same variable the mod
  lives by. Without it the shell of the session has no settings: the question stays in the terminal,
  and `/away` and `/here` answer "no settings path named".
- `hook ask` holds no turn: no input waits for an answer. The subcommand's budget for the whole
  payload (`hookBudget` in `internal/cli/hook.go`) is shorter than the hook timeout, so the last
  refusal goes out as a line and a code, not as a client disconnect.
- There is no `Stop` hook: the turn is published to the topic by the session adapter, and the contract
  knows no second caller for a turn.
- For OpenCode the same `hook ask` call is made by the plugin: a `tool.execute.before` handler on the
  `question` tool moves its input into the Claude Code hook payload.

## Installation

The bot needs a group with topics: create a bot with [@BotFather](https://t.me/BotFather)
(`/newbot`), turn on Topics in the group, and add the bot as an administrator with the "Manage topics"
and "Pin messages" rights. Turning topics on makes the group a supergroup and changes its id: you
need the new one, `-100…`.

Settings are your own `config.yaml`; its path is named by `--config` or `DM_TELEGRAM_CONFIG`. Sessions
need `telegram-bridge` on `PATH` (or its path in `DM_TELEGRAM_BRIDGE`) and `DM_TELEGRAM_CONFIG` in the
environment: the mod, the plugin, the commands and the hook live by them. The mod is connected to
Claude Code as the `telegram-bridge` plugin from the `claude-telegram-bridge` marketplace: declare it
in `extraKnownMarketplaces` and `enabledPlugins` in the project's `.claude/settings.json`, and the
project session installs it itself as soon as the folder is trusted. An unreleased mod change is
tried with `claude --plugin-dir <mod clone>`, which overrides an installed plugin of the same name.

```yaml
bot_token: "123456:ABC..."
group_id: -100...
owner_id: 111111
state_root: /Users/<name>/.dm-telegram-go
default_topic_name: work
statusline_root: ~/.claude               # status line state root; no key — ~/.claude too
topic_icon_on: 🤖                        # topic icon while the mode is on
topic_icon_off: 💻                       # topic icon otherwise
proxy: ""                                # an explicit proxy if one is needed; empty — direct
reconnect_grace: 1h                      # how long a session is kept after its connection ends
poll_timeout: 50s                        # long polling of getUpdates
context_edit_min_interval: 3s            # how often the pinned line is edited
log_max_bytes: 2097152                   # log rotation size
log_keep: 5                              # kept log generations
retry_pauses: ["5s", "15s", "1m", "5m"]  # pauses between polls after a failure
```

`topic_icon_on` and `topic_icon_off` are optional, with the defaults shown, and must come from Telegram's
closed set, `getForumTopicIconStickers`; a key with no value is refused. An icon outside the set is never
silently replaced: `doctor` and `install` refuse and name the key and the value, and the service does
not start, as with any invalid setting. The set can only be read over the network, so when Telegram
cannot be reached the value counts as not confirmed rather than invalid: `doctor` says so, `install`
reports it and goes on, the service starts and writes it to the log. A changed icon takes effect after
the service restarts, on each topic's next switch; icons already in place stay.

### Installing and removing

```bash
go install github.com/kerbyfc/telegram-bridge/cmd/telegram-bridge@latest

"$(go env GOPATH)/bin/telegram-bridge" install --config ~/.dm-telegram-go/config.yaml      # binary, autostart, start
telegram-bridge uninstall --config ~/.dm-telegram-go/config.yaml    # autostart and binary
```

Run `install` from the freshly built binary: `telegram-bridge` on `PATH` may be the earlier copy in
`~/.local/bin`, which compares itself with itself, reports `same` and leaves the service running the
old build.

`install` copies the binary to `~/.local/bin/telegram-bridge` and writes the autostart entry
`~/Library/LaunchAgents/com.kerbyfc.telegram-bridge.plist`: launchd keeps it starting at login and
restarting on failure (on Windows it is a scheduled task, see "Windows" below). A copy, not a link to
the build tree: the service must come up even when the directory it was built in is gone.

The label changed: a service installed under the old label is removed by that build's own `uninstall`
before this one is installed — the new `uninstall` does not know the old entry.

The steps are idempotent: what is already done is confirmed by a line of the report, and a live
service is not torn down without a change — `kickstart` without `-k` does not touch a running
process. When the entry or the binary changed, the service is restarted, otherwise the new code
would not be picked up.

The installer does not invent a settings file and does not compose secrets: without a valid
`config.yaml` there is no installation at all. It does write one key, `statusline_root`: under
autostart the service would not learn the status line state root otherwise, and a value you named
yourself is left alone (details in "Contribution to the status line"). Removal takes away only the
autostart entry and the binary — state, settings and the log stay, they are your data.

**Proxy.** Telegram needs no proxy: the client goes to `api.telegram.org` directly and does not read
the `HTTPS_PROXY`, `HTTP_PROXY` and lowercase environment variables at all. The `proxy` key is for
someone who still needs a proxy: the value named there is what the request gets.

**Windows** — a system service and a named pipe instead of a socket. Frames, `rid` and connection
displacement are the same: the transport is decided by `internal/endpoint`, the protocol is one.

- **Address.** The same `socket_path` key carries a pipe name here: `dm-bridge` is a short name,
  `\\.\pipe\dm-bridge` the full one, and a path (`bridge.sock`, `C:\…\bridge.sock`) is a refusal with
  a reason: a pipe has a name, and settings moved from macOS without a fix say so themselves. With no
  key, the name is derived from `state_root`: `\\.\pipe\dm-telegram-bridge-<16 sha256 chars of the
  root>`. A branch service has its own root, hence its own pipe: two services on one machine do not
  displace each other.
- **Pipe rights.** The pipe comes up with an explicit security descriptor
  `D:P(A;;GA;;;<owner SID>)`: one access entry for the current user's SID, profile inheritance
  removed. Machine administrators and SYSTEM cannot open the pipe. The platform defaults do not give
  this, so the descriptor is set, not inherited.
- **Service.** `install` creates the scheduled task `telegram-bridge` (`telegram-bridge-<name>` for a
  branch service) with an at-logon trigger, a hidden window and restart on failure
  (`RestartOnFailure`, one minute, 999 times). It runs `telegram-bridge serve --service --config
  <path>`: `--service` first redirects the service output to `<state_root>/service.log` and only then
  removes the console window, so a settings failure on the first start is visible in the file that
  `doctor` names instead of disappearing with the console. The Task Scheduler does not capture foreign
  output and the window is created by the process start, so a later removal does not keep it on
  screen longer. Removal is the same `uninstall`.
- **Idempotency.** The task entry is compared by the fingerprint in its description; the output of
  `schtasks /Query /XML` is parsed in both possible encodings (UTF-16 with a BOM and the single-byte
  console encoding), and output that does not start with an XML declaration is a refusal with a
  reason: silently rewriting the task would tear a live channel on every install. The presence of the
  task is asked twice: **by name** (`/Query /TN`, independent of whether a hidden task is listed) and
  **by the general list** (answers "can it be asked at all"); a refusal of both is a refusal with a
  reason, a missing name in a successful list is "there is no task".
- **Restart** is confirmed by the service itself: after `/Run` the installer asks it
  (`OpServiceStatus`) and requires that the service answer and that its start time be no earlier than
  the current binary. The protocol gives the start time in seconds, the file time in nanoseconds, so
  the file time is truncated to a second when comparing: a service started in the same second the
  binary was written is fresh (otherwise a regular first `install` would fail about a successful
  restart). Otherwise the installation refuses — "the service did not answer" or "the restart did not
  pick up the new build" — instead of saying "the service is running".
- **The local entry of the `link` subcommand** on Windows is a pipe too
  (`\\.\pipe\dm-telegram-link-<16 random chars>`), with its own name per run and the same rights.
- **The state directory check** on Windows asks the system for its DACL. It is closed when only the
  process owner, SYSTEM and machine administrators have access entries (on Windows that is the same
  as root on macOS: a 0700 permission does not close anything for them) and when CREATOR OWNER and
  OWNER RIGHTS, meaning the owner themselves, are there. An entry that opens the directory to someone
  else, or no DACL at all, fails `doctor` just as permissions wider than 0700 do on macOS, and names
  who the directory is open to. The restriction is not reduced, and the check does not verify whether
  it overrides someone's permission. An entry of a kind it does not parse is "not confirmed" with the
  kind named and `icacls`.

What cannot be checked here goes to a live run on Windows: pipe rights, the refusal of a second
listener on a busy pipe (the platform reports `ERROR_ALREADY_EXISTS`, not what was checked on a
socket), the encoding of `schtasks /Query /XML` output, the restart confirmation by a live service, the
service after a reboot, the absence of console windows, state directory rights (ACL), the answer of
`schtasks /Query /TN` about the task created by `install`, and the submission of a word to an idle
session. On macOS the cross-build and `go vet` under `GOOS=windows`, the transport tests and the DACL
check tests are run; the tests of the pipe itself and of reading the DACL from the system are marked
`//go:build windows` and do not run here.

## Moving from the old Python bridge

A one-time job on a machine where the Python bridge lived: the topic registry and the poll offset move
from its state directory (not in git) into the service state, after
which exactly one bridge polls the bot. The Python bridge code is gone from the repository; only
`import-topics` reads its directory.

Polling updates is one per token: a second `getUpdates` with the same token is refused by the Bot API
(`409 Conflict`), or an update goes to one then the other, and some of your words are lost for good.
So the old bridge's polling must stop before the service's first poll.

1. **Service settings.** `config.yaml` as in "Installation": its own `state_root` (not the old
   bridge's state directory — the import refuses a source lying in the service state). The same group as the old bridge: topic ids belong to it, and in another group they mean other
   topics (the import has nothing to check this with — the registry does not carry the group id).
2. **Stop the old poller** before step 3:

   ```bash
   pkill -f bridge.py
   pgrep -fl bridge.py   # empty — no process of the old bridge
   ```
3. **Move the registry and the poll offset** — while there is no service yet:

   ```bash
   telegram-bridge import-topics --config ~/.dm-telegram-go/config.yaml --from <old state dir>
   telegram-bridge import-topics --config ~/.dm-telegram-go/config.yaml --from <old state dir> --check
   ```

   `--from` names the old bridge's state directory and is required: the command has no default for it,
   and a call without the flag is refused with the flag named. The
   command only reads it: it changes neither a file nor a directory. In one run it says how many
   topics were read, how many were placed and which were skipped with a reason; `--check` is the same
   without writing, used to verify an import already done. A repeated run changes nothing, and there
   is nothing to write — the registry file is not rewritten at all. With a running service the command
   refuses: the service would overwrite what was written with its in-memory state. A silent service is
   a refusal too: it did not answer in time, so it is alive and busy, not stopped. A source lying in
   the service state directory is a refusal before reading.

   The poll offset (the `offset` file) moves with the registry, and the service starts its first poll
   from it. The old bridge wrote the offset right after parsing an update, and Telegram confirms an
   update only with the next poll: without the move the service would get the last parsed update a
   second time. No offset — nothing to move, not a refusal; a broken one (not an integer, or not
   greater than zero) is a refusal before writing, and neither the registry nor the offset is moved.
   The offset never goes back: a service that has already polled further keeps its own.

4. **Install the service** — `telegram-bridge install --config …`: it starts and begins to poll.
5. **Check.** `telegram-bridge doctor --config …` — all checks; `/away` in a session — the same topic,
   and the conversation in it is the old one.

**What moves and what does not.**

| What | How |
|---|---|
| topic registry: topic name → its id | moved by `import-topics`; this is the history of the conversation per session: the topic stays the same, and the conversation lives in it |
| poll offset | moved by `import-topics` into `updates.json`: the service starts its first poll from it, and what the old bridge parsed but Telegram did not confirm does not come a second time. The offset never goes back |
| a topic that no longer exists in Telegram | moved as is: the platform gives no topic list, there is nothing to check. A missing topic is created again on the first reference and the registry is fixed |
| session bindings: which session has which topic | not moved: the first connection of a session restores them — `/away` binds it to a topic by name |
| open questions, answer authors, closing journal | not moved: the service has no turn waiting at all — a turn ends, and there is nothing to hold a question with |
| topic mark and pinned line | not moved: the old bridge did not keep them in the topic registry. A moved topic's mark is recorded as "unknown" — the import does not know what it is — and the first mode switch goes to the platform and evens it out; the pinned line is set on the first mode switch |

## A branch service next to the main one

A change is checked by its own service, not on your live channel:

```bash
telegram-bridge install --instance 06-branch --config /path/to/branch/config.yaml
```

Everything is its own: the autostart label (`com.kerbyfc.telegram-bridge.06-branch`), the binary
name (`telegram-bridge-06-branch`) and the settings file — hence its own state, socket and bot. The
name is lowercase letters, digits and a hyphen, up to 32 characters: it goes into the autostart label
and the file name.

## Self-check

```bash
telegram-bridge doctor --config ~/.dm-telegram-go/config.yaml
telegram-bridge doctor --config ~/.dm-telegram-go/config.yaml --json
telegram-bridge doctor --config ~/.dm-telegram-go/config.yaml --session <id>   # plus an id check
```

There are eight checks, each named by a line: settings (the token is parsed into a bot id, the group is
a supergroup, the owner is named), topic icons (both are in Telegram's set), the state directory, the log with its generations and the process
output, the autostart entry, the liveness of the service itself, the bot and the group, and session
connections. With `link_resolvers` named, one more check names the path the running service
found for the program of each entry. The token, the group and the channel to Telegram are confirmed by the outcome of the last
poll — the only call the self-check makes itself is the one for the set of icons. No service — the check fails, and the last
lines of **both** files are appended to the refusal: its own log and the process output where a panic
goes (the autostart system writes it, and the log does not have it at all). The cause of a crash must be
visible with one command, not only as silence in the topic.

The autostart check also looks at the binary the entry starts. A binary removed by hand past
`uninstall` is survived by the service — the process is already in memory — while callers do not find
it, and a hook question goes to the terminal even with the mode on. The self-check says so directly:
"the service is running but its binary is gone" (without the service — "the autostart entry exists but
the binary is gone"). Installation fixes it: a repeated `install` puts the binary back and restarts the
service; `uninstall` stops it and removes the entry. The self-check itself fixes nothing.

There are three outcomes of a check: `+` confirmed, `?` not confirmed, `-` refusal. The exit code is
non-zero on a refusal, but not on "not confirmed": the first long poll returns after `poll_timeout`, and
`doctor` called right after installation names the bot and the group as not confirmed rather than
refused. A self-check nobody trusts checks nothing.

With `--session` a ninth check is added — **an id check**. `/clear` ends the conversation but not the
process, and the mod deliberately does not say `bye` to the service: if it did, the channel would die
before the client. So the service may keep knowing the session under the old id, while the status line
and the short call count the id from the client. The check names the client's id, the ids the service
knows, and whether a contribution lies under the named name. Which session is "the same" it does not
guess: no side of the protocol has a link between two ids, and guessing would be a lie about the state
of the channel.

## The session's thin client

A short call instead of starting the bridge: the process lives for one request and does not hold a
session turn.

```bash
telegram-bridge away on [topic name] --session <id> --json
telegram-bridge away off --session <id> --json
telegram-bridge away status --session <id> --json
telegram-bridge say "text" [--session <id>] [--kind message|notice] --json
telegram-bridge ask "question?" --option "a=do it" --option "b=later" --session <id> --json
```

`--session` is the same id the client puts into its input. It is not derived from the environment in
any way: the transcript file name and the environment variable do not always match it, and the
status line contribution file is named by exactly this id.

## Session adapters: the Claude Code mod and the OpenCode plugin

The session connection is held by the subcommand `telegram-bridge link --session <id> --client
<client>`, and it is one for both clients: protocol frames, the `after` cursor, reconnection,
displacement and refusals after which there is no channel all live in it. The client names itself, and
the subcommand translates the name into the kind of caller — `claude-code` holds the session with a
mod, `opencode` with an adapter.

**A restart of the service does not end the subcommand.** Stopping the service (an update,
`launchctl kickstart -k`, a reboot) cuts every session connection without answering the long
`delivery.wait`, so the subcommand sees a lost connection, not a refusal. It stays in the same
process, dials again every `--retry` (1 s by default) for as long as the service is gone, and greets
the restarted service with the same session: the session gets its topic and away mode back, within
`reconnect_grace` without being buried at all. A lost connection is the `lost` event, once per
absence; an absence longer than `--absent` (30 s by default) is the `absent` event, also once, and the
session adapter tells you that the channel is waiting for the service. The subcommand ends only on
`bye`, on its stop, or on a refusal that a new connection cannot cure (another major version of the
contract, a displaced session connection) — then the `error` event says why. A subcommand that ended
otherwise (killed, crashed) is started again by the session adapter.

| Session adapter | Where | How it submits your word | How it runs a command |
|---|---|---|---|
| Claude Code mod | [kerbyfc/claude-telegram-bridge](https://github.com/kerbyfc/claude-telegram-bridge) | a prompt submission marked as from the user | a client command call |
| OpenCode plugin | [kerbyfc/opencode-telegram-bridge](https://github.com/kerbyfc/opencode-telegram-bridge) | `session.prompt` with `queue` delivery | `session.command` with `queue` delivery |

The topic does not show which client is behind a session: delivery marks, the reply stream, topic
behaviour and command execution are the service's business. OpenCode has one thing of its own, and it
is your requirement — the line at the bottom of the window, `TG: <topic name>`; its state comes from
the `away status` call, not from reading the bridge's state.

**Turn boundaries, replies, the context percentage and the chat's name are named by the session
adapter.** Both clients call the same local entry of the subcommand: `POST /turn/start {turn?}` and
`POST /turn/end {turn?}` — turn boundaries with a turn name, by which the service keeps the "typing"
chat action in the topic, `POST /message {text}` — a finished agent reply that the service sends to the
topic as one message, `POST /context {percent}` — the share of the context window in use, which the
service puts into the topic's pinned line, and `POST /name {name}` — the chat's name, which becomes the
topic's name when you did not give one yourself. Neither the subcommand nor the service keeps its own
context count: the number is taken from the client by whoever lives inside the client. The Claude Code
mod takes it ready-made and names it on every turn boundary; the OpenCode plugin has no ready number
yet, so no percentage comes from there and such a session's pinned line carries the topic name alone.

## The reply stream: only a finished reply reaches the topic

- A finished agent reply goes to the topic whole and as one message;
- while the reply is being written, the topic shows only "typing", held by the turn boundaries;
- pieces are not sent live and nothing is edited in the topic: the stream has no assembling with
  edits of what was already sent;
- tool output never reaches the topic, nor does its input or thinking;
- a reply longer than Telegram's message limit is split at the limit, in order, by the same cutter the
  service uses for any long text; it cannot be held back or folded;
- a reply interrupted in the middle is not finished and does not reach the topic with either client:
  press Escape in the middle of a reply and nothing appears in the topic.

The client itself says what is finished, not text parsing. The Claude Code mod takes the outcome of a
turn step (`turn.step` → `answer`: the visible reply text the client assembled when all reply blocks
arrived) and sends it only for a step with a named end of the reply (`stopReason` is not `null`). The
OpenCode plugin takes the server event `session.text.ended`, which comes for a finished text block and
carries it whole; thinking goes as `session.reasoning.*`, tool calls and their input as
`session.tool.*`, and by the kind of event they never reach the topic. Sending a reply does not hold
the turn: the notice goes without waiting, like the turn boundaries.

## Link resolvers: the text behind a link in the reply

A reply may carry a link whose content the owner wants to read right in the topic: an issue, a
note, a ticket. The `link_resolvers` key of `config.yaml` names where such links live in a reply and
which command turns a link into text. The service knows nothing about the links themselves: one
rule, everything specific is in the entry.

```yaml
link_resolvers:
  - where: '(?m)^see also:(.*)$'      # place in the reply; a group narrows the search to it
    link: 'https://tracker\.example\.com/[^\s)]+'
    command: ["/usr/local/bin/tracker", "show", "--plain", "--", "{link}"]
    timeout: 10s
```

On Windows the program is a path to the `.exe`; in single quotes YAML takes backslashes as they are,
in double quotes each one is doubled:

```yaml
link_resolvers:
  - where: '\[\[(.+?)\]\]'
    link: 'https://[^\]]+'
    command: ['C:\Users\me\bin\tracker.exe', "show", "--plain", "--", "{link}"]
    # the same in double quotes: ["C:\\Users\\me\\bin\\tracker.exe", "show", "--plain", "--", "{link}"]
    timeout: 10s
```

A path with spaces needs nothing extra: the program is one array item, no shell splits it. The
output must be UTF-8 on Windows too: a command printing in the console code page is refused with
"вывод не в UTF-8", not shown garbled.

- `where` is a Go regular expression (RE2) over the whole finished reply. With a group the link is
  searched only inside it, without one inside the whole match; more than one group is an error.
  Modes are flags of the expression itself: `(?m)` for a line, `(?s)` for text spanning lines.
- `link` is a Go regular expression for a link inside each place; an empty match is not a link.
- `command` is a program and its arguments, run without a shell; `{link}` inside an argument is
  replaced by the link, which stays inside that one argument. A leading `~` in the program is
  expanded.
- The program is a bare name or a full path. A bare name is looked up in the service's own `PATH`
  when the service starts; not found — the service does not start, and the reason with the program
  and that `PATH` goes to the process output. Under launchd, systemd or the Windows task scheduler
  the service's `PATH` is usually much shorter than your terminal's, so for a program in
  `~/.local/bin` or another personal directory a full path is the reliable choice.
- The link is written by the agent. A link starting with `-` would become a flag of the command:
  put `--` before `{link}` if the command understands it, and anchor `link` to the start of a real
  link (`https://…`, `obsidian://…`) rather than matching any word.
- Windows batch files (`.cmd`, `.bat`, by the named program and by what a `PATH` search finds, in
  any case) are refused on every platform. `cmd.exe` runs them and parses the arguments again: the
  `&` of an ordinary link breaks the command, and a link like `x&calc` starts a foreign program. Use
  an executable wrapper instead.
- `timeout` is required; on expiry the command is killed with all its children: a process group on
  macOS, a job object on Windows (the command starts suspended and enters the job before its first
  instruction). A child that left for its own session (`setsid`) or process group on macOS is out of
  reach.

The command contract: the link comes as an argument, the ready text goes to stdout in UTF-8, a
failure is a non-zero exit code. `\r\n` becomes `\n`. In the topic the reply goes as it is, then the
output of each link as a separate message in the order of the links in the text, cut at Telegram's
limit. The answer to the reply does not wait for the commands; the next reply, `say` or question of
the same session waits for the previous reply's output so it does not get in between. A failed
command, an expired timeout, empty output, output that is not UTF-8, output larger than 1 MiB (the
command is stopped as soon as it grows past the limit) or a child holding the output open after the
command exited gives a line `ссылка не развёрнута: <link>: <reason>` after the reply and a
`link-unresolved` log line. A reply that did not reach the topic stops its commands; nothing follows
it. On service stop the commands are killed, nothing goes to the topic, and each dropped link leaves a
`link-dropped` log line. A link outside a place is not resolved. No key — nothing changes.

A broken entry (an invalid expression, more than one group in `where`, an empty command, `{link}`
in the program or in no argument at all, a missing or invalid `timeout`, a batch file as the program)
is an error loading the settings: the service does not start. `doctor` adds a "ссылки" check that
says what the service sees, not the terminal: the running service names the path it found for each
program (`service.status`). Without a running service the check is "not confirmed" — the terminal's
`PATH` is not the service's. Entries in the file differing from the service's ones also give "not
confirmed": restart the service.

## Log

`<state_root>/bridge.log` — appending at the end by one writer; the generation changes by size
(`log_max_bytes`, 2 MiB by default), `log_keep` previous ones are kept (5 by default).
`<state_root>/launchd.log` — the output of the service process itself: panics and refusals before the
log is opened, written by launchd. Nobody rotates it — the writer is foreign — so a panic in the
`KeepAlive` loop grows it without limit; the self-check names its size, and that is the only way to see
that it grows.

## Contribution to the status line

While a session has away mode on, the service writes two files by the status line contract of the
program that reads them (the section "Telegram bridge contribution"): the contribution
`<statusline_root>/statusline/segments/<session_id>/telegram-bridge.json` with the topic name in the
session cube's header, and the result of the probe
`<statusline_root>/statusline/probes/<session_id>/away` with the word `away`, by which the threshold
rule colours that header. The mode is off — `items` is empty and the probe file is removed. No home
directory in the service's environment — no root, and nothing is written: that is not a refusal, the
status line is a foreign reader, and its absence is not an error.

The contribution's lifetime is one day, and the service's own tick holds it: while the mode is on, it
rewrites both files once an hour. Without the tick the same lifetime would lie — a mode on for longer
than a day would leave the screen by itself.

**The contribution root is the `statusline_root` key, and the installation writes it itself.**
`CLAUDE_CONFIG_DIR` does not reach an autostart service — launchd does not inherit the shell
environment — so the installer reads the variable: the root it sees in its environment is named as the
key, and without the variable it writes `~/.claude`; a value you named yourself is not overwritten. The
service does not read the variable at all.

| What | How |
|---|---|
| where the service takes the root | the `statusline_root` key; no key — `~/.claude` |
| who names it | the installer: `CLAUDE_CONFIG_DIR` of its environment, otherwise `~/.claude`; it does not touch what you named, and an invalid path is a refusal before the file is changed |
| the key is there, the value is empty | settings load error: name a path or remove the key — without it the root is `~/.claude` |
| no home directory | nothing is written |
| tilde | a leading `~`, `~/…` and `~\…` (Windows) is expanded on load in every path key (`state_root`, `statusline_root`, `socket_path`, `log_path`); `~user` is refused |
| Windows | the same: the installer writes `~\.claude`, the service expands it on load |

## Tests

```bash
go build ./... && go vet ./...
GOOS=windows GOARCH=amd64 go build ./... && GOOS=windows GOARCH=amd64 go vet ./...   # pipe transport
go test -race ./... -count=1
go mod tidy -diff
```

Build and `go test ./...` run on the native platform; `go vet` and a Windows build run for both
platforms on any machine. Race detection needs cgo with a C compiler. No test touches your system:
installation is checked on its own home, its own autostart directory and a fake OS command runner, and
the status line contribution on its own root. The pipe tests are marked `//go:build windows` and run
only on Windows: their run is a separate line of a live check.

## Connecting another harness

The service knows nothing about a particular client. A new harness is connected by implementing the
session side of [`contract.md`](contract.md):

- hold a long connection with `telegram-bridge link --session <id> --client <name>` and read its JSON
  lines;
- report turn boundaries, finished replies and the context percentage through the local entry
  (`/turn/start`, `/turn/end`, `/message`, `/context`);
- submit your word as a user prompt and confirm the delivery (`/ack`), and run client commands from
  the topic;
- call the thin client for away mode and questions (`away on|off|status`, `say`, `ask`, `hook ask`).

Everything else — topics, the inbox, delivery marks, the pinned line and the owner filter — is the
service's, and the harness gets it without writing any of it.
