package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

func TestBuildGrokVoiceURL_UsesAPIDefaultForCLIProxyBase(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"base_url": xai.DefaultCLIBaseURL,
		},
	}
	url, err := buildGrokVoiceURL(account, nil, "tts")
	require.NoError(t, err)
	require.Equal(t, xai.DefaultBaseURL+"/tts", url)

	url, err = buildGrokVoiceURL(account, nil, "realtime")
	require.NoError(t, err)
	require.Equal(t, xai.DefaultBaseURL+"/realtime", url)
}

func TestBuildGrokVoiceURL_EmptyBaseFallsBackToAPI(t *testing.T) {
	account := &Account{
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{},
	}
	url, err := buildGrokVoiceURL(account, nil, "stt")
	require.NoError(t, err)
	require.Equal(t, xai.DefaultBaseURL+"/stt", url)
}

func TestBuildGrokVoiceURL_RequiresEndpoint(t *testing.T) {
	account := &Account{Platform: PlatformGrok, Type: AccountTypeOAuth}
	_, err := buildGrokVoiceURL(account, nil, "  ")
	require.Error(t, err)
}

func TestBuildGrokVoiceURL_EncodesCustomVoicePathSegments(t *testing.T) {
	account := &Account{Platform: PlatformGrok, Type: AccountTypeOAuth}
	got, err := buildGrokVoiceURL(account, nil, "custom-voices/nlbqfwie/audio")
	require.NoError(t, err)
	require.Equal(t, xai.DefaultBaseURL+"/custom-voices/nlbqfwie/audio", got)

	_, err = buildGrokVoiceURL(account, nil, "custom-voices/../audio")
	require.Error(t, err)
}

func TestForwardGrokVoice_RejectsNonGrok(t *testing.T) {
	svc := &OpenAIGatewayService{}
	_, err := svc.ForwardGrokVoice(context.Background(), nil, &Account{Platform: PlatformOpenAI}, "tts", []byte(`{}`), "application/json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not supported")
}

func TestAwaitGrokRealtimeAudioObservedReadsFlagAfterRelayExits(t *testing.T) {
	errCh := make(chan error, 1)
	var observed atomic.Bool
	go func() {
		observed.Store(true)
		errCh <- io.EOF
	}()
	got, err := awaitGrokRealtimeAudioObserved(errCh, &observed)
	require.ErrorIs(t, err, io.EOF)
	require.True(t, got, "audioObserved must be read after the relay returns, not before <-errCh")
}

func TestGrokRealtimeEventHasAudio(t *testing.T) {
	require.False(t, grokRealtimeEventHasAudio([]byte(`{"type":"session.created"}`)))
	require.False(t, grokRealtimeEventHasAudio([]byte(`{"type":"response.audio_transcript.delta","delta":"hi"}`)))
	require.False(t, grokRealtimeEventHasAudio([]byte(`{"type":"response.audio.delta","delta":""}`)))
	require.True(t, grokRealtimeEventHasAudio([]byte(`{"type":"response.audio.delta","delta":"abc"}`)))
	require.True(t, grokRealtimeEventHasAudio([]byte(`{"type":"response.output_audio.delta","audio":"abc"}`)))
}

func TestForwardGrokVoice_RejectsUnknownEndpoint(t *testing.T) {
	svc := &OpenAIGatewayService{}
	_, err := svc.ForwardGrokVoice(context.Background(), nil, &Account{Platform: PlatformGrok}, "unknown", []byte(`{}`), "application/json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported")
}

type voiceForwardWireUpstream struct {
	HTTPUpstream
	client *http.Client
}

