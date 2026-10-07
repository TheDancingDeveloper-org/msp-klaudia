# Model-call logs

`internal/api` writes one JSON line per model-call attempt through the
standard logger:

```
klaudia {"event":"model_call","provider":"openai","host":"api.theclawbay.com","model":"grok","attempt":1,"max":6,"status":400,"status_class":"4xx","request_id":"req_…","latency":812,"retry":true,"error":"opaque 400","session":"…"}
```

Fields are status and shape only. The line never carries the API key, any
auth header, the prompt, or tool arguments. A second line with
`input_tokens` and `output_tokens` is emitted when the stream reports usage.

The TUI discards the standard logger by default so a line cannot land in the
middle of a repaint (`tui.quietStandardLogger`). Point it at a file to keep
the output:

```
KLAUDIA_LOG=/var/log/klaudia/klaudia.log klaudia
```

The file is opened append-only, mode 0600.

## Scraping into Loki

Nothing here deploys anything; this is the Klaudia side only. The estate
already runs Grafana/Loki (`grafanaloki` on Node B) and has a working sidecar
pattern: the `msp-pinglegacy` stack ships a Grafana Alloy container that tails
a log file and pushes to Loki (see `deploy/stacks/msp-pinglegacy/pinglegacy.alloy`
in the msp-agent repo, and the `personal/vogt-*` stacks in ops).

A Klaudia deployment would do the same. Mount the `KLAUDIA_LOG` file into an
Alloy sidecar and scrape it:

```
local.file_match "klaudia" {
  path_targets = [{ __path__ = "/var/log/klaudia/klaudia.log", job = "klaudia" }]
}

loki.source.file "klaudia" {
  targets    = local.file_match.klaudia.targets
  forward_to = [loki.process.klaudia.receiver]
}

loki.process "klaudia" {
  stage.json {
    expressions = {
      session     = "session",
      model       = "model",
      provider    = "provider",
      status_class = "status_class",
    }
  }
  stage.labels {
    values = { session = "", model = "", provider = "", status_class = "" }
  }
  forward_to = [loki.write.estate.receiver]
}
```

`session`, `model`, `provider` and `status_class` become labels, which is what
makes "show me the 4xx for this session" a label query instead of a line
scan. `status_class` is low-cardinality by construction; `session` is one
value per run, so drop it from the label set if a host runs very many short
sessions and rely on the line content instead.
