package repository

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func voiceWireRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	ctx := service.WithHTTPUpstreamRedirectsDisabled(service.WithHTTPUpstreamVoiceSingleAttempt(context.Background()))
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	return req
}

func TestVoiceSingleAttemptCommittedDisconnectWireCount(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		for _, body := range []string{"", `{"name":"wire"}`} {
			t.Run(method+"/"+body, func(t *testing.T) {
				var wires atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					wires.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
				}))
				defer srv.Close()
				upstream := NewHTTPUpstream(nil).(*httpUpstreamService)
				_, err := upstream.Do(voiceWireRequest(t, method, srv.URL, body), "", 1, 1)
				require.Error(t, err)
				require.EqualValues(t, 1, wires.Load())
				requireNoUpstreamInFlight(t, upstream)
				t.Logf("actual_wire_count method=%s body_bytes=%d committed_disconnect=%d", method, len(body), wires.Load())
			})
		}
	}
}

func TestVoiceSingleAttemptRedirectWireCount(t *testing.T) {
	for _, code := range []int{301, 302, 307, 308} {
		for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
			t.Run(method+http.StatusText(code), func(t *testing.T) {
				var primary, target atomic.Int32
				destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { target.Add(1); w.WriteHeader(204) }))
				defer destination.Close()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					primary.Add(1)
					w.Header().Set("Location", destination.URL)
					w.WriteHeader(code)
				}))
				defer srv.Close()
				upstream := NewHTTPUpstream(nil).(*httpUpstreamService)
				resp, err := upstream.Do(voiceWireRequest(t, method, srv.URL, ""), "", 1, 1)
				require.NoError(t, err)
				require.Equal(t, code, resp.StatusCode)
				require.Equal(t, destination.URL, resp.Header.Get("Location"))
				require.NoError(t, resp.Body.Close())
				require.EqualValues(t, 1, primary.Load())
				require.Zero(t, target.Load())
				requireNoUpstreamInFlight(t, upstream)
				t.Logf("actual_wire_count redirect=%d method=%s primary=%d target=%d", code, method, primary.Load(), target.Load())
			})
		}
	}
}

func TestVoiceSingleAttemptHeaderPresenceAndCustomTransportFailClosed(t *testing.T) {
	var wires atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { wires.Add(1) }))
	defer srv.Close()
	for _, key := range []string{"Idempotency-Key", "idempotency-key", "X-Idempotency-Key", "x-IDEMPOTENCY-key"} {
		for _, value := range []string{"", "opaque"} {
			req := voiceWireRequest(t, http.MethodPost, srv.URL, "")
			req.Header[key] = []string{value}
			upstream := NewHTTPUpstream(nil).(*httpUpstreamService)
			_, err := upstream.Do(req, "", 1, 1)
			require.Error(t, err)
			requireNoUpstreamInFlight(t, upstream)
		}
	}
	var customCalls atomic.Int32
	_, _, err := voiceSingleAttemptHTTPClient(&http.Client{Transport: voiceUnknownRoundTripper{&customCalls}}, voiceWireRequest(t, http.MethodDelete, srv.URL, ""))
	require.Error(t, err)
	require.Zero(t, customCalls.Load())
	require.Zero(t, wires.Load())
}

type voiceUnknownRoundTripper struct{ calls *atomic.Int32 }

func (r voiceUnknownRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return nil, errors.New("must not be called")
}

func TestVoiceSingleAttemptHTTP1TLSCloneAndBodyRelease(t *testing.T) {
	var protocol atomic.Int32
	closed := make(chan struct{}, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol.Store(int32(r.ProtoMajor))
		_, _ = w.Write([]byte("audio"))
	}))
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	srv.StartTLS()
	defer srv.Close()
	upstream, call := lifecycleClient(t, srv, false)
	resp, err := call(voiceWireRequest(t, http.MethodPost, srv.URL, "{}"))
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.EqualValues(t, 1, protocol.Load(), "H2/REFUSED_STREAM cannot negotiate on unsafe Voice")
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("owned cloned transport idle connection not closed")
	}
	requireNoUpstreamInFlight(t, upstream)
	original := srv.Client()
	original.Timeout = 7 * time.Second
	derived, closeIdle, err := voiceSingleAttemptHTTPClient(original, voiceWireRequest(t, http.MethodPatch, srv.URL, "{}"))
	require.NoError(t, err)
	defer closeIdle()
	require.Equal(t, original.Timeout, derived.Timeout)
	native := original.Transport.(*http.Transport)
	clone := derived.Transport.(*http.Transport)
	require.NotSame(t, native, clone)
	require.NotSame(t, native.TLSClientConfig, clone.TLSClientConfig)
	require.Equal(t, native.Proxy == nil, clone.Proxy == nil)
	require.Equal(t, native.DialContext == nil, clone.DialContext == nil)
	require.Equal(t, native.TLSHandshakeTimeout, clone.TLSHandshakeTimeout)
	require.Equal(t, []string{"http/1.1"}, clone.TLSClientConfig.NextProtos)
	require.True(t, clone.Protocols.HTTP1())
	require.False(t, clone.Protocols.HTTP2())
	require.False(t, clone.Protocols.UnencryptedHTTP2())
}

func TestVoiceSingleAttemptCLIFallbackContextGuard(t *testing.T) {
	var wires atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wires.Add(1)
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"Access denied"}`))
	}))
	defer srv.Close()
	target := srv.Listener.Addr().String()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	defer transport.CloseIdleConnections()
	req := voiceWireRequest(t, http.MethodPost, "http://"+grokCLIProxyHost+"/v1/tts", "{}")
	req.Header.Set("Authorization", "Bearer fixture")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	require.NotNil(t, req.GetBody)
	client := httpClientWithGrokAccessDeniedFallback(&http.Client{Transport: transport})
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 403, resp.StatusCode)
	require.EqualValues(t, 1, wires.Load(), "matching CLI compatibility403 must not replay against official host")
}

func TestVoiceSingleAttemptConfiguredProxyKeepsRouting(t *testing.T) {
	var targetCalls, proxyCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyCalls.Add(1); w.WriteHeader(204) }))
	defer proxy.Close()
	upstream := NewHTTPUpstream(nil).(*httpUpstreamService)
	resp, err := upstream.Do(voiceWireRequest(t, http.MethodPost, target.URL, `{}`), proxy.URL, 1, 1)
	require.NoError(t, err)
	require.Equal(t, 204, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.EqualValues(t, 1, proxyCalls.Load())
	require.Zero(t, targetCalls.Load())
	requireNoUpstreamInFlight(t, upstream)
}
