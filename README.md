# harness-api

An OpenAI-compatible HTTP server that runs an agentic coding loop against any
OpenAI-compatible model. Point an existing client at it, and the model gets a
workspace, file tools, and a shell.

> **Status: usable, incomplete.** Phases 0–5 of 11 are done. The agent loop
> works end to end with the full core tool set: `read`, `glob`, `grep`, `edit`,
> `write` and `bash`. Not yet implemented: streaming, provider quirk profiles,
> and context compaction. See [Roadmap](#roadmap).

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
The response carries an `X-Harness-Session` header; send it back as
`harness.session_id` to continue in the same workspace, which also preserves
what the agent has already read.

`usage` is the total across every upstream call the request made, not just the
last one.

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
| 6 | Session binding, agent streaming, `/v1/sessions` | next |
| 7 | Monotonic tool guards, budgets | |
| 8 | Provider quirk profiles and autodetect | |
| 9 | Context compaction and token estimation | |
| 10 | Eval harness with programmatic checkers | |
| 11 | Hardening, rlimits, audit log, docs | |

## Dependencies

Two, deliberately: [`doublestar/v4`](https://github.com/bmatcuk/doublestar) for
globbing, and the Go standard library. Routing is `net/http`, logging is
`log/slog`. No OpenAI SDK — the quirks layer needs byte-level request control,
and the official SDKs fight unknown fields and non-standard providers.

## License

MIT. See [LICENSE](LICENSE).
