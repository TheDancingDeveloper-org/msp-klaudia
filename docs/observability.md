# Model-call logs

`internal/api` writes one JSON object per line for every attempt at a model
call — to **stderr**, except under the TUI (below):

```
{"event":"model_call","provider":"openai","host":"api.theclawbay.com","model":"grok","attempt":1,"max":6,"status":400,"status_class":"4xx","request_id":"req_…","latency":812,"retry":true,"error":"opaque 400","session":"…"}
```

A second line carrying `input_tokens` and `output_tokens` follows when the
stream reports usage.

The fields are status and shape only. A line never carries the API key, any
auth header, prompt content, or tool arguments.

stderr rather than the standard logger is deliberate: a non-interactive run
(`-p`, stream-json, ACP) keeps stdout for its protocol, and Docker captures a
container's stderr regardless.

**The TUI is the exception.** It renders inline on the same terminal stderr
writes to, so an uncoordinated line would tear the frame. While the TUI runs,
model-call lines follow the standard logger (`tui.quietStandardLogger`): they
go to the file named by `KLAUDIA_LOG` when it is set, and are discarded
otherwise.

## Where the lines end up

For a non-interactive Klaudia whose stderr is the container's own (a process
Docker started directly), Node B's promtail picks the lines up through its
docker service-discovery job: estate Loki (grafanaloki, host port 3101), job
`docker/<service>`, labelled `container`, `stack`, `service` and
`host=node-b`.

A Vogt session is **not** that case. Vogt runs Klaudia's TUI on a pseudo-terminal
the engine owns, so its stderr is the session terminal, never the container's
log stream, and nothing reaches Loki on its own. To keep the lines for a Vogt
session, set `KLAUDIA_LOG` to a file (the session's environment or the template
can carry it); getting that file into Loki is a promtail target on the
estate side, not something Klaudia does.

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