func (u voiceForwardWireUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}
func TestVoiceSingleAttemptContextDefault(t *testing.T) {
	require.False(t, HTTPUpstreamVoiceSingleAttempt(nil))
	require.False(t, HTTPUpstreamVoiceSingleAttempt(context.Background()))
	require.True(t, HTTPUpstreamVoiceSingleAttempt(WithHTTPUpstreamVoiceSingleAttempt(nil)))
}
func TestForwardGrokVoiceFreshHeadersRedirectAndAttempt(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			var wires atomic.Int32
			var pin, authorization string
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wires.Add(1)
				pin = r.Header.Get("X-CLIProxy-Auth-ID")
				authorization = r.Header.Get("Authorization")
				w.Header().Set("Location", "https://example.invalid/never-follow")
				w.WriteHeader(307)
				_, _ = w.Write([]byte("redirect"))
			}))
			defer target.Close()
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			service := &OpenAIGatewayService{cfg: cfg, httpUpstream: voiceForwardWireUpstream{client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}}
			account := &Account{ID: 17, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "server-private-proxy-key", "base_url": target.URL, "header_override_enabled": true, "header_overrides": map[string]any{"x-cliproxy-auth-id": "server-native-A"}}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(method, "/v1/tts", strings.NewReader(`{}`))
			c.Request.Header["x-CLIPROXY-auth-ID"] = []string{"customer-B"}
			c.Request.Header.Set("Authorization", "Bearer customer")
			result, err := service.ForwardGrokVoice(context.Background(), c, account, "tts", []byte(`{}`), "application/json")
			require.Nil(t, result)
			require.Error(t, err)
			require.True(t, GrokVoiceRequestAttempted(err))
			require.EqualValues(t, 1, wires.Load())
			require.Equal(t, "server-native-A", pin)
			require.Equal(t, "Bearer server-private-proxy-key", authorization)
			require.Equal(t, 307, rec.Code)
			require.Equal(t, "https://example.invalid/never-follow", rec.Header().Get("Location"))
			require.Equal(t, "redirect", rec.Body.String())
		})
	}
}
func TestForwardGrokVoiceRejectsEvenBlankCustomerIdempotencyBeforeWire(t *testing.T) {
	for _, key := range []string{"Idempotency-Key", "x-idempotency-key"} {
		var wires atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { wires.Add(1) }))
		cfg := &config.Config{}
		cfg.Security.URLAllowlist.AllowInsecureHTTP = true
		service := &OpenAIGatewayService{cfg: cfg, httpUpstream: voiceForwardWireUpstream{client: srv.Client()}}
		account := &Account{ID: 17, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture", "base_url": srv.URL}}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/tts", nil)
		c.Request.Header[key] = []string{""}
		_, err := service.ForwardGrokVoice(context.Background(), c, account, "tts", nil, "")
		require.Error(t, err)
		require.False(t, GrokVoiceRequestAttempted(err))
		require.Equal(t, 400, rec.Code)
		require.Zero(t, wires.Load())
		srv.Close()
	}
}

// This service fixture honors the existing repository context contract; native
// repository redirect behavior is covered by its unchanged regression tests.
type voiceGETRedirectUpstream struct {
	HTTPUpstream
	client *http.Client
	t      *testing.T
}

func (u voiceGETRedirectUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if !HTTPUpstreamRedirectsDisabled(req.Context()) {
		u.t.Error("GET missing native no-redirect marker")
	}
	if HTTPUpstreamVoiceSingleAttempt(req.Context()) {
		u.t.Error("GET changed to unsafe single-attempt")
	}
	client := *u.client
	if HTTPUpstreamRedirectsDisabled(req.Context()) {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client.Do(req)
}
func TestForwardGrokVoiceGETTerminalRedirectNoSourceB(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		for _, destination := range []string{"http://source-a.test/redirect", "http://source-a.test:9876/redirect", "http://sibling.test/redirect", "http://child.source-a.test/redirect", "http://unrelated.test/redirect"} {
			t.Run(fmt.Sprintf("%d/%s", code, destination), func(t *testing.T) {
				var origin, target atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirect" {
						target.Add(1)
						t.Error("redirect target received source credentials")
						return
					}
					origin.Add(1)
					if r.Header.Get("X-CLIProxy-Auth-ID") != "server-native-A" || r.Header.Get("Authorization") != "Bearer server-private-proxy-key" || r.Header.Get("X-Custom-Credential") != "fixture-A" {
						t.Error("fresh selected source credentials incorrect")
					}
					w.Header().Set("Location", destination)
					w.WriteHeader(code)
					_, _ = w.Write([]byte("terminal redirect"))
				}))
				defer server.Close()
				transport := http.DefaultTransport.(*http.Transport).Clone()
				transport.Proxy = nil
				transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
				}
				defer transport.CloseIdleConnections()
				client := &http.Client{Transport: transport}
				cfg := &config.Config{}
				cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				service := &OpenAIGatewayService{cfg: cfg, httpUpstream: voiceGETRedirectUpstream{client: client, t: t}}
				account := &Account{ID: 17, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "server-private-proxy-key", "base_url": "http://source-a.test", "header_override_enabled": true, "header_overrides": map[string]any{"x-cliproxy-auth-id": "server-native-A", "X-Custom-Credential": "fixture-A"}}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/custom-voices", nil)
				c.Request.Header.Set("Authorization", "Bearer customer-B")
				c.Request.Header.Set("X-CLIProxy-Auth-ID", "customer-B")
				result, err := service.ForwardGrokVoice(context.Background(), c, account, "custom-voices", nil, "")
				require.Error(t, err)
				require.Nil(t, result, "redirect must not produce normal success/usage result")
				require.False(t, GrokVoiceRequestAttempted(err), "GET must not become unsafe")
				require.Equal(t, code, rec.Code)
				require.Equal(t, destination, rec.Header().Get("Location"))
				require.Equal(t, "terminal redirect", rec.Body.String())
				require.True(t, IsResponseCommitted(c))
				require.EqualValues(t, 1, origin.Load())
				require.Zero(t, target.Load())
				require.Nil(t, client.CheckRedirect, "shared client must remain unchanged")
			})
		}
	}
}

