# Model-call logs

`internal/api` writes one JSON object per line to **stderr** for every attempt
at a model call:

```
{"event":"model_call","provider":"openai","host":"api.theclawbay.com","model":"grok","attempt":1,"max":6,"status":400,"status_class":"4xx","request_id":"req_…","latency":812,"retry":true,"error":"opaque 400","session":"…"}
```

A second line carrying `input_tokens` and `output_tokens` follows when the
stream reports usage.

The fields are status and shape only. A line never carries the API key, any
auth header, prompt content, or tool arguments.

stderr rather than the standard logger is deliberate. The TUI discards
`log.Default` so a library's `log.Printf` cannot land in the middle of a
repaint (`tui.quietStandardLogger`), and that would swallow these lines too.
Docker captures a container's stderr regardless.

## Where the lines end up

vogt-prod and vogt-dev run on Node B, and Node B's promtail already scrapes
every container's log stream through its docker service-discovery job. A line
written to stderr is therefore in the estate Loki (grafanaloki, host port
3101) under the existing `docker/<service>` job, labelled `container`,
`stack`, `service`, `job=docker/<service>` and `host=node-b` — the same stream
as the rest of the vogt container's logs. No shipper, promtail change, or
deploy step is needed; this is Klaudia-side only.

Stable keys worth filtering on: `session`, `provider`, `model`, `status`,
`status_class`, `attempt`, `latency`. `status_class` is low-cardinality;
`session` is one value per run.

## Capturing a request the endpoint rejected

The per-attempt line says *that* a call failed, not *what* was sent. Set
`KLAUDIA_DUMP_FAILED_REQUEST` to a file path and each non-2xx response appends
one JSON line containing the outgoing request body with the content removed:
message text, tool-call arguments and image URLs are replaced by their
lengths, tool descriptions are truncated, and no header is written, so nothing
authenticated or secret is in the file. Roles, tool names and the schema
structure are kept, which is the shape that has to be diffed.

```
KLAUDIA_DUMP_FAILED_REQUEST=/tmp/klaudia-failed.jsonl klaudia
```

The decisive reproduction is a capture proxy rather than the dump. Point the
provider's `baseURL` at a proxy that records the raw body and forwards to
`https://api.theclawbay.com/v1`, then diff a body that drew a 400 against one
the same endpoint accepted. The dump is for when that proxy isn't available;
it has already thrown away the content, so it can show the shape but not prove
it.
