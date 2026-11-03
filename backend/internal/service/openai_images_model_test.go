//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIImagesResponsesDriverAndImageModels(t *testing.T) {
	for _, override := range []string{"", "  ", " gpt-5.6-sol "} {
		t.Run(fmt.Sprintf("override=%q", override), func(t *testing.T) {
			t.Setenv("SUB2API_IMAGES_MAIN_MODEL", override)
			driver := strings.TrimSpace(override)
			if driver == "" {
				driver = "gpt-5.6-luna"
			}
			for _, model := range []string{"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare-2026-09-08"} {
				for _, quality := range []string{"xhigh", "max", "auto"} {
					for _, endpoint := range []string{openAIImagesGenerationsEndpoint, openAIImagesEditsEndpoint} {
						parsed := &OpenAIImagesRequest{Endpoint: endpoint, Model: model, Prompt: "draw a red cup", Quality: quality, Size: "1536x864", Background: "transparent", OutputFormat: "png", N: 1}
						if parsed.IsEdits() {
							parsed.InputImageURLs = []string{"https://example.com/input.png"}
						}
						body, err := buildOpenAIImagesResponsesRequest(parsed, model)
						require.NoError(t, err)
						require.Equal(t, driver, gjson.GetBytes(body, "model").String())
						require.Equal(t, model, gjson.GetBytes(body, "tools.0.model").String())
						require.Equal(t, quality, gjson.GetBytes(body, "tools.0.quality").String())
						require.Equal(t, "1536x864", gjson.GetBytes(body, "tools.0.size").String())
						require.Equal(t, "transparent", gjson.GetBytes(body, "tools.0.background").String())
						if parsed.IsEdits() {
							require.Equal(t, "edit", gjson.GetBytes(body, "tools.0.action").String())
							require.Equal(t, parsed.InputImageURLs[0], gjson.GetBytes(body, "input.0.content.1.image_url").String())
						}
					}
				}
				req := map[string]any{"model": model, "input": "draw a red cup"}
				require.True(t, normalizeOpenAIResponsesImageOnlyModel(req))
				require.Equal(t, driver, req["model"])
				require.Equal(t, model, req["tools"].([]any)[0].(map[string]any)["model"])
				require.False(t, normalizeOpenAIResponsesImageOnlyModel(req), "a valid driver must not be overwritten")
			}
			req := map[string]any{"model": "gpt-6-astra", "tools": []any{map[string]any{"type": "image_generation", "model": "gpt-image-2.5-sunburst"}}}
			require.False(t, normalizeOpenAIResponsesImageOnlyModel(req))
			require.Equal(t, "gpt-6-astra", req["model"])
		})
	}
}

func TestOpenAIImagesRejectedDriverDoesNotCoolImageModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SUB2API_IMAGES_MAIN_MODEL", "gpt-5.4-mini")
	for _, rejected := range []string{"gpt-5.4-mini", "gpt-image-2.5-flare"} {
		t.Run(rejected, func(t *testing.T) {
			repo := &modelNotFoundAccountRepoStub{}
			svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{accountRepo: repo}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, openAIImagesGenerationsEndpoint, nil)
			body := fmt.Sprintf(`{"error":{"message":"The '%s' model is not supported when using Codex with a ChatGPT account.","type":"invalid_request_error"}}`, rejected)
			resp := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			_, err := svc.handleOpenAIImagesErrorResponse(WithOpenAIImagesEndpoint(context.Background()), resp, c, openAICodexPlanGatedOAuthAccount(), "gpt-image-2.5-flare")
			require.Error(t, err)
			if rejected == "gpt-5.4-mini" {
				var upstreamErr *OpenAIImagesUpstreamError
				require.ErrorAs(t, err, &upstreamErr)
				require.Equal(t, 400, upstreamErr.StatusCode)
				require.Contains(t, upstreamErr.Message, rejected)
				require.Empty(t, repo.modelRateLimitCalls)
				require.Zero(t, repo.tempCalls)
			} else {
				require.Len(t, repo.modelRateLimitCalls, 1, "actual image-model rejection still needs bounded failover")
			}
		})
	}
}

