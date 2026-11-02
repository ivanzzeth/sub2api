package service

import (
	"context"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