type speechPriceGroupRepo struct {
	GroupRepository
	group *Group
}

func (r speechPriceGroupRepo) GetByIDLite(context.Context, int64) (*Group, error) {
	return r.group, nil
}

type speechHTTPUpstreamFixture struct {
	HTTPUpstream
	client *http.Client
	target string
	t      *testing.T
}

func (u speechHTTPUpstreamFixture) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if !HTTPUpstreamRedirectsDisabled(req.Context()) || !HTTPUpstreamVoiceSingleAttempt(req.Context()) || req.GetBody != nil {
		u.t.Error("speech lost fresh/no-replay transport contract")
	}
	copy := req.Clone(req.Context())
	target, _ := url.Parse(u.target)
	target.Path = req.URL.Path
	copy.URL = target
	copy.Host = ""
	return u.client.Do(copy)
}

func TestStandardSpeechActualNativeFeeAndSafeResponseBoundary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		price      *float64
		perRequest bool
		words      string
		ok         bool
	}{
		{"normal", speechPricePointer(30), false, "7", true}, {"zero-usage", speechPricePointer(30), false, "0", true},
		{"nil", nil, false, "7", false}, {"free", speechPricePointer(0), false, "7", false}, {"negative", speechPricePointer(-1), false, "7", false}, {"nan", speechPricePointer(math.NaN()), false, "7", false}, {"infinity", speechPricePointer(math.Inf(1)), false, "7", false}, {"effective-per-request", speechPricePointer(30), true, "7", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wires atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wires.Add(1)
				if r.URL.Path != "/v1/audio/speech" {
					t.Error("wrong internal route")
				}
				w.Header().Set("Content-Type", "audio/mpeg")
				w.Header().Set(speechPrivateCountHeader, tc.words)
				w.Header().Set("Set-Cookie", "never")
				w.Header().Set("X-Supplier-Secret", "never")
				_, _ = io.WriteString(w, "ID3\x04\x00\x00\x00\x00\x00\x00")
			}))
			defer srv.Close()
			gid := int64(41)
			group := &Group{ID: gid, Platform: PlatformOpenAI, AudioTTSPricePerMillionChars: tc.price, RateMultiplier: 1}
			ch := &ChannelService{groupRepo: speechPriceGroupRepo{group: group}}
			rows := []ChannelModelPricing{}
			if tc.perRequest {
				rows = append(rows, ChannelModelPricing{Models: []string{"doubao-seed-tts-2.0"}, Platform: PlatformOpenAI, BillingMode: BillingModePerRequest})
			}
			ch.cache.Store(populateChannelCache([]Channel{{ID: 1, Status: StatusActive, GroupIDs: []int64{gid}, ModelPricing: rows}}, map[int64]string{gid: PlatformOpenAI}))
			cfg := &config.Config{}
			svc := &OpenAIGatewayService{cfg: cfg, channelService: ch, resolver: NewModelPricingResolver(ch, &BillingService{}), httpUpstream: speechHTTPUpstreamFixture{client: srv.Client(), target: srv.URL, t: t}}
			key := &APIKey{ID: 5, GroupID: &gid, Group: group, User: &User{ID: 8}}
			account := &Account{ID: 9, GroupIDs: []int64{gid}, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-proxy-client", "base_url": "https://proxy.fixture.invalid/v1", "model_mapping": map[string]any{"doubao-seed-tts-2.0": "source-A/doubao-seed-tts-2.0"}, "speech_profiles": map[string]any{"source-A/doubao-seed-tts-2.0": speechProfileV1}, "upstream_protocols": map[string]any{"source-A/doubao-seed-tts-2.0": string(speechProtocolVolcengineTTS)}}}
			parsed, err := ParseOpenAISpeechRequest([]byte(`{"model":"doubao-seed-tts-2.0","input":"你好，hello!"}`))
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/audio/speech", nil)
			result, billingKey, err := svc.ForwardOpenAISpeech(context.Background(), c, key, account, parsed, parsed.Model)
			if !tc.ok {
				require.Error(t, err)
				require.Zero(t, wires.Load())
				return
			}
			require.NoError(t, err)
			require.Equal(t, int32(1), wires.Load())
			require.Equal(t, tc.words, rec.Header().Get(speechPublicCountHeader))
			require.Empty(t, rec.Header().Get(speechPrivateCountHeader))
			require.Empty(t, rec.Header().Get("Set-Cookie"))
			require.Empty(t, rec.Header().Get("X-Supplier-Secret"))
			require.Equal(t, "10", rec.Header().Get("Content-Length"))
			require.Zero(t, result.Usage.InputTokens)
			words, _ := strconv.ParseFloat(tc.words, 64)
			require.Equal(t, words/1e6, result.AudioUsage.DurationOrUnits)
			cost := (&BillingService{}).CalculateAudioCost("tts", result.AudioUsage.DurationOrUnits, groupAudioPriceConfigFromAPIKey(billingKey), 0.5)
			require.InDelta(t, words/1e6*30*0.5, cost.ActualCost, 1e-12)
		})
	}
}
func speechPricePointer(v float64) *float64 { return &v }