func TestGPTImage25PricingDoesNotUseLegacyImageRates(t *testing.T) {
	for _, model := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare-2026-09-08", "gpt-image-2.5-sunburst-2026-09-08"} {
		svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{"gpt-image-2": {InputCostPerToken: 2.5e-6, OutputCostPerImageToken: 15e-6}}}
		p := svc.GetModelPricing(model)
		require.NotNil(t, p)
		require.Equal(t, 5e-6, p.InputCostPerToken)
		require.Equal(t, 8e-6, p.InputCostPerImageToken)
		require.Equal(t, 30e-6, p.OutputCostPerImageToken)
		require.Equal(t, 1.25e-6, p.CacheReadInputTokenCost)
		require.Zero(t, p.OutputCostPerToken)
		custom := &LiteLLMModelPricing{InputCostPerToken: 7e-6}
		svc.pricingData[model] = custom
		require.Same(t, custom, svc.GetModelPricing(model), "explicit pricing must win")
	}
}

func TestGPTImage25AccountModelPermissions(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		for _, model := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
			for _, mapping := range []map[string]any{nil, {model: model}} {
				account := &Account{Platform: PlatformOpenAI, Type: accountType, Credentials: map[string]any{"model_mapping": mapping}}
				require.True(t, account.IsModelSupported(model))
				require.Equal(t, model, account.GetMappedModel(model))
			}
			restricted := &Account{Platform: PlatformOpenAI, Type: accountType, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-image-2": "gpt-image-2"}}}
			require.False(t, restricted.IsModelSupported(model), "an explicit administrator allowlist must remain restricted")
		}
	}
}

func TestGPTImage25UsagePreservesImageInputTokens(t *testing.T) {
	svc := &OpenAIGatewayService{}
	var usage OpenAIUsage
	svc.parseOpenAIImagesSSEUsageBytes([]byte(`{"type":"response.completed","response":{"tool_usage":{"image_gen":{"input_tokens":1550,"input_tokens_details":{"image_tokens":1521,"text_tokens":29},"output_tokens":515,"output_tokens_details":{"image_tokens":515}}}}}`), &usage)
	require.Equal(t, 1550, usage.InputTokens)
	require.Equal(t, 1521, usage.ImageInputTokens)
	require.Equal(t, 515, usage.ImageOutputTokens)
}

const seedreamModelFixture = "doubao-seedream-5pro-synthetic"
const seedreamProfileFixture = "ark-agent-plan-seedream-5pro-v1"

func seedreamAccountFixture(public, route string) *Account {
	return &Account{ID: 91, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic-relay", "base_url": "https://example.invalid/v1", "model_mapping": map[string]any{public: route}, "image_profiles": map[string]any{route: seedreamProfileFixture}}}
}
func TestSeedreamTrustedFinalMapping(t *testing.T) {
	route := "source-owned/" + seedreamModelFixture
	for _, tc := range []struct {
		name, public, channel, mapped, mapKey, profile, accountType string
		reject                                                      bool
	}{
		{name: "literal", public: seedreamModelFixture, mapped: route},
		{name: "ordinary_public_account_mapping", public: "gpt-image-1", mapped: route},
		{name: "ordinary_public_channel_mapping", public: "gpt-image-1", channel: seedreamModelFixture, mapped: route},
		{name: "wrong_vendor_map_key", public: "gpt-image-1", mapped: route, mapKey: seedreamModelFixture, reject: true},
		{name: "missing_profile", public: "gpt-image-1", mapped: route, profile: "missing", reject: true},
		{name: "invalid_profile", public: "gpt-image-1", mapped: route, profile: "untrusted-v99", reject: true},
		{name: "seedream_mapped_gpt", public: seedreamModelFixture, mapped: "gpt-image-1", reject: true},
		{name: "oauth_account", public: seedreamModelFixture, mapped: route, accountType: AccountTypeOAuth, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := tc.public
			if tc.channel != "" {
				selected = tc.channel
			}
			account := seedreamAccountFixture(selected, tc.mapped)
			if tc.accountType != "" {
				account.Type = tc.accountType
			}
			key := tc.mapped
			if tc.mapKey != "" {
				key = tc.mapKey
			}
			profile := seedreamProfileFixture
			if tc.profile != "" {
				profile = tc.profile
			}
			if profile == "missing" {
				delete(account.Credentials, "image_profiles")
			} else {
				account.Credentials["image_profiles"] = map[string]any{key: profile}
			}
			raw := []byte(fmt.Sprintf(`{"model":%q,"prompt":"draw","size":"1K","n":1}`, tc.public))
			c, _ := newOpenAIImagesTestContext(t, raw)
			svc := &OpenAIGatewayService{}
			parsed, err := svc.ParseOpenAIImagesRequest(c, raw)
			require.NoError(t, err)
			require.Empty(t, parsed.SeedreamProfile, "parser must not grant trust")
			err = qualifySeedreamImageProfile(account, parsed, tc.channel)
			if tc.reject {
				var prewrite *SeedreamImageProfileError
				require.ErrorAs(t, err, &prewrite)
				require.Empty(t, parsed.SeedreamProfile)
			} else {
				require.NoError(t, err)
				require.Equal(t, seedreamProfileFixture, parsed.SeedreamProfile)
				require.Equal(t, "1K", parsed.SizeTier)
			}
		})
	}
}

