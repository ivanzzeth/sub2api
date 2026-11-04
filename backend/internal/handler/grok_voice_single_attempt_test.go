//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type voiceAttemptAccountRepo struct {
	openAIImagesFailoverAccountRepo
	cooldowns atomic.Int32
}

func (r *voiceAttemptAccountRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.cooldowns.Add(1)
	return nil
}
func (r *voiceAttemptAccountRepo) SetRateLimited(context.Context, int64, time.Time) error {
	r.cooldowns.Add(1)
	return nil
}
func (r *voiceAttemptAccountRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	return nil
}

func voiceWireHandler(t *testing.T, srv *httptest.Server) (*OpenAIGatewayHandler, *grokMediaSlotsCache, *voiceAttemptAccountRepo) {
	t.Helper()
	h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false)
	accounts := []service.Account{}
	for i := 1; i <= 3; i++ {
		accounts = append(accounts, service.Account{ID: int64(i), Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 10, Priority: i, GroupIDs: []int64{24}, Credentials: map[string]any{"api_key": "fixture"}})
	}
	repo := &voiceAttemptAccountRepo{openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{accounts: accounts}}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	upstream.call = func(req *http.Request, id int64) (*http.Response, error) {
		target, err := url.Parse(srv.URL)
		if err != nil {
			return nil, err
		}
		target.Path = req.URL.Path
		copy := req.Clone(req.Context())
		copy.URL = target
		copy.Host = ""
		copy.Header.Set("X-Test-Account", strconv.FormatInt(id, 10))
		return client.Do(copy)
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	concurrency := service.NewConcurrencyService(slots)
	h.gatewayService = service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, concurrency, nil, nil, nil, upstream, nil, nil, service.NewGrokTokenProvider(repo, nil), nil, nil, nil, nil, nil)
	return h, slots, repo
}

func TestGrokVoiceUnsafeCommittedDisconnectNeverSwitchesAccount(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
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
			h, slots, _ := voiceWireHandler(t, srv)
			c, rec := grokMediaSlotContext(context.Background(), true)
			c.Request.Method = method
			c.Request.Body = io.NopCloser(strings.NewReader(`{"input":"test"}`))
			h.GrokVoice(c, "tts")
			require.EqualValues(t, 1, wires.Load())
			require.Equal(t, http.StatusBadGateway, rec.Code)
			require.Equal(t, slots.acquired, slots.released)
			require.False(t, slots.duplicateRelease)
			t.Logf("handler_actual_wire_count method=%s committed_disconnect=%d slots=%d/%d", method, wires.Load(), slots.acquired, slots.released)
		})
	}
}

func TestGrokVoiceUnsafe503PreservesCooldownWithoutNextAccount(t *testing.T) {
	var wires atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wires.Add(1)
		w.Header().Set("Retry-After", "9")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"temporary"}}`))
	}))
	defer srv.Close()
	h, slots, repo := voiceWireHandler(t, srv)
	c, rec := grokMediaSlotContext(context.Background(), true)
	h.GrokVoice(c, "tts")
	require.EqualValues(t, 1, wires.Load())
	require.Greater(t, repo.cooldowns.Load(), int32(0))
	require.GreaterOrEqual(t, rec.Code, 400)
	require.Equal(t, slots.acquired, slots.released)
}

func TestGrokVoiceGETRetainsAccountFailover(t *testing.T) {
	var wires atomic.Int32
	var accounts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accounts = append(accounts, r.Header.Get("X-Test-Account"))
		if wires.Add(1) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"temporary"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"voices":[]}`))
	}))
	defer srv.Close()
	h, slots, _ := voiceWireHandler(t, srv)
	c, rec := grokMediaSlotContext(context.Background(), false)
	h.GrokVoice(c, "custom-voices")
	require.EqualValues(t, 2, wires.Load())
	require.Len(t, accounts, 2)
	require.NotEqual(t, accounts[0], accounts[1])
	require.Equal(t, 200, rec.Code)
	require.Equal(t, slots.acquired, slots.released)
}

func TestGrokVoiceWrongGroupRejectedBeforeWire(t *testing.T) {
	var wires atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { wires.Add(1) }))
	defer srv.Close()
	h, _, _ := voiceWireHandler(t, srv)
	c, rec := grokMediaSlotContext(context.Background(), true)
	key, _ := middleware2.GetAPIKeyFromContext(c)
	key.Group.Platform = service.PlatformOpenAI
	h.GrokVoice(c, "tts")
	require.Equal(t, 404, rec.Code)
	require.Zero(t, wires.Load())
}

func TestStandardSpeechValidationDoesNotExecuteAnyProvider(t *testing.T) {
	for _, extra := range []string{`,"speed":"1"`, `,"speed":null`, `,"voice":"alloy"`, `,"instructions":"read faster"`, `,"url":"https://example.invalid"`} {
		h, _, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
		var wires atomic.Int32
		upstream.call = func(*http.Request, int64) (*http.Response, error) {
			wires.Add(1)
			return nil, fmt.Errorf("must not reach provider")
		}
		c, rec := grokMediaSlotContext(context.Background(), true)
		key, ok := middleware2.GetAPIKeyFromContext(c)
		if !ok {
			t.Fatal("missing test key")
		}
		key.Group.Platform = service.PlatformOpenAI
		c.Request = httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"doubao-seed-tts-2.0","input":"hello"`+extra+`}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.AudioSpeech(c)
		if rec.Code != 400 || wires.Load() != 0 {
			t.Fatalf("status=%d supplier=%d", rec.Code, wires.Load())
		}
	}
}
