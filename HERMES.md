# Aurora for Hermes Agent

This fork makes Aurora work out of the box as an OpenAI-compatible backend for
[Hermes Agent](https://github.com/NousResearch/hermes-agent) — including full
tool-calling support over streaming connections.

Upstream Aurora emulates tool calling on top of the ChatGPT Web backend via a
`<tool_call>` text protocol, but returned a plain JSON body whenever `tools`
were present. Hermes always prefers `stream=true`, so the SDK's SSE parser
failed on every tool-enabled request. This fork fixes that and more.

## What this fork changes

1. **Streaming tool calls (the big one).** `stream=true` + `tools` now returns
   a canonical OpenAI SSE sequence:
   `role chunk -> text deltas -> tool_call head (id/type/name) -> arguments
   delta -> finish_reason=tool_calls -> usage (if requested) -> [DONE]`.
   The upstream ChatGPT call remains non-streaming internally (the
   refusal-retry loop needs the full text), and the result is replayed as SSE.
   Verified byte-for-byte with the OpenAI Python SDK 2.24.0 that Hermes uses.
2. **`tool_call_id` -> tool name resolution.** Hermes sends `role=tool`
   messages with only `tool_call_id` (no `name`). Aurora now resolves the name
   from the preceding assistant message's `tool_calls`, so tool results are
   labeled correctly in the ChatGPT prompt.
3. **`content: null`** (not `""`) on responses carrying `tool_calls`, per the
   OpenAI spec.
4. **Full JSON Schemas in the tool prompt.** Upstream flattened each tool's
   schema to a one-line-per-param summary, losing nested objects/arrays/enums.
   Hermes ships ~20 tools with rich schemas (cronjob, computer_use, ...);
   they are now embedded minified and intact.

## Requirements

- Go 1.24+ to build (or Docker).
- **At least one logged-in ChatGPT account.** Tool calling is gated behind
  `CapToolCalling` which requires login — free/noauth accounts get
  `403 Tool calling requires a logged-in ChatGPT account`.
  Provide any one of:
  - `access_tokens.txt` — one ChatGPT `access_token` per line
  - `session_tokens.txt` — one `__Secure-next-auth.session-token` per line
  - `refresh_tokens.txt` — one OpenAI `refresh_token` per line

## Run

```bash
git clone https://github.com/SyntaxSugarRush/aurora
cd aurora
go build -o aurora
echo "YOUR_ACCESS_TOKEN" > access_tokens.txt
./aurora            # listens on 0.0.0.0:8080
```

Useful env vars:

```env
Authorization=choose-a-local-api-key   # clients must send Bearer <key>
TOOL_CALLING_ENABLED=true              # default
STREAM_MODE=true                       # default
REFUSAL_RETRIES=3                      # sandbox-refusal retry budget
DEBUG_TOOL_LOG=tool_debug.log          # trace tool parsing when debugging
```

## Point Hermes at it

`~/.hermes/config.yaml`:

```yaml
model: gpt-5-6            # recommended for tool use (gpt-5-5-instant also good;
                          # avoid "auto" — inconsistent tool-call compliance)
                          # choices: auto, gpt-5-6[-thinking|-pro],
                          # gpt-5-5-{instant,thinking,pro}, gpt-5,
                          # gpt-4o, gpt-4o-mini, o3, o4-mini[-high]
providers:
  custom:
    name: aurora
    base_url: http://127.0.0.1:8080/v1
    api_key: choose-a-local-api-key    # must match Authorization env; any
                                       # value works if Authorization is unset
```

Then `hermes` as usual. Tool calling, streaming, parallel tool calls, and
usage accounting all work.

## Known limitations (inherent to ChatGPT Web emulation)

- **Tool-call turns are buffered, not live-streamed.** The model's answer is
  collected fully (to allow refusal retries), then replayed as SSE. Chat turns
  without tools stream live as usual.
- **Model compliance is stochastic.** The ChatGPT Web model sometimes denies
  the tools exist or fabricates a tool output instead of emitting
  `<tool_call>`. Aurora detects both (English + Portuguese patterns) and
  retries (`REFUSAL_RETRIES`, default 3), which absorbs most failures.
  Measured first-attempt tool-call rates: `gpt-5-6` and `gpt-5-5-instant`
  ~100%, `gpt-5-6-thinking` ~75%, `auto` ~50%. **Use `gpt-5-6` or
  `gpt-5-5-instant` for agent workloads; avoid `auto`.**
- Tool results arrive at the model as a user-role message
  (`Tool (name): <output>` — ChatGPT Web rejects a literal `tool` author role
  with a 500), and the model is nudged to treat them as ground truth.
- `temperature`/`top_p`/penalties are best-effort: ChatGPT Web does not expose
  sampling controls, so they're injected as prompt hints only.

## Verifying your setup

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer choose-a-local-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5-6", "stream": true,
    "messages": [{"role": "user", "content": "What files are in /tmp?"}],
    "tools": [{"type": "function", "function": {"name": "terminal",
      "description": "Run a shell command",
      "parameters": {"type": "object",
        "properties": {"command": {"type": "string"}},
        "required": ["command"]}}}]
  }'
```

Expect SSE chunks ending with `finish_reason":"tool_calls"` and `[DONE]`.