func TestSeedreamOriginalJSONRejectsBeforeForward(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","stream":null}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","stream":false,"stream":false}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","str\u0065am":false,"stream":false}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","size":"2K"}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","watermark":null}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","n":1.9}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","n":1.0}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","n":null}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K"}{}`,
	} {
		t.Run(body, func(t *testing.T) {
			raw := []byte(body)
			c, _ := newOpenAIImagesTestContext(t, raw)
			upstream := &httpUpstreamRecorder{resp: openAIImagesJSONResponse()}
			svc := newOpenAIImagesTestService(upstream)
			parsed, err := svc.ParseOpenAIImagesRequest(c, raw)
			if err == nil {
				_, err = svc.ForwardImages(context.Background(), c, seedreamAccountFixture("gpt-image-1", "source-owned/"+seedreamModelFixture), raw, parsed, "")
			}
			require.Error(t, err)
			require.Nil(t, upstream.lastReq, "original JSON must be rejected before downstream preparation")
		})
	}
}

func TestSeedreamForwardImageCountSizeAndErrorClose(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		reject     bool
	}{
		{"one_1K", `{"data":[{"b64_json":"aGVsbG8=","size":"1024x1024"}]}`, 200, false},
		{"one_2K", `{"data":[{"b64_json":"aGVsbG8=","size":"2048x2048"}]}`, 200, false},
		{"no_size", `{"data":[{"b64_json":"aGVsbG8="}]}`, 200, true},
		{"two_images", `{"data":[{"b64_json":"AA==","size":"1K"},{"b64_json":"AQ==","size":"1K"}]}`, 200, true},
		{"wrong_size", `{"data":[{"b64_json":"AA==","size":"2K"}]}`, 200, true},
		{"no_1_5K_guess", `{"data":[{"b64_json":"AA==","size":"1.5K"}]}`, 200, true},
		{"error_body_closed", `{"error":{"message":"synthetic rejected"}}`, 502, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size := "1K"
			if tc.name == "one_2K" {
				size = "2K"
			}
			raw := []byte(fmt.Sprintf(`{"model":"gpt-image-1","prompt":"draw","size":%q,"n":1}`, size))
			c, _ := newOpenAIImagesTestContext(t, raw)
			price1K, price2K := 0.20, 0.30
			group := &Group{ImagePrice1K: &price1K}
			if size == "2K" {
				group.ImagePrice2K = &price2K
			}
			c.Set("api_key", &APIKey{Group: group})
			closed := false
			readCloser := &seedreamTrackingBody{Reader: strings.NewReader(tc.body), closed: &closed}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: readCloser}}
			svc := newOpenAIImagesTestService(upstream)
			parsed, err := svc.ParseOpenAIImagesRequest(c, raw)
			require.NoError(t, err)
			result, err := svc.ForwardImages(context.Background(), c, seedreamAccountFixture("gpt-image-1", "source-owned/"+seedreamModelFixture), raw, parsed, "")
			require.NotNil(t, upstream.lastReq)
			require.True(t, HTTPUpstreamImageSingleAttempt(upstream.lastReq.Context()))
			require.False(t, HTTPUpstreamVoiceSingleAttempt(upstream.lastReq.Context()))
			require.Equal(t, "source-owned/"+seedreamModelFixture, gjson.GetBytes(upstream.lastBody, "model").String())
			require.True(t, closed)
			if tc.reject {
				require.Error(t, err)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, result.ImageCount)
				require.Equal(t, size, result.ImageSize)

				svc.billingService = &BillingService{}
				cost := svc.calculateOpenAIImageCost(context.Background(), seedreamModelFixture, getAPIKeyFromContext(c), result, 1)
				expected := price1K
				if size == "2K" {
					expected = price2K
				}
				require.InDelta(t, expected, cost.ActualCost, 1e-12)
				require.Equal(t, string(BillingModeImage), cost.BillingMode)
				require.Equal(t, "source-owned/"+seedreamModelFixture, result.UpstreamModel)
			}
		})
	}
}

type seedreamTrackingBody struct {
	io.Reader
	closed *bool
}

func (b *seedreamTrackingBody) Close() error { *b.closed = true; return nil }
