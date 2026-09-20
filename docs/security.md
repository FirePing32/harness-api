# Security

## What this server is

`harness-api` accepts an HTTP request and, on behalf of whoever sent it, reads
and writes files and runs shell commands on the machine it is running on.

**That is remote code execution by design.** It is the feature. Anyone who can
reach `POST /v1/chat/completions` can run any command the server's user can run.

The rest of this document is about what that does and does not imply, because
the useful question is not "is it safe" — it isn't, inherently — but "what is
actually bounded, and what isn't".

## Threat model

The posture is **trusted local users**: prevent accidents and limit blast
radius. It is not "contain an adversary", and nothing here should be read as
if it were.

Concretely, this server assumes:

- Everyone who can reach the listener is someone you would give a shell to.
- The model driving it may be wrong, confused, or manipulated by content it
  reads, but is not an attacker with a goal.
- The machine is not shared with untrusted users.

If any of those is false for your deployment, the controls below are not
sufficient and you want a container boundary. `Shell` is an interface
(`internal/workspace/shell.go`) precisely so a sandboxed implementation can be
added without touching the tool, the agent loop, or anything above them.

## What is bounded

### The file tools

`read`, `write`, `edit`, `glob` and `grep` cannot leave the session's
workspace. This is enforced by `os.Root`, which resolves every path component
with `openat` relative to a held directory descriptor — a kernel check, not a
string comparison.

That means the following all fail, and are tested:

| Attempt | Result |
|---|---|
| `../../etc/passwd` | rejected before any syscall |
| `/etc/passwd` | rejected: absolute path outside the root |
| a symlink inside the workspace pointing at `/etc` | fails at the syscall |
| a symlink planted *after* the path was validated | fails at the syscall |
| a path containing a NUL byte | rejected |

The last row is the reason for `os.Root` rather than the more obvious
`filepath.EvalSymlinks` followed by a prefix test: that approach is a
time-of-check-to-time-of-use race by construction.

Symlinks *within* the workspace are followed normally — repositories use them.

### Request-level ceilings

Every agent request is bounded on four axes, each with a finite default:

| Bound | Default | Config key |
|---|---|---|
| Tool-calling turns | 50 | `agent.max_iterations` |
| Wall clock | 10m | `agent.max_wall_clock` |
| Upstream tokens | 1,000,000 | `agent.max_total_tokens` |
| Request body | 1 MiB | `server.max_body_bytes` |

A request may lower any of these; it cannot raise them. The caller is not the
one paying the upstream bill.

### Command execution

| Bound | Default | Config key |
|---|---|---|
| Per-command timeout | 2m | `shell.default_timeout` |
| Maximum requested timeout | 10m | `shell.max_timeout` |
| Output returned to the model | 30 KiB | `shell.tail_bytes` |
| Overflow file written | 5 MiB | `shell.spill_bytes` |

On timeout the **entire process group** is terminated, not just the shell:
SIGTERM, a two-second grace period, then SIGKILL. Without this, `npm test`
leaves node and its workers running long after the request returned.

SIGTERM rather than SIGINT is deliberate and was verified by measurement. POSIX
requires a non-interactive shell without job control to set SIGINT to *ignored*
in background children, so `something &` inside `bash -c` cannot be interrupted
by SIGINT at all — bash dies and the child keeps running.

### Resource ceilings

| Bound | Default | Config key |
|---|---|---|
| Processor time | timeout x core count | `shell.cpu_factor` |
| Single file size | 1 GiB | `shell.max_file_size_kb` |
| Concurrent processes | off | `shell.max_processes` |

Applied with the shell's own `ulimit`, through a wrapper that execs the real
shell: `sh -c 'ulimit ...; exec "$0" "$@"' bash -c <command>`. The plan called
for `setrlimit` via `SysProcAttr`, which does not exist — Go's `SysProcAttr`
carries no rlimit fields on any unix and there is no pre-exec hook. The model's
command stays a separate argv element, so it is never re-parsed and a syntax
error still reports the line number the model would expect.

These cannot be lifted by the command: lowering a hard limit is irreversible
for a non-root process, and a command that tries gets "Operation not
permitted". There is a test asserting exactly that, because without it the
mechanism would be decorative.