func TestStandardSpeechProfileAndNumericInputReject(t *testing.T) {
	for _, extra := range []string{`,"speed":"1"`, `,"speed":null`, `,"voice":null`, `,"input":"duplicate"`} {
		_, err := ParseOpenAISpeechRequest([]byte(`{"model":"doubao-seed-tts-2.0","input":"hi"` + extra + `}`))
		require.Error(t, err)
	}
	a := &Account{Credentials: map[string]any{"speech_profiles": map[string]any{"source-A/doubao-seed-tts-2.0": speechProfileV1}, "upstream_protocols": map[string]any{"source-A/doubao-seed-tts-2.0": string(speechProtocolVolcengineTTS)}}}
	require.Equal(t, speechProfileV1, speechAccountProfile(a, "source-A/doubao-seed-tts-2.0"))
	require.Empty(t, speechAccountProfile(a, "source-B/doubao-seed-tts-2.0"))
	require.Empty(t, speechAccountProfile(a, "doubao-seed-tts-2.0"))
}

// These are the real native HTTP forwarding boundary, with a constructed Proxy
// upstream. Proxy's own tests own the subsequent xAI wire conversion.
func TestXAISpeechFullMappedProfileAndNativeBillingIdentity(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformGrok} {
		for _, clientModel := range []string{"customer-speech", "source-A/native-tts-alias"} {
			for _, perRequest := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/per-request=%t", platform, clientModel, perRequest), func(t *testing.T) {
					var wires atomic.Int32
					const final = "source-A/native-tts-alias"
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						wires.Add(1)
						require.Equal(t, "/v1/audio/speech", r.URL.Path)
						var payload map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
						require.Equal(t, final, payload["model"])
						require.Equal(t, "eve", payload["voice"])
						require.Equal(t, "你好！", payload["input"])
						require.Equal(t, 1.2, payload["speed"])
						w.Header().Set("Content-Type", "audio/wav")
						_, _ = io.WriteString(w, "RIFF-fixture-audio")
					}))
					defer srv.Close()
					gid := int64(41)
					group := &Group{ID: gid, Platform: platform, RateMultiplier: 1, AudioTTSPricePerMillionChars: speechPricePointer(30)}
					channel := &ChannelService{groupRepo: speechPriceGroupRepo{group: group}}
					rows := []ChannelModelPricing{}
					if perRequest {
						rows = append(rows, ChannelModelPricing{Models: []string{"tts"}, Platform: platform, BillingMode: BillingModePerRequest, PerRequestPrice: speechPricePointer(.125)})
					}
					channel.cache.Store(populateChannelCache([]Channel{{ID: 1, Status: StatusActive, GroupIDs: []int64{gid}, ModelPricing: rows}}, map[int64]string{gid: platform}))
					svc := &OpenAIGatewayService{cfg: &config.Config{}, channelService: channel, resolver: NewModelPricingResolver(channel, &BillingService{}), httpUpstream: speechHTTPUpstreamFixture{client: srv.Client(), target: srv.URL, t: t}}
					key := &APIKey{ID: 5, GroupID: &gid, Group: group, User: &User{ID: 8}}
					account := &Account{ID: 9, GroupIDs: []int64{gid}, Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture", "base_url": "https://proxy.fixture.invalid/v1", "model_mapping": map[string]any{"customer-speech": final, final: final}, "speech_profiles": map[string]any{final: speechProfileXAI}, "upstream_protocols": map[string]any{final: string(speechProtocolXAI)}}}
					require.Equal(t, speechProfileXAI, speechAccountProfile(account, final))
					require.Empty(t, speechAccountProfile(account, "native-tts-alias"))
					body, _ := json.Marshal(map[string]any{"model": clientModel, "input": "你好！", "voice": "eve", "response_format": "wav", "speed": 1.2})
					parsed, err := ParseOpenAISpeechRequest(body)
					require.NoError(t, err)
					rec := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(rec)
					ctx.Request = httptest.NewRequest("POST", "/v1/audio/speech", nil)
					result, billingKey, err := svc.ForwardOpenAISpeech(context.Background(), ctx, key, account, parsed, clientModel)
					require.NoError(t, err)
					require.EqualValues(t, 1, wires.Load())
					require.Equal(t, 200, rec.Code)
					require.Equal(t, "audio/wav", rec.Header().Get("Content-Type"))
					require.Empty(t, rec.Header().Get(speechPublicCountHeader))
					require.Empty(t, rec.Header().Get(speechPrivateCountHeader))
					require.Equal(t, "tts", result.Model)
					require.Equal(t, "tts", result.BillingModel)
					require.Equal(t, "tts", result.UpstreamModel)
					require.Zero(t, result.Usage.InputTokens)
					require.Zero(t, result.Usage.OutputTokens)
					require.Equal(t, estimateGrokVoiceAudioUsage("tts", body, "application/json", nil, 0), result.AudioUsage)
					require.InDelta(t, 3.0/1e6, result.AudioUsage.DurationOrUnits, 1e-15)
					require.Equal(t, key.ID, billingKey.ID)
					require.Equal(t, key.GroupID, billingKey.GroupID)
					require.Equal(t, gid, billingKey.Group.ID)
					require.Equal(t, platform, key.Group.Platform, "original Key group cannot be switched")
					if perRequest {
						resolved := svc.resolveOpenAIChannelPricing(context.Background(), result.BillingModel, billingKey)
						require.NotNil(t, resolved)
						require.Equal(t, BillingModePerRequest, resolved.Mode)
						require.Equal(t, .125, resolved.DefaultPerRequestPrice, "existing tts per-request row retains priority")
						cost, err := (&BillingService{}).CalculateCostUnified(CostInput{Ctx: context.Background(), Model: result.BillingModel, GroupID: &gid, Group: group, RequestCount: 1, RateMultiplier: .5, Resolver: svc.resolver, Resolved: resolved})
						require.NoError(t, err)
						require.InDelta(t, .0625, cost.ActualCost, 1e-12)
					} else {
						cost := (&BillingService{}).CalculateAudioCost("tts", result.AudioUsage.DurationOrUnits, groupAudioPriceConfigFromAPIKey(billingKey), .5)
						require.InDelta(t, 3.0/1e6*30*.5, cost.ActualCost, 1e-12)
					}
				})
			}
		}
	}
}

