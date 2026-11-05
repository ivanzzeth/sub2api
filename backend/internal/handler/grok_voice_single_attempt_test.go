//go:build unit

package handler

import (
	"context"
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
		accounts = append(accounts, service.Account{ID: int64(i), Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 10, Priority: i, GroupIDs: []int64{24}, Credentials: map[string]any{"api_key": "fixture", "model_mapping": map[string]any{"grok-4.5": "grok-4.5", "grok-tts": "grok-tts"}, "speech_profiles": map[string]any{"grok-tts": "xai-grok-tts-v1"}, "upstream_protocols": map[string]any{"grok-tts": "xai-voice-http"}}})
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
	key.Group.Platform = service.PlatformAnthropic
	h.GrokVoice(c, "tts")
	require.Equal(t, 404, rec.Code)
	require.Zero(t, wires.Load())
}

func TestStandardSpeechValidationDoesNotExecuteAnyProvider(t *testing.T) {
	for _, extra := range []string{`,"speed":"1"`, `,"speed":null`, `,"voice":"alloy"`, `,"instructions":"read faster"`, `,"url":"https://example.invalid"`} {
		t.Run(extra, func(t *testing.T) {
			var wires atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				wires.Add(1)
				t.Error("unsupported Ark parameter reached provider")
			}))
			defer srv.Close()
			h, _, repo := voiceWireHandler(t, srv)
			for i := range repo.accounts {
				repo.accounts[i].Platform = service.PlatformOpenAI
				repo.accounts[i].Credentials = map[string]any{"api_key": "fixture", "model_mapping": map[string]any{"doubao-seed-tts-2.0": "source-A/doubao-seed-tts-2.0"}, "speech_profiles": map[string]any{"source-A/doubao-seed-tts-2.0": "ark-agent-plan-tts-2-v1"}, "upstream_protocols": map[string]any{"source-A/doubao-seed-tts-2.0": "volcengine-tts-v3-http"}}
			}
			c, rec := grokMediaSlotContext(context.Background(), true)
			key, ok := middleware2.GetAPIKeyFromContext(c)
			require.True(t, ok)
			key.Group.Platform = service.PlatformOpenAI
			c.Request = httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"doubao-seed-tts-2.0","input":"hello"`+extra+`}`))
			c.Request.Header.Set("Content-Type", "application/json")
			h.AudioSpeech(c)
			require.Equal(t, 400, rec.Code)
			require.Zero(t, wires.Load())
		})
	}
}

func TestModelLessTTSPublicAliasAndNonTTSLegacySelection(t *testing.T) {
	for _, name := range []string{"authorized-alias", "text-only-model-grant", "non-tts-only-account"} {
		t.Run(name, func(t *testing.T) {
			var wires atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wires.Add(1)
				w.WriteHeader(503)
				_, _ = io.WriteString(w, `{"error":{"message":"synthetic terminal provider failure"}}`)
			}))
			defer srv.Close()
			h, _, repo := voiceWireHandler(t, srv)
			for i := range repo.accounts {
				repo.accounts[i].Credentials["model_mapping"] = map[string]any{"customer-speech": "grok-tts"}
			}
			c, rec := grokMediaSlotContext(context.Background(), true)
			key, ok := middleware2.GetAPIKeyFromContext(c)
			require.True(t, ok)
			key.Group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"customer-speech"}}
			c.Request = httptest.NewRequest("POST", "/v1/tts", strings.NewReader(`{"text":"hello","language":"auto","voice_id":"eve"}`))
			endpoint := "tts"
			if name == "text-only-model-grant" {
				key.Group.ModelAllowlist.Models = []string{"grok-4.5"}
			}
			if name == "non-tts-only-account" {
				endpoint = "stt"
				c.Request = httptest.NewRequest("POST", "/v1/stt", strings.NewReader("synthetic"))
			}
			h.GrokVoice(c, endpoint)
			if name == "authorized-alias" {
				require.EqualValues(t, 1, wires.Load())
				require.GreaterOrEqual(t, rec.Code, 400)
			} else {
				require.Zero(t, wires.Load())
				require.GreaterOrEqual(t, rec.Code, 400)
			}
		})
	}
}

func TestNonTTSVoiceKeepsOriginalChatSelectionAndExcludesTTSOnlyAccount(t *testing.T) {
	for _, endpoint := range []string{"stt", "custom-voices"} {
		t.Run(endpoint, func(t *testing.T) {
			var wires atomic.Int32
			var selected string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wires.Add(1)
				selected = r.Header.Get("X-Test-Account")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"text":"synthetic","voices":[]}`)
			}))
			defer srv.Close()
			h, slots, repo := voiceWireHandler(t, srv)
			repo.accounts = repo.accounts[:2]
			repo.accounts[0].Credentials["model_mapping"] = map[string]any{"customer-speech": "grok-tts"}
			repo.accounts[1].Credentials["model_mapping"] = map[string]any{"grok-4.5": "grok-4.5"}
			c, rec := grokMediaSlotContext(context.Background(), endpoint == "stt")
			key, ok := middleware2.GetAPIKeyFromContext(c)
			require.True(t, ok)
			key.Group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"grok-4.5", "customer-speech"}}
			method := http.MethodGet
			if endpoint == "stt" {
				method = http.MethodPost
			}
			c.Request = httptest.NewRequest(method, "/v1/"+endpoint, strings.NewReader("synthetic"))
			h.GrokVoice(c, endpoint)
			require.Equal(t, http.StatusOK, rec.Code)
			require.EqualValues(t, 1, wires.Load())
			require.Equal(t, "2", selected, "old non-TTS path must select the grok-4.5 Chat account, not the TTS-only mapping")
			require.Equal(t, slots.acquired, slots.released)
			require.False(t, slots.duplicateRelease)
		})
	}
}