The wrapper is `bash` rather than `sh`, which is load-bearing. `ulimit -f`
counts blocks and the block size is shell-dependent — bash uses 1024 bytes,
dash uses 512 — so setting the limit in one shell and running the command in
another enforces a different number than was configured. Setting and running in
the same shell removes the ambiguity.

The processor-time ceiling sets the soft limit below the hard one rather than
both together. With them equal, Linux delivers SIGXCPU and SIGKILL in the same
instant and the wait status reports the SIGKILL — the same code as the timeout
sweep, leaving nothing able to tell a processor-time kill from a timeout.

**The processor-time ceiling is not a second wall clock.** At the default
factor it is the most CPU time a command respecting its timeout could possibly
use, so it never fires for well-behaved work. What it catches is a process that
left its process group by double-forking and survived the group kill — an
rlimit is inherited across fork and exec, so it follows the escapee, and
nothing else here would ever stop it.

**`max_processes` is off by default and that is a real gap.** `RLIMIT_NPROC`
counts every process owned by the real user id, not the ones this command
started, so a value chosen for a dedicated service account will refuse the
first fork on a shared login that already has several hundred. A ceiling that
turns every command into an inexplicable failure is worse than no ceiling: the
model cannot tell a policy refusal from a bug, so it rephrases instead of
adapting. Set it when the server has a user to itself.

**Address space is not limited at all.** macOS rejects `ulimit -v` outright,
and on Linux it breaks the Go toolchain and the JVM, both of which reserve
large virtual mappings they never touch.

A command killed by a ceiling exits 152 (SIGXCPU) or 153 (SIGXFSZ), and the
tool result says in words which ceiling, what the value was, and that any file
being written is truncated. A bare exit code there is indistinguishable from
the command being wrong, and a model that reads it that way rewrites a command
that was fine.

### The audit log

Off unless `audit.path` (or `-audit-log`) is set. When on, every shell command
and every guard refusal is appended as one JSON object per line, flushed as it
happens.

It is deliberately separate from the operational log, which is levelled and
routinely turned down to warn. A trail that disappears when someone quietens
the logs is not a trail.

| Recorded | |
|---|---|
| `command` | session, workspace, the command, workdir, exit code, duration, ceilings in force |
| `denied` | session, tool, which guard, and the reason given to the model |

**Commands are recorded verbatim, including any credentials in them.**
`curl -H "Authorization: Bearer sk-..."` is a thing models write. Everywhere
else in this server credentials are redacted in the log handler so that leaking
one is not a possible mistake; here they are not, because a record of
*approximately* what ran cannot answer the question an audit log exists to
answer. The file is created `0600` and is as sensitive as whatever passes
through those commands. Do not ship it to a log aggregator without thinking
about what is in it.

A failed audit write never fails the request that triggered it, and is reported
once rather than on every subsequent command. Otherwise a full disk becomes an
outage, and anyone who wanted the audit trail switched off would have a way to
switch it off.

### Context compaction

A conversation approaching the model's window is compacted rather than allowed
to overflow. This has one security-adjacent consequence worth stating: when a
file's contents are dropped from the conversation, its read-before-edit
observation is invalidated, so a later edit is refused rather than applied
against contents the model no longer has.

That invariant degrading silently was the specific failure this guards against.
The ledger would go on vouching for bytes nobody could see, the check would
pass, and an edit would be written from the model's recollection.

### Tool guards

Every tool call is inspected before it runs. Guards can only **deny**, never
permit, which makes the chain monotonic: no guard can undo another's refusal,
and adding one can only ever make the agent more cautious. That removes an
entire class of bug where "is this permitted" has a different answer depending
on registration order.

| Guard | What it stops |
|---|---|
| `repeat-tool` | A fourth identical call to the same tool in one request |
| `command-denylist` | `rm -rf /`, fork bombs, `curl \| sh`, `mkfs`, `dd of=/dev/…`, `shutdown` |
| `timeout-policy` | A command asking for more time than the request has left |

A denied call comes back to the model as an ordinary tool result explaining
what was refused and what to do instead, so it can change approach rather than
rephrase the same command.