func TestXAISpeechMissingAuthorityOrUnsupportedSubsetNeverExecutes(t *testing.T) {
	for _, name := range []string{"same-name-no-profile", "no-upstream-protocol", "mismatched-profile", "foreign-group", "invalid-voice", "speed", "format", "instructions", "stream"} {
		t.Run(name, func(t *testing.T) {
			var wires atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { wires.Add(1) }))
			defer srv.Close()
			gid := int64(41)
			group := &Group{ID: gid, Platform: PlatformOpenAI, AudioTTSPricePerMillionChars: speechPricePointer(30)}
			ch := &ChannelService{groupRepo: speechPriceGroupRepo{group: group}}
			ch.cache.Store(populateChannelCache(nil, map[int64]string{gid: PlatformOpenAI}))
			svc := &OpenAIGatewayService{cfg: &config.Config{}, channelService: ch, resolver: NewModelPricingResolver(ch, &BillingService{}), httpUpstream: speechHTTPUpstreamFixture{client: srv.Client(), target: srv.URL, t: t}}
			const final = "source-A/native-tts-alias"
			account := &Account{ID: 9, GroupIDs: []int64{gid}, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture", "base_url": "https://proxy.fixture.invalid/v1", "model_mapping": map[string]any{"customer-speech": final}, "speech_profiles": map[string]any{final: speechProfileXAI}, "upstream_protocols": map[string]any{final: string(speechProtocolXAI)}}}
			fields := map[string]any{"model": "customer-speech", "input": "hello", "voice": "eve"}
			switch name {
			case "same-name-no-profile":
				delete(account.Credentials, "speech_profiles")
			case "no-upstream-protocol":
				delete(account.Credentials, "upstream_protocols")
			case "mismatched-profile":
				account.Credentials["speech_profiles"] = map[string]any{final: speechProfileV1}
			case "foreign-group":
				account.GroupIDs = []int64{99}
			case "invalid-voice":
				fields["voice"] = "../eve"
			case "speed":
				fields["speed"] = 1.6
			case "format":
				fields["response_format"] = "flac"
			case "instructions":
				fields["instructions"] = "read faster"
			case "stream":
				fields["stream_format"] = "sse"
			}
			body, _ := json.Marshal(fields)
			parsed, err := ParseOpenAISpeechRequest(body)
			if err == nil {
				rec := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(rec)
				ctx.Request = httptest.NewRequest("POST", "/v1/audio/speech", nil)
				_, _, err = svc.ForwardOpenAISpeech(context.Background(), ctx, &APIKey{ID: 5, GroupID: &gid, Group: group}, account, parsed, parsed.Model)
			}
			require.Error(t, err)
			require.Zero(t, wires.Load(), "invalid selection/params must reject before provider")
		})
	}
}

type ttsProfileAccountRepo struct {
	AccountRepository
	accounts []Account
	err      error
}

