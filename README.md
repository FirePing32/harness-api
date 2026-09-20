# harness-api

An OpenAI-compatible HTTP server that runs an agentic coding loop against any
OpenAI-compatible model. Point an existing client at it, and the model gets a
workspace, file tools, and a shell.

> **Status: built, and now in first contact with a real model.** All eleven
> phases are done: the core tool set (`read`, `glob`, `grep`, `edit`, `write`,
> `bash`), streaming, persistent sessions, tool guards, provider quirk
> profiles, context compaction, resource ceilings, an audit log, and an eval
> harness with eighteen programmatically checked tasks. It runs end to end
> against a live provider, and under `-race` on Linux and macOS.
>
> **There are still no suite numbers here.** Individual runs work; the full
> suite has not been run to completion, so every capability claim below remains
> an argument from design. What the first real runs did produce is
> [two corrections to this design](#what-first-contact-changed), which is the
> more useful early return.

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

### Long tasks

When a conversation approaches the model's context window, the server compacts
it: first by dropping the bodies of old tool results, then — only if that is
not enough — by summarising older turns. The system prompt and your original
task survive verbatim; the most recent turns are kept untouched.

Files whose contents were dropped have their read-before-edit observation
invalidated, so a later edit is refused with an instruction to re-read rather
than applied against contents the model no longer holds.

The context window comes from the provider profile. If the profile does not
know one, compaction is **disabled** rather than guessing — set
`context.window` to enable it.

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
| `-audit-log` | Append every command and refusal to this file. Off by default. |
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
  Upstream 4xx bodies *are* relayed, because "unknown model" is worth seeing —
  but only after credential scrubbing, since several providers reflect the
  request including the API key. Anything meaning "this server's account is the
  problem" (401, 402, 403) is replaced wholesale and returned as 502.
- Tool calls pass through a deny-only guard chain: repeated identical calls,
  a destructive-command denylist, and commands that cannot finish in the time
  left. The denylist catches accidents, not adversaries.
- Commands run under processor-time and file-size ceilings that they cannot
  lift. Memory and process count are *not* bounded by default; the reasons are
  in [docs/security.md](docs/security.md) and they are not good news.
- `-audit-log` records every command and refusal, separately from the
  operational log so that quietening one does not lose the other.

## Evaluation

```sh
go build -o harness-eval ./cmd/harness-eval

harness-eval run -model gpt-4.1 -label baseline -out baseline.json
harness-eval compare baseline.json candidate.json
```

Eighteen tasks, three repetitions each, checked by programs rather than by an LLM
judge — judges disagree with themselves across runs, and a regression detector
that is itself noisy detects noise. Tracked: pass@1, mean turns to success,
tokens per task, and **tool error rate per tool**. That last one is the direct
feedback signal on tool ergonomics: if `edit` errors on a third of its calls,
the fix is in `edit.go`, not in the prompt.

Two things it refuses to do. A run that never reached the model is excluded
from the pass-rate denominator and reported separately, because a rate-limited
afternoon otherwise looks identical to a capability regression. And `compare`
runs a Fisher exact test before calling a difference real — at three
repetitions per task, 20/30 against 24/30 is p = 0.38, and a tool that reports
that as a win gets used to justify changes that did nothing.

Both directions of every checker are verified without a model:
`go test ./internal/eval/` asserts that no checker passes an untouched repo,
and `./evals/verify-checkers.sh` asserts that each one accepts a real solution
and rejects the specific near-miss it exists to catch. See
[docs/evals.md](docs/evals.md).

## What first contact changed

Nine phases of design reasoning, then an hour against a real model. Two of the
conclusions did not survive, and both were found by watching turn-level
progress events rather than by anything the test suite could check.

**A rule was enforced but never stated.** The system prompt said "read a file
before *editing* it". The observation ledger also required it before
*creating*, and nothing told the model that — so creating one file took four
turns: `write` refused, `read` the missing path, `write` again, answer. Ten
thousand tokens for one line of text. The model behaved well throughout; it
read the error, understood the refusal, and recovered. The defect was upstream
of it.

**The fix for that did not work.** Stating the rule in the prompt changed
nothing — the model still went straight to `write`, identical first call,
measured twice. The commit is kept in history rather than squashed, because a
failed fix with its measurement attached is worth more to the next reader than
a tidy story.

**So the rule itself was wrong.** Looking at why it existed: it was meant to
stop a create from clobbering a concurrent creator. It never did. `write`
stats the path under the session lock immediately before authorising, so "not
there" holds at the moment of the write and creating it destroys nothing.
Requiring an earlier read actually *widened* the race it was meant to close —
one turn apart rather than microseconds. It cost a turn on every file creation
and protected against nothing, and it is gone. Everything that can actually
lose bytes is still refused.

**Honest accounting on the result:** removing the refusal did not make runs
shorter. Turn count stayed at four, because the model spent the freed turn on a
redundant second read. A guaranteed-wasted turn was eliminated; no efficiency
gain has been demonstrated. Saying otherwise would be exactly the kind of claim
this suite exists to stop.

A separate pass found a 402 "insufficient balance" from a provider being
relayed to clients as a 400 "invalid request" — telling the caller to fix a
request that was fine. And it found this README promising that upstream error
bodies are "never echoed to clients", when in fact 4xx bodies *are* relayed
after credential scrubbing. Overstating a security guarantee is worse than
understating one: it invites the reader to stop checking whether the scrubbing
is adequate, and the scrubbing is the entire control.

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
forces a re-read. Creating a file that is not there needs no prior read at all:
the path is stat'd under the session lock immediately before the write, so
nothing can be destroyed. Requiring an earlier read bought nothing and cost a
measured turn on every file creation. Compaction marks entries stale, because
otherwise the invariant quietly degrades into a rubber stamp once the contents
leave context.

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

**Compaction prunes before it summarises.** A long run overflows the window
because tool results are large, not because the conversation is long — twenty
file reads at 40 KB each is most of a context window, and almost none of it is
still needed. Dropping old tool-result bodies is free and usually enough;
summarising costs a generation and loses detail. Doing it the other way round
spends both to solve a problem that deleting stale file contents would have
solved for nothing.

**Token estimates correct themselves instead of vendoring a tokenizer.**
OpenAI's tokenizer says nothing useful about Llama or Qwen, and this server
talks to whatever it is pointed at. Every response carries `usage.prompt_tokens`
for a request whose byte count is known exactly — a free labelled sample — so a
byte ratio converges on the real tokenizer within a few turns, for any
tokenizer, with no dependency.

**Resource ceilings are applied with `ulimit`, not `setrlimit`.** Go's
`SysProcAttr` has no rlimit fields on any unix and there is no pre-exec hook, so
the plan's approach did not exist. A wrapper that sets the limits and then
`exec`s the real shell keeps the model's command as a separate argv element, so
it is never re-parsed and a syntax error still reports the line number the model
expects. Bash's bare `ulimit -t N` sets soft and hard together, which makes the
ceiling one-way — there is a test asserting a command cannot raise it, because
without that the whole mechanism would be decorative.

**Guards can only deny, never permit.** An allow result would make the outcome
depend on registration order, and every new guard would have to be reasoned
about against every existing one. Deny-only makes the chain monotonic: adding
a guard can only make the system more restrictive, and ordering affects which
*message* the model sees, nothing else.

## Development

```sh
go test -race ./...          # required; the concurrency design depends on it
go vet ./...
gofmt -l ./internal ./cmd
./evals/verify-checkers.sh   # the eval fixtures, no model needed
```

CI runs all four on Linux *and* macOS. Both, deliberately: half of what this
project does is syscall-shaped — `os.Root`, process groups, signal semantics,
rlimits — and those are exactly the things that differ between the two. The
SIGINT finding in phase 5 and the `/var`-to-`/private/var` one in phase 10 were
both platform behaviour a single-OS matrix would have shipped.

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
| 9 | Context compaction and token estimation | done |
| 10 | Eval harness with programmatic checkers | done |
| 11 | Resource ceilings, audit log, CI, suite expansion | done |
| — | First real-model runs; suite pass not yet completed | in progress |

## Dependencies

Two, deliberately: [`doublestar/v4`](https://github.com/bmatcuk/doublestar) for
globbing, and the Go standard library. Routing is `net/http`, logging is
`log/slog`. No OpenAI SDK — the quirks layer needs byte-level request control,
and the official SDKs fight unknown fields and non-standard providers.

## License

MIT. See [LICENSE](LICENSE).
