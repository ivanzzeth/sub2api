//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiProxyChatAudioInputAndTextOutputScope(t *testing.T) {
	for _, format := range []string{"wav", "mp3"} {
		body := []byte(`{"messages":[{"content":[{"type":"input_audio","input_audio":{"data":"opaque-base64","format":"` + format + `"}}]}]}`)
		require.True(t, geminiProxyChatAudioInputSupported(body))
		require.True(t, geminiProxyChatTextOutputOnly(body))
	}
	require.False(t, geminiProxyChatAudioInputSupported([]byte(`{"messages":[{"content":[{"type":"input_audio","input_audio":{"data":"x","format":"mp4"}}]}]}`)))
	require.False(t, geminiProxyChatTextOutputOnly([]byte(`{"modalities":["text","audio"]}`)))
	require.False(t, geminiProxyChatTextOutputOnly([]byte(`{"audio":{"format":"wav","voice":"alloy"}}`)))
	require.Equal(t, "video_url", chatConversionUnsupportedMedia([]byte(`{"messages":[{"content":[{"type":"video_url","video_url":{"url":"https://fixture.invalid/video"}}]}]}`)))
}

// Reuse the real wire recorder while returning the protocol used by raw Chat.
type geminiProxyAudioChatUpstream struct{ geminiProxyCandidateUpstream }

func (u *geminiProxyAudioChatUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	resp, err := u.geminiProxyCandidateUpstream.Do(req, proxy, accountID, concurrency)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(strings.NewReader(`{"id":"chatcmpl-audio-fixture","model":"source-route","choices":[{"index":0,"message":{"role":"assistant","content":"fixture text"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	return resp, nil
}

func TestGeminiProxyChatAudioRawWirePreservesStandardInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []string{"single-alias", "shared-target", "padded-target", "allowlist-denied"} {
		t.Run(scenario, func(t *testing.T) {
			full := geminiProxyCandidateAccount()
			if scenario == "shared-target" {
				full.Credentials["model_mapping"].(map[string]any)["media2"] = "source-route"
				publication := full.Extra["gateway_supply_source_v1"].(map[string]any)["publication"].(map[string]any)["publish_input"].(map[string]any)
				publication["models"] = append(publication["models"].([]any), map[string]any{"name": "media2", "input_price": .03, "output_price": .04})
			}
			if scenario == "padded-target" {
				full.Credentials["model_mapping"].(map[string]any)["media"] = " source-route "
			}
			input, output := 1e-6, 2e-6
			group := &Group{ID: 20, Platform: PlatformOpenAI, Status: StatusActive, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"media"}}, ModelPricing: []ChannelModelPricing{{Models: []string{"media"}, BillingMode: BillingModeToken, InputPrice: &input, OutputPrice: &output}}}
			if scenario == "allowlist-denied" {
				group.ModelAllowlist.Models = []string{"foreign"}
			}
			groupID := group.ID
			key := &APIKey{GroupID: &groupID, Group: group}
			repository := &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{full.ID: full}}
			upstream := &geminiProxyAudioChatUpstream{geminiProxyCandidateUpstream: geminiProxyCandidateUpstream{status: http.StatusOK}}
			service := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream, accountRepo: repository, resolver: NewModelPricingResolver(nil, newTestBillingServiceForResolver())}
			// A tiny PCM WAV container is transport data, not an audio-understanding oracle.
			wav := []byte{'R', 'I', 'F', 'F', 38, 0, 0, 0, 'W', 'A', 'V', 'E', 'f', 'm', 't', ' ', 16, 0, 0, 0, 1, 0, 1, 0, 0x40, 0x1f, 0, 0, 0x80, 0x3e, 0, 0, 2, 0, 16, 0, 'd', 'a', 't', 'a', 2, 0, 0, 0, 0, 0}
			encoded := base64.StdEncoding.EncodeToString(wav)
			body := []byte(`{"model":"media","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"` + encoded + `","format":"wav"}},{"type":"text","text":"Transcribe this fixture"}]}],"modalities":["text"],"unknownExtension":{"keep":true}}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			c.Request.Header.Set("X-CLIProxy-Auth-ID", "forged-B")
			c.Set("api_key", key)
			result, err := service.ForwardAsChatCompletions(context.Background(), c, full, body, "", "")
			if scenario == "allowlist-denied" {
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, upstream.requests)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Equal(t, 1, repository.getByIDCalls)
			require.Len(t, upstream.requests, 1)
			request := upstream.requests[0]
			require.Equal(t, "/v1/chat/completions", request.URL.Path)
			require.Equal(t, "server-A", request.Header.Get("X-CLIProxy-Auth-ID"))
			require.Equal(t, "1", request.Header.Get("X-CLIProxy-Single-Attempt"))
			require.True(t, HTTPUpstreamVoiceSingleAttempt(request.Context()))
			require.True(t, HTTPUpstreamRedirectsDisabled(request.Context()))
			actual, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			require.Equal(t, "source-route", gjson.GetBytes(actual, "model").String())
			require.Equal(t, encoded, gjson.GetBytes(actual, "messages.0.content.0.input_audio.data").String())
			require.Equal(t, "wav", gjson.GetBytes(actual, "messages.0.content.0.input_audio.format").String())
			require.JSONEq(t, string(ReplaceModelInBody(body, "source-route")), string(actual))
		})
	}
}

func TestGeminiProxyChatExecutionScopeRejectsUnqualifiedOrChangedRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []string{"inbound-headers-only", "changed-target", "different-account"} {
		t.Run(scenario, func(t *testing.T) {
			full := geminiProxyCandidateAccount()
			upstream := &geminiProxyAudioChatUpstream{geminiProxyCandidateUpstream: geminiProxyCandidateUpstream{status: http.StatusOK}}
			service := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Request.Header.Set("X-CLIProxy-Auth-ID", "forged-B")
			c.Request.Header.Set("X-CLIProxy-Single-Attempt", "1")
			ctx, target := context.Background(), "source-route"
			if scenario != "inbound-headers-only" {
				ctx = withGeminiProxyChatExecutionScope(ctx, full, "media", 20)
				if scenario == "changed-target" {
					target = "foreign-route"
				} else {
					full = geminiProxyCandidateAccount()
				}
			}
			response, err := service.sendCCUpstreamRequest(ctx, c, full, "https://proxy.example/v1/chat/completions", []byte(`{"model":"`+target+`","messages":[]}`), false, "synthetic-proxy-key", "", "")
			if scenario == "inbound-headers-only" {
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Len(t, upstream.requests, 1)
				require.Empty(t, upstream.requests[0].Header.Get("X-CLIProxy-Single-Attempt"))
				require.False(t, HTTPUpstreamVoiceSingleAttempt(upstream.requests[0].Context()))
				require.False(t, HTTPUpstreamRedirectsDisabled(upstream.requests[0].Context()))
			} else {
				require.Error(t, err)
				require.Nil(t, response)
				require.Empty(t, upstream.requests)
			}
		})
	}
}