func TestOpenAIGroupModelLessTTSKeepsOriginalKeyAndAccountScope(t *testing.T) {
	for _, fault := range []string{"valid", "no-grant", "wrong-profile", "foreign-group", "non-tts"} {
		t.Run(fault, func(t *testing.T) {
			var wires atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wires.Add(1)
				require.Equal(t, "/v1/tts", r.URL.Path)
				require.Equal(t, "source-A-pin", r.Header.Get("X-CLIProxy-Auth-ID"))
				data, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.NotContains(t, string(data), `"model"`)
				require.Contains(t, string(data), `"text_normalization":false`)
				require.Contains(t, string(data), `"alaw"`)
				w.WriteHeader(503)
				_, _ = io.WriteString(w, `{"error":{"message":"synthetic terminal failure"}}`)
			}))
			defer srv.Close()
			h, slots, repo := voiceWireHandler(t, srv)
			for i := range repo.accounts {
				a := &repo.accounts[i]
				a.Platform = service.PlatformOpenAI
				a.Credentials["base_url"] = "https://proxy.fixture.invalid/v1"
				a.Credentials["model_mapping"] = map[string]any{"customer-speech": "source-A/speech"}
				a.Credentials["upstream_protocols"] = map[string]any{"source-A/speech": "xai-voice-http"}
				a.Credentials["speech_profiles"] = map[string]any{"source-A/speech": "xai-grok-tts-v1"}
				a.Credentials["header_override_enabled"] = true
				a.Credentials["header_overrides"] = map[string]any{"X-CLIProxy-Auth-ID": "source-A-pin"}
			}
			ctx, rec := grokMediaSlotContext(context.Background(), true)
			key, ok := middleware2.GetAPIKeyFromContext(ctx)
			require.True(t, ok)
			key.Group.Platform = service.PlatformOpenAI
			key.Group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"customer-speech"}}
			originalKey, originalGroup := key.ID, *key.GroupID
			endpoint := "tts"
			switch fault {
			case "no-grant":
				key.Group.ModelAllowlist.Models = []string{"grok-4.5"}
			case "wrong-profile":
				for i := range repo.accounts {
					delete(repo.accounts[i].Credentials, "speech_profiles")
				}
			case "foreign-group":
				for i := range repo.accounts {
					repo.accounts[i].GroupIDs = []int64{99}
				}
			case "non-tts":
				endpoint = "stt"
			}
			ctx.Request = httptest.NewRequest("POST", "/v1/"+endpoint, strings.NewReader(`{"text":"你好！","language":"auto","voice_id":"eve","text_normalization":false,"output_format":{"codec":"alaw","sample_rate":8000}}`))
			ctx.Request.Header.Set("X-CLIProxy-Auth-ID", "forged-B")
			h.GrokVoice(ctx, endpoint)
			require.Equal(t, originalKey, key.ID)
			require.Equal(t, originalGroup, *key.GroupID)
			require.Equal(t, service.PlatformOpenAI, key.Group.Platform)
			if fault == "valid" {
				require.Equal(t, http.StatusBadGateway, rec.Code)
				require.Contains(t, rec.Body.String(), `"type":"upstream_error"`)
				require.Contains(t, rec.Body.String(), "Upstream service temporarily unavailable")
				require.EqualValues(t, 1, wires.Load())
			} else {
				require.Zero(t, wires.Load())
				if fault == "non-tts" {
					require.Equal(t, 404, rec.Code)
				} else {
					require.Equal(t, 400, rec.Code)
				}
			}
			require.Equal(t, slots.acquired, slots.released)
			require.False(t, slots.duplicateRelease)
		})
	}
}
