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

### Credentials

- The upstream API key is never logged. Redaction happens in the `slog`
  handler, so omitting it at a call site is not a possible mistake.
- Upstream error bodies are **never** echoed to clients. Several providers
  reflect the request they received, Authorization header included.
- An upstream 401 is reported to the client as 502, because the client's
  credential was fine — the server's was rejected.

## What is not bounded

Read this list as a whole. Each item is a deliberate choice, not an oversight.

**`bash` is not confined to the workspace.** `cd /etc && cat passwd` works.
`curl … | sh` works. Reading the caller's SSH keys works. The path jail covers
the file tools and nothing else. The timeout and output cap are ergonomics —
they stop a hung test suite and a runaway log — and they are not containment.

**There is no resource limit on commands.** No CPU, memory, file-size or
process-count ceiling. A fork bomb will do what a fork bomb does. (`setrlimit`
is planned for a later phase and will narrow this, not close it.)

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