**`command-denylist` is ergonomics, not a security boundary.** The patterns
match command text, and command text has unlimited ways to say the same thing:
`r''m -rf /`, `$(echo rm) -rf /`, a variable holding the path, a script that
does it. Anything *trying* to get past it will. What it catches is the
accident — a path constructed badly so that `rm -rf /tmp/build/` becomes
`rm -rf /`. Patterns are anchored to command position, so a dangerous word
inside a quoted string is not matched.

Configure with `guards.deny_commands` (RE2 patterns; `null` uses the built-in
list, `[]` disables the check) or turn the whole chain off with
`guards.enabled=false`.

### Credentials

- The upstream API key is never logged. Redaction happens in the `slog`
  handler, so omitting it at a call site is not a possible mistake.
- Upstream error bodies **are** relayed to clients for 4xx statuses, after
  being credential-scrubbed by `logx.Redact`. They are relayed because a
  provider saying "unknown model" or "unsupported parameter" is the most useful
  thing available to whoever has to fix it.

  The scrubbing is a regex over `sk-…`, `Bearer …` and `api_key`/`token`
  values, and it is what stands between a provider that reflects the request —
  Authorization header included, which several do — and a client that should
  not see it. Treat it as the load-bearing control it is, and do not assume
  these bodies are withheld.
- Statuses that mean **this server's account is the problem** are replaced
  entirely rather than relayed: 401 and 403 (credentials), and 402 (billing).
  All three become 502. Passing them through would tell the caller to fix their
  own token or their own request, which is the opposite of the truth.
- Failed authentication is logged at warn with the path and remote address,
  but never the token presented: a near-miss would put a credential in a log
  file, and a wrong guess is still somebody's real password somewhere else.

## What is not bounded

Read this list as a whole. Each item is a deliberate choice, not an oversight.

**`bash` is not confined to the workspace.** `cd /etc && cat passwd` works.
`curl … | sh` works. Reading the caller's SSH keys works. The path jail covers
the file tools and nothing else. The timeout and output cap are ergonomics —
they stop a hung test suite and a runaway log — and they are not containment.

**Memory is not limited, and neither is process count by default.** A fork bomb
will do what a fork bomb does unless `shell.max_processes` is set, and nothing
bounds resident memory at all. Processor time and file size *are* bounded — see
[Resource ceilings](#resource-ceilings) — which narrows this rather than
closing it.

**There is no network restriction.** A command can reach anything the host can,
including link-local metadata endpoints on a cloud instance.

**Sessions are a single trust domain.** Session ids are unguessable, and one
session's *file tools* cannot reach another's workspace — but `bash` can, since
it is not jailed. There is no tenant isolation and none is claimed.

**Prompt injection is not defended against.** An agent that reads a file
containing instructions may follow them. With an unjailed shell available, the
blast radius of that is the server's user account. This is the sharpest edge
in the whole design: treat repository contents the agent reads as capable of
directing it.

## Running it safely

In rough order of importance:

1. **Keep the default bind.** `127.0.0.1` is the default. Binding elsewhere
   requires `-allow-non-loopback` *and* configured `server.auth_tokens`; the
   server refuses to start otherwise, and logs a warning banner when it does.

2. **Set `server.auth_tokens` anyway.** Cheap, and it stops a stray process or
   a browser page on the same machine from driving the agent. Compared with
   `subtle.ConstantTimeCompare`.

3. **Run it as a dedicated user.** The blast radius of everything above is that
   user's permissions. Do not run it as yourself if you can avoid it, and do
   not run it as root.

4. **Turn off the shell if you do not need it.** `shell.enabled=false`, or
   `HARNESS_SHELL_ENABLED=false`. The file tools remain, and they *are*
   jailed. This converts the threat model from "shell access" to "read and
   write inside one directory", which is a categorically different thing. When
   the shell is off the tool is not offered to the model at all, so it plans
   around the tools it has rather than repeatedly trying one that refuses.

5. **Point workspaces at disposable checkouts.** `harness.workspace` binds a
   session to a directory you nominate, and a session bound that way is never
   deleted by this server. But the agent can still change anything in it. Use a
   working tree whose state you can recover with `git`.

## Reporting

This is a personal project under active development. Open an issue at
<https://github.com/FirePing32/harness-api/issues>. Do not expect a
coordinated-disclosure process.
