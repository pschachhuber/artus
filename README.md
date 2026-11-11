# Artus

A minimal personal agent harness. One small binary gives you a local web interface, an agent that keeps its own notes, runs tasks on a schedule, drives a real browser and handles your secrets without ever seeing them.

- **One file, nothing to install.** A single executable of about 12 MB for macOS, Windows and Linux. No runtime, no database, no third-party code: Artus is written in Go using only the standard library.
- **Everything is a file.** Memory, notes, skills, tasks and conversations are plain Markdown and JSON in one folder. The agent manages them with ordinary file tools, and you can read, edit, back up or sync them with anything.
- **Small fixed context.** The system prompt and the ten built-in tools take about 3,800 tokens. Everything else is loaded when it is needed.
- **Your choice of model.** Anthropic, OpenAI and OpenRouter, with a per-chat model and reasoning effort, and an allowlist of cheaper models the agent may use for sub-agents.
- **The core is locked.** The agent can change its notes, skills, tools and tasks. It cannot change the harness, its configuration, your instructions or your secrets.

> Artus is young. It runs with your user account's privileges and has no sandbox: read [Security](#security) before giving it credentials.

## Contents

- [Quick start](#quick-start)
- [How it is organised](#how-it-is-organised)
- [Context and memory](#context-and-memory)
- [Tools](#tools)
- [The browser](#the-browser)
- [Secrets](#secrets)
- [Tasks and scheduling](#tasks-and-scheduling)
- [Models](#models)
- [The interface](#the-interface)
- [Security](#security)
- [Updates](#updates)
- [Configuration](#configuration)
- [Command line](#command-line)
- [Development](#development)

## Quick start

1. Download the file for your system from the [latest release](https://github.com/pschachhuber/artus/releases/latest):

   | System | File |
   |---|---|
   | Windows | `artus_windows_amd64.exe` (or `arm64`) |
   | macOS, Apple silicon | `artus_darwin_arm64` |
   | macOS, Intel | `artus_darwin_amd64` |
   | Linux | `artus_linux_amd64` (or `arm64`) |

2. Run it. On macOS and Linux make it executable first:

   ```bash
   chmod +x artus_darwin_arm64
   ```

   ```bash
   ./artus_darwin_arm64
   ```

   The binaries are not code-signed, so macOS and Windows ask you to confirm the first run. You can check a download against `checksums.txt` from the same release.

3. Artus prints a link and opens it in your browser. Paste an API key for Anthropic, OpenAI or OpenRouter, pick a model, and start a chat.

**Requirements.** `git` and `bash` should be on the machine: the agent's shell tool uses bash, and the change log uses git. On Windows, install [Git for Windows](https://git-scm.com/download/win), which provides both. A Chromium-family browser is used if present and downloaded automatically if not.

**From source.** With Go 1.24 or newer:

```bash
git clone https://github.com/pschachhuber/artus && cd artus && go build ./cmd/artus
```

## How it is organised

Artus keeps everything in `~/.artus` (override with `ARTUS_HOME`).

| Path | What it is | In context | Who changes it |
|---|---|---|---|
| `memory.md` | Durable facts about you and how you want the agent to work | Always | Agent, you |
| `topics/*.md` | Notes by subject: the knowledge base | One index line each; read on demand | Agent, you |
| `skills/<name>/SKILL.md` | Procedures the agent worked out, with scripts beside them ([Agent Skills](https://agentskills.io) format) | Name and description; read on demand | Agent, you |
| `tools/<name>/tool.json` | Tools the agent built: a manifest and a script | The tool's schema | Agent, you |
| `tasks/<id>/TASK.md`, `NOTES.md` | A standing job, and the agent's working state between runs | When the task runs | Agent, you |
| `chats/*.jsonl` | Every conversation, task run and sub-agent, append-only | The current one; the rest by search | Harness |
| `instructions.md` | Your standing rules for the agent | Always | You |
| `config.json` | Default model, limits, autonomy, server | No | You |
| `.env` | Secrets, as values or Proton Pass references | Names only | You (the agent can add) |
| `schedule.json` | Triggers | No | Agent, through the `schedule` tool |
| `browser/` | Browser settings, profiles, downloads | No | Agent (settings), harness |

The folder is a git repository that tracks only what the agent may change. Every change is recorded, and you can review and revert each one under **Settings → Changes**. Secrets, configuration, conversations and browser profiles are never tracked.

**What the agent can and cannot change.** It can change memory, topics, skills, its own tools, tasks and their triggers, and browser settings. It cannot change the harness itself, the base system prompt, `instructions.md`, `config.json`, or existing secrets. Its file tools refuse those paths, and if a shell command changes one anyway, Artus restores it and tells both you and the agent. Artus cannot tell your own text editor from the agent's shell, so change these files under **Settings**, or by hand while Artus is stopped.

There are no skills shipped by default. Skills are for what the agent learns about *your* procedures; everything about Artus itself that a model cannot infer is in the prompt, the tool descriptions and `artus browser help`.

## Context and memory

The design follows what current harnesses have converged on: retrieve on demand, keep the fixed part small and stable, and never rewrite history.

- **Frozen prompt.** The system prompt (under 30 lines, plus your instructions, `memory.md` and the topic and skill indexes) is rendered once per context segment and stored with the conversation. It and the tool list stay byte-identical until the next compaction, so provider prompt caches keep hitting.
- **Append-only history.** Nothing already sent is edited or removed. This is required by current Claude models, whose reasoning is signed against the history that produced it, and it is what makes caching effective everywhere.
- **Just-in-time loading.** Topics, skills and past conversations are found with `grep`, `glob` and `read` when needed. There is no vector database.
- **Bounded tool output.** A tool result over 30,000 characters is cut to its head and tail in context, and the full text is saved to a file the agent can page through.
- **Compaction.** When the context passes 70% of the model's window, or 200,000 tokens if that is lower, the agent first saves anything durable to memory or topics, then writes a structured summary. The next request starts from that summary alone. The full transcript stays on disk and remains searchable.
- **Sub-agents.** Bulky reading and searching can be handed to a sub-agent with a fresh context; only its report comes back.
- **Searchable history.** `grep` over `chats/` searches what was said and shows who said it, so past conversations work as a memory tier without an index.
- **Runaway protection.** A long run is reminded every 20 model requests that stopping with a partial result is an option, and pauses at `limits.turns`.
- **Layers.** Short-term memory is the current context and a task's `NOTES.md`. Long-term memory is `memory.md`, then topics, then the conversation archive, then skills.

A monthly **maintenance task** is installed by default. It tidies memory and topics, checks skills, tools and triggers, cleans temporary files and reports problems with the harness without being able to change it. Edit, pause or delete it like any other task.

## Tools

| Tool | What it does |
|---|---|
| `bash` | Runs a shell command. The working directory persists; long-running commands can go to the background. |
| `read` | Reads a file with line numbers, or shows an image or PDF to the model. |
| `write`, `edit` | Create a file, or replace an exact piece of text in one. |
| `glob`, `grep` | Find files by name pattern, or search contents with a regular expression. |
| `web_fetch` | Fetches a URL as readable text. |
| `agent` | Runs a sub-agent, optionally on a cheaper model from the allowlist. Several in one turn run in parallel. |
| `schedule` | Sets, lists and cancels triggers. |
| `ask_secret` | Asks you for a secret through a form; the model never sees the value. |

Web search uses the provider's own search tool where one exists (Anthropic, OpenAI, OpenRouter) and can be switched off in Settings.

The agent can add tools of its own: a folder under `tools/` with a `tool.json` manifest (name, description, JSON Schema parameters, command) and a script that receives the arguments as JSON on standard input.

## The browser

`artus browser` is a browser driver built into the binary. It speaks the Chrome DevTools Protocol directly to Chrome, Edge, Brave or Chromium, and its commands mirror Microsoft's Playwright CLI, so models already know how to use it.

```bash
artus browser open https://example.com
```

```bash
artus browser fill e5 --secret SHOP_PASSWORD
```

- **Snapshots, not screenshots.** Each command prints the page as an accessibility outline with a reference on every element (`[ref=e12]`). Large snapshots go to a file instead of into context.
- **Real input and auto-waiting.** Clicks and keystrokes are real input events. Before acting, Artus waits until the element is visible, stable and enabled, scrolls it into view and checks nothing covers it.
- **Your browser, or one it fetches.** An installed Chrome, Edge, Brave or Chromium is used if found. Otherwise Chrome for Testing is downloaded on first use. `artus browser doctor` shows what will be used and lists missing system libraries on Linux.
- **Logins persist.** Each session has a profile under `~/.artus/browser/profiles`, so sign-ins survive restarts.
- **Hand-off.** When a step needs you (a CAPTCHA, a code sent to your phone, a passkey), the agent runs `artus browser handoff "reason"`. The chat flags it, and you finish the step in the browser window or in the live view inside the interface, then click **I'm done**.

Run `artus browser help` for every command. Settings live in `~/.artus/browser/config.json`: browser choice or path, headless or visible, viewport, locale, timezone, proxy, timeouts, allowed and blocked sites.

Artus does not solve CAPTCHAs or disguise itself to get past bot protection. Those go through the hand-off.

## Secrets

Secrets live in `~/.artus/.env`, one `NAME=value` per line. The model works with names and never sees a value:

- In a shell command, mentioning `$NAME` injects that secret as an environment variable for that command only.
- In the browser, `fill <ref> --secret NAME` types it, and `--totp NAME` types the current authenticator code.
- Reading `.env` shows names with values masked, and every known value is replaced by `[secret:NAME]` in anything the model reads back.
- The agent can ask you for a secret (a form appears in the chat; what you type goes straight to the file) and can store one it generated itself with `artus secret add NAME`. It cannot read, replace or delete existing secrets.
- Provider API keys and the Proton Pass token are reserved: they are never injected into commands the agent runs.

### Proton Pass

A value can be a reference instead of the secret itself:

```
SHOP_PASSWORD=pass://Artus/Shop/password
SHOP_TOTP=pass://Artus/Shop/totp
```

Artus resolves references through [`pass-cli`](https://protonpass.github.io/pass-cli/) when they are used, with its own session, and passes the reason for each access to Proton Pass's audit log. To set it up:

1. Install `pass-cli` and create a vault for the agent.
2. Create an agent token scoped to that vault:

   ```bash
   pass-cli agent create artus --expiration 3m --vault Artus
   ```

3. Paste the token under **Settings → Secrets → Proton Pass** and enter the vault name.

The Proton Pass integration is implemented against the published `pass-cli` documentation and tested with a stand-in for the CLI, not against a live Proton account.

## Tasks and scheduling

A task is a description of a job that should run on its own. Create one under **Tasks** with a few sentences. On its first run the agent plans the work and sets its own triggers. Each later run starts in a fresh context with the task description and the notes the previous run left.

Triggers are set with the `schedule` tool or by the agent itself:

| Form | Meaning |
|---|---|
| `cron 30 7 * * 1-5` | Five-field cron in local time |
| `every 2h` | A fixed interval (`m`, `h`, `d`, `w`) |
| `in 30m` | Once, after a delay |
| `at 2026-12-01T09:00` | Once, at a time |

A trigger wakes either a task or a chat. "Remind me in two hours" wakes the same conversation.

The scheduler runs inside Artus, so triggers fire only while it is running. One that came due while it was off fires once at the next start. To have tasks run reliably, start Artus when you log in; asking Artus to set that up for your system is a reasonable first job.

## Models

Model ids are written `provider/model`: `anthropic/claude-opus-5-5`, `openai/gpt-5.6-terra`, `openrouter/google/gemini-3.5-flash`. Lists are fetched live from each provider, so new models appear without an update.

- **Effort** is one setting, `low` to `max`, mapped onto what each provider and model supports.
- **Reasoning** is shown as it streams. Anthropic and OpenAI return summaries of their reasoning, not the raw text.
- **The allowlist** names the models the agent may choose for sub-agents besides the default.
- **Refusal fallbacks.** On Claude models where a safety classifier can decline a request, Artus opts into Anthropic's server-side fallback, so a declined request is re-served by a fallback model in the same call.

Requests go straight from your machine to the provider. Apart from those, Artus contacts only GitHub, to look for updates (see [Updates](#updates)), and Google's Chrome for Testing downloads when it needs a browser and none is installed; the agent itself reaches whatever its tools reach.

## The interface

A single page served from the binary: no framework, no build step, no external requests.

- **Chats:** streaming text and reasoning, live tool cards with output and diffs, sub-agent lanes, attachments by button, paste or drop, message queueing while the agent works, fork and retry.
- **Context:** a meter in each chat, and an inspector showing what fills the context, which files are loaded, cache use, and the exact request that would be sent next.
- **Tasks, Topics, Skills:** browse, edit, run, pause.
- **Settings:** keys, default model and effort, the allowlist, limits, your instructions, secrets, and the change log with revert.

All state lives in the server. Close the tab and the run continues; reopen it and you see exactly where it is.

## Security

Artus is a program that runs shell commands and drives a browser on your behalf. Treat it accordingly.

**What Artus enforces**

- The server listens on `127.0.0.1` only, requires an access token, rejects requests whose `Host` is not this machine (DNS rebinding) and cross-origin writes (CSRF).
- The interface runs under a strict content security policy with no inline script, and files it displays are served sandboxed.
- Secret values are kept out of the model's context and the transcripts, as described above.
- Commands the agent runs get a separate token that reaches only the browser and secret-add endpoints. It cannot change settings. The interface's own token is replaced by `[secret:ARTUS_TOKEN]` wherever it turns up in what the model reads.
- Locked files are refused by the file tools and restored if changed by other means. Chat transcripts and their metadata are the harness's too: lines appended to a transcript from outside are cut off, and changed metadata is put back.
- In "confirm" mode, commands, file edits and schedule changes wait for your approval; a task you approved then runs unattended.
- The change history runs git with a fixed configuration and no hooks, so nothing planted in `.git` runs.
- A file the model is shown (an image, a PDF) is checked to be one providers accept and kept as it was seen.

**What it does not**

- **There is no sandbox.** Shell commands run as you. An agent that is tricked by a web page or a file can do what you can do, including reading `config.json` and using the interface's token without ever seeing it. The locks and guards stop mistakes and casual injection; they are not a wall. Only a separate operating-system user, a virtual machine or a container makes them absolute. For unattended tasks with real credentials, use one of those.
- **Redaction is best-effort.** It removes known values from text the model sees. It does not stop a command from sending a secret somewhere over the network.
- **Prompt injection is unsolved.** Artus tells the model to treat tool output as information, and current models mostly comply, but no model is immune. Keep the autonomy setting on "confirm commands" for sensitive work, give the agent a dedicated Proton Pass vault with only what it needs, and set spending limits on your provider keys.

## Updates

Artus looks for new releases on this repository's GitHub Releases page. Under **Settings › Updates** you choose what it does:

- **Automatic** (the default): it checks a minute after starting and then daily, and installs a new version as soon as no chat or task is running and nobody is being waited for in the browser, then restarts. An open interface reconnects by itself and says it was updated.
- **Notify:** it checks daily and shows "Update available" in the sidebar; you install with one click.
- **Off:** it never contacts GitHub on its own. **Check now** still works.

A release is installed only if it comes from this project's releases over HTTPS, matches the SHA-256 published in its `checksums.txt`, and the downloaded program starts and reports the expected version. The new program replaces the old one in place, which also works on Windows while Artus runs; the old file is removed at the next start. On macOS and Linux the restart keeps the same process.

`artus update` does the same from a terminal (through the running Artus if there is one), and `artus update --check` only looks. A build from source reports version `dev` and does not update itself. If Artus lives in a folder it may not write to, it says so and links to the release instead.

## Configuration

`config.json` is edited under **Settings**; the agent cannot change it.

| Key | Default | Meaning |
|---|---|---|
| `model`, `effort` | —, `medium` | Default model and reasoning effort |
| `allowedModels` | `[]` | Models the agent may use for sub-agents |
| `autonomy` | `auto` | `auto` runs tools freely; `confirm` asks before commands and edits in chats |
| `webSearch` | `true` | Use the provider's search tool (applies from a chat's next context segment) |
| `update` | `auto` | `auto`, `notify` or `off`; see [Updates](#updates) |
| `limits.subagents`, `limits.depth` | `8`, `1` | Sub-agents at once, and how deep they nest |
| `limits.turns` | `80` | Model requests per run before it pauses; reply "continue" to carry on |
| `limits.taskTokens` | `3000000` | Token budget of one unattended task run |
| `context.compactAt`, `context.compactMaxTokens` | `0.7`, `200000` | When to compact |
| `context.toolOutputChars` | `30000` | Tool output kept in context |
| `context.defaultWindow` | `200000` | Window assumed when a provider does not report one |
| `server.host`, `server.port` | `127.0.0.1`, `7717` | Where the interface is served |
| `secrets.backend`, `secrets.vault` | `env` | Set to `protonpass` and a vault name to use Proton Pass |

Environment variables: `ARTUS_HOME` moves the data folder; `ARTUS_PROMPT` points to a file that replaces the base system prompt; `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` and `OPENROUTER_API_KEY` are used when no key is stored.

## Command line

| Command | Does |
|---|---|
| `artus` | Starts Artus and opens the interface |
| `artus serve --port N --no-open` | The same, with options |
| `artus run "task"` | Runs one prompt and prints the answer, for scripts (`--model`, `--effort`) |
| `artus url` | Prints the link that signs a browser in |
| `artus browser <command>` | Drives the browser; `artus browser help` lists commands |
| `artus secret add NAME` | Stores a secret read from standard input |
| `artus update [--check]` | Installs the latest release, or only reports it |
| `artus version` | Prints the version |

## Development

```bash
go test ./...
```

runs everything offline: the agent loop against a scripted model, the provider clients against recorded streams, the scheduler, the server, and the browser driver against a local Chromium (set `ARTUS_TEST_CHROME` to its path; the browser tests skip if none is found).

The provider clients can also be tested against the real APIs. One OpenRouter key covers all three wire formats, because OpenRouter serves Messages- and Responses-compatible endpoints as well as its own:

```bash
ARTUS_LIVE=1 OPENROUTER_API_KEY=... go test ./internal/llm -run Live -v
```

To try the interface without a key, start with `ARTUS_MOCK=1` and choose the model `mock/echo`.

| Package | Role |
|---|---|
| `cmd/artus` | Command line and wiring |
| `internal/agent` | The loop, context assembly, compaction, tools, sub-agents, task runs |
| `internal/llm` | One message format and the Anthropic, OpenAI and OpenRouter clients |
| `internal/browser` | The browser driver |
| `internal/sched` | Triggers and cron |
| `internal/secrets` | The secret registry, Proton Pass references, redaction, TOTP |
| `internal/store` | The data folder: configuration, transcripts, indexes, change log |
| `internal/server` | HTTP API, event stream, access control |
| `internal/update` | Finding, verifying and installing releases |
| `web` | The interface |
| `defaults` | The base system prompt and the default task |

**Releases.** Pushing a tag such as `v0.1.0` runs [GoReleaser](https://goreleaser.com) through `.github/workflows/release.yml` and publishes the binaries and checksums as a GitHub Release.

## License

MIT. See [LICENSE](LICENSE).
