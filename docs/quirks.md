# Provider compatibility

"OpenAI-compatible" describes a shape, not a specification. Every provider
implements a slightly different subset and rejects a slightly different set of
fields, and the disagreements are mundane rather than exotic: which parameter
carries the token limit, what the system role is called, and whether an
assistant message with tool calls should have `null` content, `""` content, or
no content key at all.

This server handles that with **profiles** — data, not code — plus a fallback
that learns from the provider's own error messages.

## Choosing a profile

```sh
export HARNESS_UPSTREAM_PROFILE=deepseek
# or
./harness-api -upstream-profile deepseek
```

| Profile | Token field | System role | Sampling | Schema | Notes |
|---|---|---|---|---|---|
| `generic` | `max_tokens` | `system` | yes | basic | Default. Sends only what everything accepts. |
| `openai` | `max_completion_tokens` | `system` | yes | full | `strict`, `parallel_tool_calls`, stream usage |
| `openai-reasoning` | `max_completion_tokens` | `developer` | **no** | full | o-series and similar |
| `deepseek` | `max_tokens` | `system` | yes | full | |
| `deepseek-reasoner` | `max_tokens` | `system` | **no** | full | |
| `anthropic-compat` | `max_tokens` | `system` | yes | full | Omits empty tool-call content |
| `groq` | `max_tokens` | `system` | yes | full | |
| `together` | `max_tokens` | `system` | yes | basic | |
| `vllm` | `max_tokens` | `system` | yes | basic | Empty-string content, strips `<think>` |
| `ollama` | `max_tokens` | `system` | yes | basic | Empty-string content, strips `<think>` |

`generic` is deliberately the most restrictive. Being too conservative costs a
feature; being too liberal costs a 400 that looks like the model is broken.

> **Provenance.** These profiles come from provider documentation and from
> error messages reported in the wild. They have **not** each been verified
> against a live endpoint. Where a profile here and a real provider disagree,
> the provider is right. Fix it with an override rather than waiting for a
> release.

## When your provider is not listed

Override individual fields. No code change, no new profile:

```json
{
  "upstream": {
    "profile": "generic",
    "profile_overrides": {
      "max_tokens_field": "max_completion_tokens",
      "system_role": "developer",
      "sampling": false,
      "schema_dialect": "basic"
    }
  }
}
```

Every field of a profile can be overridden:

| Field | Values | What it does |
|---|---|---|
| `max_tokens_field` | `max_tokens`, `max_completion_tokens` | Which parameter carries the output limit |
| `system_role` | `system`, `developer`, `user` | `user` folds system text into the first user message |
| `tool_call_content` | `null`, `empty`, `omit` | Empty assistant content alongside tool calls |
| `parallel_tool_calls` | bool | Whether the field may be sent at all |
| `strict_schemas` | bool | Whether function defs may carry `strict` |
| `sampling` | bool | Whether `temperature`/`top_p`/penalties may be sent |
| `max_stop_sequences` | int | Clamp on the `stop` array; 0 means no limit |
| `schema_dialect` | `full`, `basic` | How much JSON Schema the provider can parse |
| `strip_think_tags` | bool | Remove inline `<think>…</think>` from responses |
| `stream_format` | `""`, `sse`, `ndjson` | Force stream framing; empty sniffs it |
| `stream_usage` | bool | Whether `stream_options` is understood |
| `context_window` | int | Total token budget, for compaction |

## Autodetect

If the profile is still wrong, the server learns from the rejection instead of
failing. On a 400 whose message names a parameter, it adjusts, retries once,
and logs what it inferred:

```
level=WARN msg="provider rejected a request; adjusting and retrying"
  model=picky fix=max_completion_tokens pin_with=upstream.profile_overrides
  provider_said="Unsupported parameter: 'max_tokens' is not supported..."
```

That turns "wrong profile" from a hard failure into a slower success plus a
line telling you exactly what to pin. **Pin it** — otherwise you pay a failed
request for the same lesson on every process restart.

Recognised complaints:

| Inferred fix | Triggered by, roughly |
|---|---|
| `max_completion_tokens` | "Unsupported parameter: 'max_tokens' … use 'max_completion_tokens'" |
| `max_tokens` | "unknown field 'max_completion_tokens'" |
| `system-role` | "'developer' is not a valid role" |
| `developer-role` | "'system' is not a valid role" |
| `no-parallel-tool-calls` | "Extra inputs are not permitted: parallel_tool_calls" |
| `no-sampling` | "This model does not support temperature" |
| `no-strict` | "unknown parameter 'strict'" |
| `basic-schema` | "unsupported schema keyword: oneOf" |
| `no-stream-usage` | "Extra inputs are not permitted: stream_options" |

Two bounds keep this from becoming a problem of its own:

- **At most 3 adjustments per model.** Without a cap, a provider rejecting
  something that cannot be inferred produces an unbounded retry loop that looks
  like a hang and costs money on every attempt.
- **Each fix is adopted once.** A provider that keeps repeating one complaint
  fails rather than looping.

Learned adjustments are **per model**, not per endpoint. Two models behind one
base URL can have different rules, and applying one model's lesson to the other
would be a fresh source of failures.

An error that cannot be interpreted — a quota message, a bad API key, a missing
model — is never guessed at. Adopting a wrong adjustment would persist for the
life of the process.

## Enforced for every profile

Some things are not configurable because getting them wrong is a hard failure
rather than a degradation:

- **`reasoning_content` is never sent upstream.** DeepSeek returns 400 when it
  sees its own reasoning echoed back in a later request. The same applies to
  `reasoning` and `thinking` keys captured from unknown-field passthrough.
- **The `harness` extension object never reaches a provider.**
- **`stream_options` is dropped from non-streaming requests.**

These live at the single outbound serialisation point in
`internal/upstream/client.go`, so "never send X" is enforced once rather than
remembered at each call site.

## Schema dialects

`basic` restricts tool parameter schemas to flat objects with primitive typed
properties. Self-hosted runtimes compile the schema into a constrained decoder
and either reject anything richer or silently mishandle it — which surfaces as
the model emitting arguments that do not match what was asked for, rather than
as an error.

All built-in tools already keep their schemas inside `basic`, so the dialect
currently affects nothing. It matters for future tools, and there is a test
asserting that both dialects stay free of `oneOf`, `anyOf`, `allOf`, `$ref`
and `format`.
