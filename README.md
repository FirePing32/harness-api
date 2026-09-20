# harness-api

An OpenAI-compatible HTTP server that runs an agentic coding loop against any
OpenAI-compatible model. Point an existing client at it, and the model gets a
workspace, file tools, and a shell.

> **Status: usable, incomplete.** Phases 0–8 of 11 are done. The agent loop
> works end to end with the full core tool set — `read`, `glob`, `grep`,
> `edit`, `write`, `bash` — plus streaming, persistent sessions, tool guards
> and provider quirk profiles. Not yet implemented: context compaction, so a
> long task can still overflow the model's window. See [Roadmap](#roadmap).

## Why

Agentic capability is dominated by the model. A harness cannot make a weak model
strong — but it controls how much capability gets left on the table, and that
gap is large. The same model can swing dozens of points on agentic benchmarks
between a good harness and a poor one.

The design follows [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)
(MIT), read at source rather than from secondary coverage. No DSH code is
included here; the behaviours were reimplemented from its documented design.

## Requirements

Go 1.26 or later. `os.Root`, used for the path jail, needs 1.24+.

## Running

```sh
go build -o harness-api ./cmd/harness-api

export HARNESS_UPSTREAM_BASE_URL=https://api.openai.com/v1
export HARNESS_UPSTREAM_API_KEY=sk-...      # or OPENAI_API_KEY
export HARNESS_UPSTREAM_MODEL=gpt-4.1

./harness-api
```

Then point any OpenAI client at `http://127.0.0.1:8080/v1`:

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8080/v1", api_key="unused")

resp = client.chat.completions.create(
    model="gpt-4.1",
    messages=[{"role": "user", "content": "rename oldName to newName everywhere"}],
    extra_body={"harness": {"workspace": "/path/to/your/project"}},
)
print(resp.choices[0].message.content)
```

Omit `harness.workspace` and the agent gets a fresh empty workspace instead.

`usage` is the total across every upstream call the request made, not just the
last one.

### Sessions

Every response carries an `X-Harness-Session` header. Send that id back to
continue in the same workspace, which also preserves what the agent has
already read — so a follow-up edit does not need a re-read.

There are four ways to name a session, tried in this order. They all exist
because gateways disagree about what they forward: some drop unknown headers
but keep unknown body fields, others do the reverse.

| Channel | Example |
|---|---|
| `X-Harness-Session` header | `X-Harness-Session: ws_abc…` |
| `X-Harness-Workspace` header | `X-Harness-Workspace: /path/to/project` |
| Body | `"harness": {"session_id": "ws_abc…"}` |
| Model suffix | `"model": "gpt-4.1::ws_abc…"` |

The model suffix is ugly and always survives, because `model` is required and
no proxy interprets it. The suffix is stripped before the model name reaches
the provider.

Sessions can also be managed directly:

```sh
curl -X POST localhost:8080/v1/sessions -d '{"workspace":"/path/to/project"}'
curl localhost:8080/v1/sessions
curl -X DELETE localhost:8080/v1/sessions/ws_abc…
```

Deleting an ephemeral session removes its directory; a session bound to a
directory you nominated gives up the handle and leaves your files alone. The
response says which happened.

### Providers

`-upstream-profile` selects the compatibility profile: `openai`,
`openai-reasoning`, `deepseek`, `deepseek-reasoner`, `groq`, `together`,
`vllm`, `ollama`, `anthropic-compat`, or `generic` (the default, and the most
conservative).

If your provider is not listed, override individual fields rather than waiting
for a profile — `upstream.profile_overrides` is a partial profile merged over
the named one.

If the profile is still wrong, the server learns from the provider's own 400s,
retries, and logs what to pin:

```
level=WARN msg="provider rejected a request; adjusting and retrying"
  model=picky fix=max_completion_tokens pin_with=upstream.profile_overrides
```

Capped at three adjustments per model, so a rejection nothing can be inferred
from fails once rather than looping. See [docs/quirks.md](docs/quirks.md).

### Streaming

`"stream": true` works, with one limitation worth knowing. An agent run has
several generations and only the last is the answer — the others are the model
saying "let me check that file" before a tool call. Concatenating them reads
like a transcript of someone thinking out loud, so only the final turn is
emitted.

The consequence is that the final turn is not known to be final until it
arrives without tool calls, so its content is produced before streaming begins.
It is then sent in pieces so progressive renderers behave normally, but there
is **no time-to-first-token benefit** over a non-streaming request. What
streaming buys is the connection staying open and, with the option below,
visibility into the work.

Set `harness.stream_events` for structured progress on `choices[0].delta.harness`:

```json
{"type": "tool_start", "turn": 1, "tool": "read", "call_id": "c1",
 "args": {"path": "main.go"}, "summary": "read main.go"}