func (r *ttsProfileAccountRepo) ListSchedulableByPlatform(context.Context, string) ([]Account, error) {
	return r.accounts, r.err
}
func (r *ttsProfileAccountRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]Account, error) {
	return r.accounts, r.err
}
func TestModelLessTTSUsesAuthorizedPublicAliasAndExactProfile(t *testing.T) {
	gid := int64(41)
	makeAccount := func(id int64, alias string) *Account {
		final := "source-A/native-tts-alias"
		return &Account{ID: id, GroupIDs: []int64{gid}, Platform: PlatformGrok, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"model_mapping": map[string]any{alias: final}, "speech_profiles": map[string]any{final: speechProfileXAI}, "upstream_protocols": map[string]any{final: string(speechProtocolXAI)}}}
	}
	for _, name := range []string{"public-alias", "same-profile-two-credentials", "text-only-permission", "foreign-group", "ambiguous-alias", "no-profile", "wrong-protocol", "read-fault"} {
		t.Run(name, func(t *testing.T) {
			a := makeAccount(1, "customer-speech")
			accounts := []Account{*a}
			models := []string{"customer-speech"}
			var fail error
			switch name {
			case "same-profile-two-credentials":
				accounts = append(accounts, *makeAccount(2, "customer-speech"))
			case "text-only-permission":
				models = []string{"grok-4.5"}
			case "foreign-group":
				accounts[0].GroupIDs = []int64{99}
			case "ambiguous-alias":
				accounts = append(accounts, *makeAccount(2, "other-speech"))
				models = append(models, "other-speech")
			case "no-profile":
				delete(accounts[0].Credentials, "speech_profiles")
			case "wrong-protocol":
				accounts[0].Credentials["upstream_protocols"] = map[string]any{}
			case "read-fault":
				fail = fmt.Errorf("authoritative account read unavailable")
			}
			repo := &ttsProfileAccountRepo{accounts: accounts, err: fail}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: repo}
			key := &APIKey{GroupID: &gid, Group: &Group{ID: gid, Platform: PlatformGrok, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: models}}}
			model, excluded, err := svc.ResolveGrokTTSSelectionModel(context.Background(), key)
			if name == "public-alias" || name == "same-profile-two-credentials" {
				require.NoError(t, err)
				require.Equal(t, "customer-speech", model)
				require.Empty(t, excluded)
			} else {
				require.Error(t, err)
				require.Empty(t, model)
			}
		})
	}
}

// The real scheduler candidate shape omits protocol/profile fields; its full
// account payload retains them. This reproduces the observed P3 boundary.
type ttsProfileSnapshotCache struct {
	SchedulerCache
	metadata []*Account
	full     *Account
	err      error
	reads    int
}

func (c *ttsProfileSnapshotCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	return c.metadata, true, nil
}
func (c *ttsProfileSnapshotCache) GetAccount(context.Context, int64) (*Account, error) {
	c.reads++
	return c.full, c.err
}
func (r *ttsProfileAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	if r.err != nil {
		return nil, r.err
	}
	return nil, ErrNoAvailableAccounts
}
func TestModelLessTTSHydratesSchedulerMetadataBeforeProfileAdmission(t *testing.T) {
	for _, platform := range []string{PlatformGrok, PlatformOpenAI} {
		for _, fault := range []string{"valid", "read-failure", "missing-profile", "foreign-group", "disabled"} {
			t.Run(platform+"/"+fault, func(t *testing.T) {
				gid := int64(18)
				metadata := &Account{ID: 15, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{gid}, Credentials: map[string]any{"model_mapping": map[string]any{"customer-speech": "source-A/speech"}}}
				full := *metadata
				full.Credentials = map[string]any{"model_mapping": map[string]any{"customer-speech": "source-A/speech"}, "speech_profiles": map[string]any{"source-A/speech": speechProfileXAI}, "upstream_protocols": map[string]any{"source-A/speech": string(speechProtocolXAI)}}
				cache := &ttsProfileSnapshotCache{metadata: []*Account{metadata}, full: &full}
				repo := &ttsProfileAccountRepo{}
				switch fault {
				case "read-failure":
					cache.err = fmt.Errorf("synthetic full-account outage")
					repo.err = cache.err
				case "missing-profile":
					delete(full.Credentials, "speech_profiles")
				case "foreign-group":
					full.GroupIDs = []int64{99}
				case "disabled":
					full.Status = "inactive"
					full.Schedulable = false
				}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: repo, schedulerSnapshot: NewSchedulerSnapshotService(cache, nil, repo, nil, nil)}
				key := &APIKey{ID: 13, GroupID: &gid, Group: &Group{ID: gid, Platform: platform, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"customer-speech"}}}}
				model, excluded, err := svc.ResolveGrokTTSSelectionModel(context.Background(), key)
				require.Greater(t, cache.reads, 0)
				require.NotContains(t, metadata.Credentials, "speech_profiles", "candidate cache must not be mutated or promoted into a new authority")
				if fault == "valid" {
					require.NoError(t, err)
					require.Equal(t, "customer-speech", model)
					require.NotContains(t, excluded, int64(15))
				} else {
					require.Error(t, err)
					require.Contains(t, excluded, int64(15))
				}
			})
		}
	}
}

