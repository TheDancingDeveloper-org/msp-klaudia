package api

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/bedrock"
	"github.com/anthropics/anthropic-sdk-go/option"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// BedrockEndpointEnv overrides the Bedrock runtime endpoint (a VPC interface
// endpoint's DNS name, or a test server). Unset, the SDK targets
// https://bedrock-runtime.<region>.amazonaws.com.
const BedrockEndpointEnv = "KLAUDIA_BEDROCK_ENDPOINT"

// NewBedrock builds a Client for Claude on Amazon Bedrock. It is IAM-
// authenticated API use: requests are SigV4-signed with credentials from the
// AWS default chain (an ECS task role or instance profile in AWS; env vars or a
// shared profile locally), or a Bedrock API key when AWS_BEARER_TOKEN_BEDROCK
// is set. No key material lives in klaudia's config. model is the Bedrock
// model id or inference-profile id (e.g. an AU cross-region profile), sent as
// given. betas is the allow-list of anthropic_beta values forwarded to
// Bedrock; everything else the agent loop would send is dropped, since
// Bedrock rejects beta flags it does not serve.
func NewBedrock(ctx context.Context, region string, betas []string) (*Client, error) {
	region = strings.TrimSpace(region)
	if region == "" {
		region = firstEnv("AWS_REGION", "AWS_DEFAULT_REGION")
	}
	if region == "" {
		return nil, fmt.Errorf("provider \"bedrock\" needs a region: set region in config.toml or AWS_REGION")
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("provider \"bedrock\": loading AWS configuration: %w", err)
	}
	httpc := newHTTPClient()
	opts := []option.RequestOption{
		option.WithHeader("x-app", "cli"),
		option.WithMaxRetries(maxRetries()),
		option.WithHTTPClient(httpc),
		bedrock.WithConfig(awsCfg),
	}
	endpoint := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", region)
	if custom := strings.TrimSpace(os.Getenv(BedrockEndpointEnv)); custom != "" {
		endpoint = strings.TrimRight(custom, "/")
		opts = append(opts, option.WithBaseURL(endpoint))
	}
	allowed := make([]anthropic.AnthropicBeta, 0, len(betas))
	for _, b := range betas {
		if b = strings.TrimSpace(b); b != "" {
			allowed = append(allowed, anthropic.AnthropicBeta(b))
		}
	}
	return &Client{
		sdk:          anthropic.NewClient(opts...),
		httpc:        httpc,
		baseURL:      endpoint,
		bedrock:      true,
		bedrockBetas: allowed,
	}, nil
}

// adaptForBedrock removes what Bedrock does not serve from one request: beta
// flags outside the configured allow-list, and the server-side web_search /
// web_fetch tools (Anthropic-hosted, not available on Bedrock).
func (c *Client) adaptForBedrock(params *anthropic.BetaMessageNewParams) {
	keep := map[anthropic.AnthropicBeta]bool{}
	for _, b := range c.bedrockBetas {
		keep[b] = true
	}
	betas := params.Betas[:0:0]
	for _, b := range params.Betas {
		if keep[b] {
			betas = append(betas, b)
		}
	}
	params.Betas = betas
	tools := params.Tools[:0:0]
	for _, t := range params.Tools {
		if isServerWebTool(t) {
			continue
		}
		tools = append(tools, t)
	}
	params.Tools = tools
}

func isServerWebTool(t anthropic.BetaToolUnionParam) bool {
	return t.OfWebSearchTool20250305 != nil || t.OfWebSearchTool20260209 != nil ||
		t.OfWebSearchTool20260318 != nil || t.OfWebFetchTool20250910 != nil ||
		t.OfWebFetchTool20260209 != nil || t.OfWebFetchTool20260309 != nil ||
		t.OfWebFetchTool20260318 != nil
}

// bedrockTransient matches the exceptions Bedrock sends inside a response
// stream that are worth retrying: throttling, capacity and transient server
// faults. The SDK surfaces them as "received exception <code>: <message>".
var bedrockTransient = regexp.MustCompile(`(?i)received exception (throttlingException|serviceUnavailableException|internalServerException|modelStreamErrorException|modelNotReadyException)`)

// IsBedrockTransient reports whether err is a retryable Bedrock stream exception.
func IsBedrockTransient(err error) bool {
	return err != nil && bedrockTransient.MatchString(err.Error())
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}
