# Streaming transcript fixtures

## Provenance — read this before trusting these files

These transcripts are **hand-written from documented wire formats**, not captured
from live providers. They are constructed to exercise specific framing and
fragmentation behaviours that the documentation and provider changelogs describe.

That distinction matters. A hand-written fixture proves the parser handles the
case *as we understand it*. It does not prove that any particular provider
actually emits that shape today. Where a real capture and a fixture here
disagree, the capture wins.

**Replace these with real captures when API keys are available.** Record with:

```
curl -N https://<provider>/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"...","stream":true,"messages":[...],"tools":[...]}' \
  > testdata/sse/<provider>_<scenario>.sse
```

Keep the bytes exactly as received, including CRLF and blank lines. Strip only the
`Authorization` header if you captured headers too.

## Files

Each file's leading comment lines (`: ...`) state which behaviour it pins down.
Comment lines are also legitimate SSE, so they double as parser input rather than
being stripped before the test reads them.

| File | Behaviour under test |
|---|---|
| `openai_parallel_tools.sse` | Two parallel tool calls; id/name on first fragment only; arguments split across chunks |
| `deepseek_reasoning.sse` | `reasoning_content` deltas preceding `content` deltas |
| `vllm_no_done_no_space.sse` | `data:` with no space; stream ends at EOF with no `[DONE]` |
| `ndjson_plain.ndjson` | No SSE framing at all; one JSON object per line |
| `keepalive_comments.sse` | `:` comment keepalives; one event split across several `data:` lines |
| `indexless_tool_calls.sse` | Two distinct tool calls with no `index` field on any fragment |
| `split_escapes.sse` | Arguments split mid-escape-sequence and mid-`\u` escape |
| `usage_final_chunk.sse` | Trailing usage-only chunk with an empty `choices` array |
| `single_chunk_tool.sse` | A complete tool call delivered in one chunk, arguments included |
| `error_mid_stream.sse` | HTTP 200, then an error object partway through the stream |
| `crlf_line_endings.sse` | CRLF line terminators throughout |
| `empty_args_tool.sse` | Zero-argument call signalled by a lone `"arguments":""` |