type ttsPinnedProxyUpstream struct{ speechHTTPUpstreamFixture }

func (u ttsPinnedProxyUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	u.t.Helper()
	require.Equal(u.t, "proxy.fixture.invalid", req.URL.Hostname())
	require.Contains(u.t, []string{"/v1/audio/speech", "/v1/tts"}, req.URL.Path)
	var pins []string
	for name, values := range req.Header {
		if strings.EqualFold(name, "X-CLIProxy-Auth-ID") {
			pins = append(pins, values...)
		}
	}
	require.Equal(u.t, []string{"source-A-pin"}, pins)
	return u.speechHTTPUpstreamFixture.Do(req, proxy, id, concurrency)
}
func TestSameOpenAIKeyUsesSpeechAndModelLessTTSThroughPinnedProxy(t *testing.T) {
	for _, fault := range []string{"valid", "no-grant", "no-profile", "no-pin", "official-host", "foreign-group", "full-read-failure"} {
		t.Run(fault, func(t *testing.T) {
			var wires atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wires.Add(1)
				var fields map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&fields))
				if r.URL.Path == "/v1/tts" {
					require.NotContains(t, fields, "model")
					require.Equal(t, "你好！", fields["text"])
					require.Equal(t, "eve", fields["voice_id"])
					require.Equal(t, false, fields["text_normalization"])
					require.Equal(t, "alaw", fields["output_format"].(map[string]any)["codec"])
				} else {
					require.Equal(t, "source-A/speech", fields["model"])
				}
				w.Header().Set("Content-Type", "audio/mpeg")
				_, _ = io.WriteString(w, "ID3-synthetic-audio")
			}))
			defer srv.Close()
			gid := int64(18)
			group := &Group{ID: gid, Platform: PlatformOpenAI, AudioTTSPricePerMillionChars: speechPricePointer(30), ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"customer-speech"}}}
			account := Account{ID: 15, GroupIDs: []int64{gid}, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "fixture", "base_url": "https://proxy.fixture.invalid/v1", "model_mapping": map[string]any{"customer-speech": "source-A/speech"}, "speech_profiles": map[string]any{"source-A/speech": speechProfileXAI}, "upstream_protocols": map[string]any{"source-A/speech": string(speechProtocolXAI)}, "header_override_enabled": true, "header_overrides": map[string]any{"X-CLIProxy-Auth-ID": "source-A-pin"}}}
			repo := &ttsProfileAccountRepo{accounts: []Account{account}}
			channel := &ChannelService{groupRepo: speechPriceGroupRepo{group: group}}
			channel.cache.Store(populateChannelCache(nil, map[int64]string{gid: PlatformOpenAI}))
			svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: repo, channelService: channel, resolver: NewModelPricingResolver(channel, &BillingService{}), httpUpstream: ttsPinnedProxyUpstream{speechHTTPUpstreamFixture{client: srv.Client(), target: srv.URL, t: t}}}
			key := &APIKey{ID: 13, GroupID: &gid, Group: group, User: &User{ID: 8}}
			switch fault {
			case "no-grant":
				group.ModelAllowlist.Models = []string{"grok-4.5"}
			case "no-profile":
				delete(account.Credentials, "speech_profiles")
			case "no-pin":
				delete(account.Credentials, "header_override_enabled")
			case "official-host":
				account.Credentials["base_url"] = "https://api.x.ai/v1"
			case "foreign-group":
				account.GroupIDs = []int64{99}
				repo.accounts = []Account{account}
			case "full-read-failure":
				cache := &ttsProfileSnapshotCache{metadata: []*Account{&account}, err: fmt.Errorf("synthetic full-account outage")}
				repo.err = cache.err
				svc.schedulerSnapshot = NewSchedulerSnapshotService(cache, nil, repo, nil, nil)
			}
			if fault == "valid" {
				body := []byte(`{"model":"customer-speech","input":"你好！","voice":"eve","response_format":"mp3"}`)
				parsed, err := ParseOpenAISpeechRequest(body)
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(rec)
				ctx.Request = httptest.NewRequest("POST", "/v1/audio/speech", nil)
				result, billingKey, err := svc.ForwardOpenAISpeech(context.Background(), ctx, key, &account, parsed, "customer-speech")
				require.NoError(t, err)
				require.Equal(t, "tts", result.BillingModel)
				require.Equal(t, key.ID, billingKey.ID)
			}
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest("POST", "/v1/tts", nil)
			ctx.Request.Header.Set("X-CLIProxy-Auth-ID", "forged-B")
			result, qualified, err := svc.ForwardGrokTTSForKey(context.Background(), ctx, key, &account, "customer-speech", []byte(`{"text":"你好！","language":"auto","voice_id":"eve","text_normalization":false,"output_format":{"codec":"alaw","sample_rate":8000}}`), "application/json")
			if fault == "valid" {
				require.NoError(t, err)
				require.EqualValues(t, 2, wires.Load())
				require.Equal(t, account.ID, qualified.ID)
				require.Equal(t, "tts", result.Model)
				require.Equal(t, gid, *key.GroupID)
				require.Equal(t, PlatformOpenAI, key.Group.Platform)
			} else {
				require.Error(t, err)
				require.Zero(t, wires.Load())
			}
		})
	}
}

