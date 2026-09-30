# Claude on Amazon Bedrock (`provider = "bedrock"`)

Klaudia can talk to Claude through Amazon Bedrock instead of the Anthropic API. It is the same
native client and the same agent loop; only the transport changes. The SDK's `bedrock` option
rewrites each Messages request to Bedrock's `InvokeModelWithResponseStream`, signs it with
SigV4, and translates Bedrock's event stream back to the usual SSE events. Streaming, tool use,
usage accounting, the stream-idle watchdog and prompt caching work as they do on the Anthropic
provider.

```toml
provider = "bedrock"
region = "ap-southeast-2"                              # or AWS_REGION / AWS_DEFAULT_REGION
model = "au.anthropic.claude-sonnet-4-5-20250929-v1:0" # a Bedrock model id or inference-profile id
# bedrockBetas = ["context-management-2025-06-27"]      # anthropic_beta flags to forward (default: none)
```

- **Credentials** always come from the AWS default credential chain: an ECS task role or
  instance profile in AWS, or `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_PROFILE` locally.
  `AWS_BEARER_TOKEN_BEDROCK` (a Bedrock API key) is honoured when set. There is no key material
  in `config.toml`. This is IAM-authenticated API use, not a subscription login.
- **Model**: required. Bedrock has no Klaudia default, and Claude aliases (`sonnet`) are not
  translated. Use an **inference-profile id** to keep processing inside a geography, for example
  an `au.` cross-region profile for Australian data residency.
- **Region** is withheld from an untrusted project's config, as `provider` and `baseURL` are,
  because it decides where prompts are processed.
- **Betas**: Bedrock rejects `anthropic_beta` flags it does not serve, so every flag the agent
  would send is dropped unless it is listed in `bedrockBetas`.
- **Server-side web tools** (`web_search`, `web_fetch`) are Anthropic-hosted and are removed
  from Bedrock requests.
- **Errors**: throttling and capacity exceptions (`throttlingException`,
  `serviceUnavailableException`, `internalServerException`, `modelStreamErrorException`,
  `modelNotReadyException`) are classified as transient. HTTP 429/5xx are retried by the SDK as
  usual.
- **Endpoint**: `KLAUDIA_BEDROCK_ENDPOINT` overrides `https://bedrock-runtime.<region>.amazonaws.com`,
  for example to use a VPC interface endpoint's DNS name.

`klaudia --doctor` reports the auth kind as `aws`. Credentials are resolved at request time, so
a missing role shows up on the first turn as an AWS error.