```

Standard SDKs ignore unknown keys inside `delta`, so this is safe to leave on
with a client that has never heard of it — verified against `openai-python`.

Configuration layers lowest to highest: built-in defaults, JSON config file
(`-config`), `HARNESS_*` environment variables, then explicitly-passed flags.
A flag left at its zero value does not clobber a value set by file or env.

| Flag | Purpose |
|---|---|
| `-addr` | Listen address. Default `127.0.0.1:8080`. |
| `-allow-non-loopback` | Required to bind anything but loopback. See [Security](#security). |
| `-upstream-base-url` | OpenAI-compatible endpoint. |
| `-upstream-model` | Default model when a request does not name one. |
| `-workspace-root` | Parent directory for ephemeral workspaces. |
| `HARNESS_SHELL_ENABLED=false` | Env only. Drops `bash`, keeping the jailed file tools. |
| `-log-level` / `-log-format` | `debug\|info\|warn\|error`, `json\|text`. |

## Security

Read this before binding to anything routable.

**This server is designed to execute code on behalf of its callers.** Anyone who
can reach `/v1/*` can run any command the server's user can run. That is the
feature, not a flaw — but it means the endpoint is remote code execution by
design. [docs/security.md](docs/security.md) sets out the threat model, what is
actually bounded, and what is not.

The posture is *trusted local users*: prevent accidents and limit blast radius,
not contain an adversary.

- Binds `127.0.0.1` by default. A non-loopback bind requires `-allow-non-loopback`
  **and** configured auth tokens; the server refuses to start otherwise.
- Bearer-token auth on `/v1/*`, constant-time compared. `/healthz` and `/readyz`
  sit outside it.
- File tools are confined to the session workspace by `os.Root`, which resolves
  every path component against a held directory descriptor. A symlink out of the
  tree fails at the syscall, including one planted after the path was validated.
- `bash` is **not** jailed. `cd /etc && cat passwd` works. Process-group kill on
  timeout and an output cap are ergonomics, not containment. Turn it off
  entirely with `shell.enabled=false` if you only want the file tools.
- Credentials are redacted in the log handler rather than at call sites.
  Upstream error bodies are never echoed to clients — several providers reflect
  the request, including the API key.
- Tool calls pass through a deny-only guard chain: repeated identical calls,
  a destructive-command denylist, and commands that cannot finish in the time
  left. The denylist catches accidents, not adversaries.

## Design notes

A few decisions that are load-bearing, and why:

**One wire schema for both directions** (`internal/oai`). The agent loop's
dominant operation is appending to message history and re-sending it. Two type
sets would mean a full conversion every turn and a place for fields to disappear
silently mid-conversation.

**One outbound serialisation chokepoint** (`upstream.BuildBody`). Rules like
"never send `reasoning_content`" — DeepSeek returns 400 — are enforced once
rather than remembered at every call site.

**Tool-call accumulation keyed by `index`**. `id` and `name` are set-if-empty;
`arguments` is only ever concatenated and never parsed until the stream ends,
because fragments split mid-escape. `Index` is `*int` because `0` and absent are
different values that `omitempty` renders identically. Twelve transcripts in
[`testdata/sse`](testdata/sse) pin the provider variations down.

**Read-before-edit as a version check, not a flag.** The ledger records a content
hash, so a file rewritten by a shell command invalidates the observation and
forces a re-read. A confirmed absence is itself an observation, and is what
authorises creating a file. Compaction marks entries stale, because otherwise the
invariant quietly degrades into a rubber stamp once the contents leave context.

**Asymmetric truncation.** File views drop the tail and tell the model how to
continue. Shell output drops the *head*, because the error in a failed build is
at the bottom under the progress log; the full output spills to a file under
`.harness/output/` that the model can page through.

**`bash` is stateless per call.** A fresh `bash -c` each time, with a `workdir`
argument instead of `cd`. Matching DeepSeek Harness here deleted the hardest
code in the package — no sentinel framing, no shell-death respawn, no pipe
bookkeeping — and what a persistent shell would preserve either gets passed
explicitly or lives in the filesystem, which persists anyway.

**Tool error messages are implementation, not decoration.** They are the model's
only recovery signal, so each one says what was wrong and what to do next.

**Provider quirks are data, not code.** There are more providers than anyone
will write structs for, so compatibility is a set of transforms selected by
configuration, and a provider nobody has heard of is a config change. The
transforms never mutate the caller's request: the loop re-sends the running
history every turn, so an in-place edit would compound — a system message
renamed on turn one renamed again on turn two.

**Guards can only deny, never permit.** An allow result would make the outcome
depend on registration order, and every new guard would have to be reasoned
about against every existing one. Deny-only makes the chain monotonic: adding
a guard can only make the system more restrictive, and ordering affects which
*message* the model sees, nothing else.

## Development

```sh
go test -race ./...     # required; the concurrency design depends on it
go vet ./...
gofmt -l ./internal ./cmd
```

## Roadmap

| Phase | Deliverable | Status |
|---|---|---|
| 0 | Skeleton: config, routing, health, graceful shutdown | done |
| 1 | Wire types, passthrough proxy | done |
| 2 | Streaming reader, delta accumulator, transcripts | done |
| 3 | Workspace, path jail, fs-observation ledger, `read` + `glob` | done |
| 4 | The agent loop, `grep` / `write` / `edit` | done |
| 5 | `bash` and the `Shell` interface | done |
| 6 | Session binding, agent streaming, `/v1/sessions` | done |
| 7 | Monotonic tool guards, budgets | done |
| 8 | Provider quirk profiles and autodetect | done |
| 9 | Context compaction and token estimation | next |
| 10 | Eval harness with programmatic checkers | |
| 11 | Hardening, rlimits, audit log, docs | |

## Dependencies

Two, deliberately: [`doublestar/v4`](https://github.com/bmatcuk/doublestar) for
globbing, and the Go standard library. Routing is `net/http`, logging is
`log/slog`. No OpenAI SDK — the quirks layer needs byte-level request control,
and the official SDKs fight unknown fields and non-standard providers.

## License

MIT. See [LICENSE](LICENSE).