// The current candidate set, not absence from a negative set, authorizes an ID.
func TestModelLessTTSPositiveCurrentAccountAndFreshWireIdentity(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformGrok} {
		for _, scenario := range []string{"stale-B-not-current", "fresh-wire", "removed-group", "disabled", "removed-profile", "full-read-failure"} {
			t.Run(platform+"/"+scenario, func(t *testing.T) {
				var wires atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					wires.Add(1)
					require.Equal(t, "/v1/tts", r.URL.Path)
					require.Equal(t, "Bearer fresh-proxy-key", r.Header.Get("Authorization"))
					require.Equal(t, "source-A-pin", r.Header.Get("X-CLIProxy-Auth-ID"))
					data, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Equal(t, `{"text":"hello","language":"auto","voice_id":"eve","output_format":{"codec":"alaw","sample_rate":8000}}`, string(data))
					w.Header().Set("Content-Type", "audio/mpeg")
					_, _ = io.WriteString(w, "ID3-synthetic-audio")
				}))
				defer srv.Close()
				gid := int64(18)
				group := &Group{ID: gid, Platform: platform, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"customer-speech"}}}
				key := &APIKey{ID: 13, GroupID: &gid, Group: group, User: &User{ID: 8}}
				// Old B has the same formerly allowed alias, Group and profile but is no
				// longer in today's candidate list; only A is currently present.
				old := Account{ID: 16, GroupIDs: []int64{gid}, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "stale-proxy-key", "base_url": "https://stale.fixture.invalid/v1", "model_mapping": map[string]any{"customer-speech": "source-A/speech"}, "speech_profiles": map[string]any{"source-A/speech": speechProfileXAI}, "upstream_protocols": map[string]any{"source-A/speech": string(speechProtocolXAI)}, "header_override_enabled": true, "header_overrides": map[string]any{"X-CLIProxy-Auth-ID": "stale-B-pin"}}}
				current := old
				current.ID = 15
				current.Credentials = map[string]any{"api_key": "fresh-proxy-key", "base_url": "https://proxy.fixture.invalid/v1", "model_mapping": map[string]any{"customer-speech": "source-A/speech"}, "speech_profiles": map[string]any{"source-A/speech": speechProfileXAI}, "upstream_protocols": map[string]any{"source-A/speech": string(speechProtocolXAI)}, "header_override_enabled": true, "header_overrides": map[string]any{"X-CLIProxy-Auth-ID": "source-A-pin"}}
				metadata := current
				metadata.Credentials = map[string]any{"model_mapping": map[string]any{"customer-speech": "source-A/speech"}}
				repo := &ttsProfileAccountRepo{accounts: []Account{current}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: repo, httpUpstream: ttsPinnedProxyUpstream{speechHTTPUpstreamFixture{client: srv.Client(), target: srv.URL, t: t}}}
				if scenario != "stale-B-not-current" {
					old.ID = current.ID
					cache := &ttsProfileSnapshotCache{metadata: []*Account{&metadata}, full: &current}
					switch scenario {
					case "removed-group":
						current.GroupIDs = []int64{99}
					case "disabled":
						current.Status = "inactive"
						current.Schedulable = false
					case "removed-profile":
						delete(current.Credentials, "speech_profiles")
					case "full-read-failure":
						cache.err = fmt.Errorf("synthetic full-account outage")
						repo.err = cache.err
					}
					svc.schedulerSnapshot = NewSchedulerSnapshotService(cache, nil, repo, nil, nil)
				}
				rec := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(rec)
				ctx.Request = httptest.NewRequest("POST", "/v1/tts", nil)
				ctx.Request.Header.Set("X-CLIProxy-Auth-ID", "client-forged")
				result, qualified, err := svc.ForwardGrokTTSForKey(context.Background(), ctx, key, &old, "customer-speech", []byte(`{"text":"hello","language":"auto","voice_id":"eve","output_format":{"codec":"alaw","sample_rate":8000}}`), "application/json")
				if scenario == "fresh-wire" {
					require.NoError(t, err)
					require.EqualValues(t, 1, wires.Load())
					require.Same(t, &current, qualified)
					require.Equal(t, "tts", result.Model)
					require.Equal(t, "https://stale.fixture.invalid/v1", old.Credentials["base_url"], "old selected object must not be silently repaired or reused")
				} else {
					require.Error(t, err)
					require.Nil(t, qualified)
					require.Zero(t, wires.Load())
				}
				require.Equal(t, platform, key.Group.Platform)
				require.Equal(t, gid, *key.GroupID)
			})
		}
	}
}
