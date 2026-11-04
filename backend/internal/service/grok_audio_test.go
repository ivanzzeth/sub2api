package service

import (
	"context"
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
			account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-proxy-client", "base_url": "https://proxy.fixture.invalid/v1", "model_mapping": map[string]any{"doubao-seed-tts-2.0": "source-A/doubao-seed-tts-2.0"}, "speech_profiles": map[string]any{"source-A/doubao-seed-tts-2.0": speechProfileV1}, "upstream_protocols": map[string]any{"source-A/doubao-seed-tts-2.0": string(speechProtocolVolcengineTTS)}}}
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
