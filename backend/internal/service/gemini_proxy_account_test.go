//go:build unit

package service

import (
	"bufio"
	"bytes"
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func geminiProxyCandidateAccount() *Account {
	note := "gateway-provider/v1:source"
	return &Account{ID: 21, Name: "source", Notes: &note, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{20}, Concurrency: 1, Credentials: map[string]any{"api_key": "synthetic-proxy-key", "base_url": "https://proxy.example/v1", "model_mapping": map[string]any{"media": "source-route"}, "header_override_enabled": true, "header_overrides": map[string]any{"X-CLIProxy-Auth-ID": "server-A"}}, Extra: map[string]any{"gateway_supply_source_v1": map[string]any{"version": 1, "source_id": "apikey/gemini/source", "primary_account_id": 21, "role": "primary", "publication": map[string]any{"availability_enabled": true, "publish_input": map[string]any{"models": []any{map[string]any{"name": "media", "input_price": 0.01, "output_price": 0.02}}}}}}}
}
func TestGeminiProxyModelOriginQualification(t *testing.T) {
	for _, tc := range []string{"valid", "unmapped", "no-pin", "foreign-origin", "foreign-primary", "wildcard-only", "paused", "no-publication", "image-price", "countTokens"} {
		t.Run(tc, func(t *testing.T) {
			a := geminiProxyCandidateAccount()
			model, action := "media", "generateContent"
			switch tc {
			case "unmapped":
				model = "unknown"
			case "no-pin":
				a.Credentials["header_override_enabled"] = false
			case "foreign-origin":
				a.Extra["gateway_supply_source_v1"].(map[string]any)["source_id"] = "apikey/gemini/foreign"
			case "foreign-primary":
				a.Extra["gateway_supply_source_v1"].(map[string]any)["primary_account_id"] = 22
			case "wildcard-only":
				a.Credentials["model_mapping"] = map[string]any{"*": "source-route"}
			case "paused":
				a.Schedulable = false
			case "no-publication":
				delete(a.Extra["gateway_supply_source_v1"].(map[string]any), "publication")
			case "image-price":
				a.Extra["gateway_supply_source_v1"].(map[string]any)["publication"].(map[string]any)["publish_input"].(map[string]any)["models"] = []any{map[string]any{"name": "media", "image": true, "input_price": 0, "output_price": 0}}
			case "countTokens":
				action = "countTokens"
			}
			require.Equal(t, tc == "valid", GeminiProxyModelAllowed(a, model, action))
		})
	}
}
func TestGeminiProxyBaseAndOriginalGroupScope(t *testing.T) {
	for _, base := range []string{"https://proxy.example", "https://proxy.example/", "https://proxy.example/v1"} {
		a := geminiProxyCandidateAccount()
		a.Credentials["base_url"] = base
		root, err := geminiProxyBaseURL(a)
		require.NoError(t, err)
		target, err := buildGeminiAIStudioModelActionURL(root, "source-route", "generateContent", false)
		require.NoError(t, err)
		require.Equal(t, "https://proxy.example/v1beta/models/source-route:generateContent", target)
	}
	id, foreign := int64(20), int64(21)
	ctx := WithGeminiProxyGroupScope(context.Background(), id)
	require.True(t, geminiProxyGroupScopeAllows(ctx, &id))
	require.False(t, geminiProxyGroupScopeAllows(ctx, &foreign))
	require.False(t, geminiProxyGroupScopeAllows(ctx, nil))
}

type geminiProxyCandidateUpstream struct {
	HTTPUpstream
	requests []*http.Request
	status   int
}

func (u *geminiProxyCandidateUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	var wire bytes.Buffer
	if err := req.Write(&wire); err != nil {
		return nil, err
	}
	received, err := http.ReadRequest(bufio.NewReader(&wire))
	if err != nil {
		return nil, err
	}
	u.requests = append(u.requests, received.WithContext(req.Context()))
	payload := `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"cachedContentTokenCount":2}}`
	if u.status >= 400 {
		payload = `{"error":{"code":503,"message":"unknown-after-write"}}`
	}
	return &http.Response{StatusCode: u.status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
}
func TestGeminiProxyForwardPreservesPinNativeBodyAndSingleAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{200, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			u := &geminiProxyCandidateUpstream{status: status}
			s := &GeminiMessagesCompatService{httpUpstream: u, cfg: &config.Config{}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"unknownExtension":{"keep":true}}`)
			c.Request = httptest.NewRequest("POST", "/v1beta/models/media:generateContent", strings.NewReader(string(body)))
			c.Request.Header.Set("X-CLIProxy-Auth-ID", "forged-B")
			result, err := s.ForwardNative(context.Background(), c, geminiProxyCandidateAccount(), "media", "generateContent", false, body)
			require.Len(t, u.requests, 1)
			req := u.requests[0]
			require.Equal(t, "server-A", req.Header.Get("X-CLIProxy-Auth-ID"))
			require.Equal(t, "1", req.Header.Get("X-CLIProxy-Single-Attempt"))
			require.Equal(t, "/v1beta/models/source-route:generateContent", req.URL.Path)
			actual, readErr := io.ReadAll(req.Body)
			require.NoError(t, readErr)
			require.JSONEq(t, string(body), string(actual))
			require.True(t, HTTPUpstreamVoiceSingleAttempt(req.Context()))
			if status == 200 {
				require.NoError(t, err)
				require.Equal(t, 5, result.Usage.OutputTokens)
			} else {
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover)
			}
		})
	}
}
func TestGeminiProxyRequiresRealExactPositiveTokenPrice(t *testing.T) {
	input, output := 0.01, 0.02
	group := &Group{ModelPricing: []ChannelModelPricing{{Models: []string{"media"}, BillingMode: BillingModeToken, InputPrice: &input, OutputPrice: &output}}}
	require.True(t, geminiProxyExactTokenPrice(group, "media"))
	require.False(t, geminiProxyExactTokenPrice(group, "foreign"))
	input = 0
	require.False(t, geminiProxyExactTokenPrice(group, "media"))
	input = 0.01
	group.ModelPricing[0].Models = []string{"*"}
	require.False(t, geminiProxyExactTokenPrice(group, "media"))
	require.False(t, geminiProxyPositiveTokenPrice(nil))
	require.False(t, geminiProxyPositiveTokenPrice(&ModelPricing{InputPricePerToken: 0, OutputPricePerToken: 1}))
	require.True(t, geminiProxyPositiveTokenPrice(&ModelPricing{InputPricePerToken: 0.01, OutputPricePerToken: 0.02}))
}

func TestGeminiProxyKeyGroupCurrentPriceAuthority(t *testing.T) {
	for _, scenario := range []string{"fresh-full-account-and-group-price", "foreign-group", "allowlist-denied", "fresh-account-disabled", "source-reference-price-only"} {
		t.Run(scenario, func(t *testing.T) {
			full := geminiProxyCandidateAccount()
			input, output := 1e-6, 2e-6
			group := &Group{ID: 20, Platform: PlatformOpenAI, Status: StatusActive, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"media"}}, ModelPricing: []ChannelModelPricing{{Models: []string{"media"}, BillingMode: BillingModeToken, InputPrice: &input, OutputPrice: &output}}}
			groupID := group.ID
			key := &APIKey{GroupID: &groupID, Group: group}
			switch scenario {
			case "foreign-group":
				group.ID, groupID = 22, 22
			case "allowlist-denied":
				group.ModelAllowlist.Models = []string{"foreign"}
			case "fresh-account-disabled":
				full.Schedulable = false
			case "source-reference-price-only":
				group.ModelPricing = nil
			}
			repository := &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{full.ID: full}}
			billing := newTestBillingServiceForResolver()
			// An unrelated fallback remains positive. It and the Source's
			// reference card must never replace current original Group pricing.
			billing.fallbackPrices["media"] = &ModelPricing{InputPricePerToken: .9, OutputPricePerToken: .8}
			service := &GatewayService{accountRepo: repository, resolver: NewModelPricingResolver(nil, billing)}
			selectedSnapshot := &Account{ID: full.ID}
			result, err := service.ValidateGeminiProxyAccountForKey(context.Background(), key, selectedSnapshot, "media", "media", "generateContent")
			require.Equal(t, 1, repository.getByIDCalls, "a scheduler snapshot is not credential authority")
			if scenario == "fresh-full-account-and-group-price" {
				require.NoError(t, err)
				require.Same(t, full, result)
				resolved := service.resolveChannelPricing(context.Background(), "media", key)
				require.Equal(t, PricingSourceGroup, resolved.Source)
				require.Equal(t, input, resolved.BasePricing.InputPricePerToken)
				require.Equal(t, output, resolved.BasePricing.OutputPricePerToken)
			} else {
				require.Error(t, err)
				require.Nil(t, result)
			}
		})
	}
}
