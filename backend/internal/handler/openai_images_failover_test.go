//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type openAIImagesFailoverAccountRepo struct {
	service.AccountRepository
	accounts []service.Account
}

func (r openAIImagesFailoverAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, service.ErrNoAvailableAccounts
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableUngroupedByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r openAIImagesFailoverAccountRepo) accountsForPlatform(platform string) []service.Account {
	out := make([]service.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform {
			out = append(out, account)
		}
	}
	return out
}

type openAIImagesFailoverHTTPUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
}

func (u *openAIImagesFailoverHTTPUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"req_img_failover"},
		},
		Body: io.NopCloser(bytes.NewBufferString(
			"data: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"code\":\"server_error\",\"message\":\"image backend unavailable\"}}\n\n",
		)),
	}, nil
}

func (u *openAIImagesFailoverHTTPUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func TestOpenAIGatewayHandlerImages_ServerErrorFailsOverAndReturnsClearErrorWhenExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(3130)
	accounts := []service.Account{
		{
			ID:          1,
			Name:        "image-account-1",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    0,
			Credentials: map[string]any{"access_token": "token-1"},
		},
		{
			ID:          2,
			Name:        "image-account-2",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    1,
			Credentials: map[string]any{"access_token": "token-2"},
		},
	}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: accounts}
	upstream := &openAIImagesFailoverHTTPUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		nil,
		nil,
		nil,
		upstream,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	concurrencyService := service.NewConcurrencyService(nil)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		concurrencyService,
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil,
		nil,
		nil,
		nil,
		cfg,
	)
	handler.maxAccountSwitches = 10

	body := []byte(`{"model":"gpt-image-1","prompt":"draw a cat","quality":"high","size":"1536x1024"}`)
	core, observedLogs := observer.New(zap.DebugLevel)
	requestCtx := logger.IntoContext(context.Background(), zap.New(core))
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body)).WithContext(requestCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      99,
		GroupID: &groupID,
		Group: &service.Group{
			ID:                   groupID,
			AllowImageGeneration: true,
		},
		User: &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})

	handler.Images(c)

	accountSelectingLogs := observedLogs.FilterMessage("openai.images.account_selecting").All()
	require.NotEmpty(t, accountSelectingLogs)
	loggedFields := make(map[string]string)
	for _, field := range accountSelectingLogs[0].Context {
		loggedFields[field.Key] = field.String
	}
	require.Equal(t, "high", loggedFields["img_quality"])
	require.Equal(t, "1536x1024", loggedFields["img_size"])
	require.NotContains(t, loggedFields, "prompt")

	require.Equal(t, []int64{1, 2}, upstream.calls())
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "upstream_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "Upstream service temporarily unavailable", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())

	rawEvents, ok := c.Get(service.OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := rawEvents.([]*service.OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 2)
	require.Equal(t, "failover", events[0].Kind)
	require.Equal(t, "failover", events[1].Kind)
}

// Reuse the real Images handler, scheduler/service and existing account repo.
// Only native persistence and external provider HTTP are deterministic doubles.
type seedreamHandlerHTTPUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
}

func (u *seedreamHandlerHTTPUpstream) Do(r *http.Request, proxyURL string, id int64, concurrency int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, id)
	u.mu.Unlock()
	if !service.HTTPUpstreamImageSingleAttempt(r.Context()) {
		return nil, fmt.Errorf("missing trusted image transport marker")
	}
	return u.HTTPUpstream.Do(r, proxyURL, id, concurrency)
}
func (u *seedreamHandlerHTTPUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}
func seedreamHandlerFixture(t *testing.T, origin string, profiles bool) (*OpenAIGatewayHandler, *seedreamHandlerHTTPUpstream) {
	t.Helper()
	route := "source-owned/doubao-seedream-5pro-synthetic"
	accounts := []service.Account{}
	for i := int64(1); i <= 2; i++ {
		creds := map[string]any{"api_key": "synthetic-relay-only", "base_url": origin, "model_mapping": map[string]any{"gpt-image-1": route}, "pool_mode": true, "pool_mode_retry_count": 3}
		if profiles {
			creds["image_profiles"] = map[string]any{route: "ark-agent-plan-seedream-5pro-v1"}
		}
		accounts = append(accounts, service.Account{ID: i, Name: fmt.Sprint("seedream-", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: int(i - 1), Credentials: creds})
	}
	cfg := &config.Config{RunMode: config.RunModeSimple, Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	upstream := &seedreamHandlerHTTPUpstream{HTTPUpstream: repository.NewHTTPUpstream(cfg)}
	gateway := service.NewOpenAIGatewayService(openAIImagesFailoverAccountRepo{accounts: accounts}, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	handler := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	handler.maxAccountSwitches = 10
	return handler, upstream
}
func runSeedreamHandler(t *testing.T, h *OpenAIGatewayHandler, body string, tierPrices ...*float64) *httptest.ResponseRecorder {
	t.Helper()
	groupID := int64(3130)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	price1K, price2K := 0.20, 0.30
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, AllowImageGeneration: true, ImagePrice1K: &price1K, ImagePrice2K: &price2K}
	if len(tierPrices) == 2 {
		group.ImagePrice1K = tierPrices[0]
		group.ImagePrice2K = tierPrices[1]
	}
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 99, GroupID: &groupID, Group: group, User: &service.User{ID: 100}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
	h.Images(c)
	return rec
}
func TestSeedreamHandlerRawJSONStopsBeforeProxyPreparation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","stream":null}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","stream":false,"stream":false}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","str\u0065am":false,"stream":false}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K","si\u007ae":"2K"}`,
		`{"model":"gpt-image-1","prompt":"draw","size":"1K"}{}`,
	} {
		t.Run(body, func(t *testing.T) {
			var downstream atomic.Int32
			// If the Sub guard were bypassed, this downstream image handler would receive
			// the request, just where shared Proxy preparation may remove stream.
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downstream.Add(1); w.WriteHeader(500) }))
			defer proxy.Close()
			h, upstream := seedreamHandlerFixture(t, proxy.URL, true)
			rec := runSeedreamHandler(t, h, body)
			require.Equal(t, 400, rec.Code)
			require.Empty(t, upstream.calls())
			require.Zero(t, downstream.Load())
		})
	}
}
func TestSeedreamHandlerAttemptNeverSwitchesAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"commit_then_EOF", "typed_unknown_502", "invalid_output_size", "redirect_307", "redirect_308"} {
		t.Run(mode, func(t *testing.T) {
			var calls, targetCalls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); w.WriteHeader(500) }))
			defer target.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.Equal(t, "source-owned/doubao-seedream-5pro-synthetic", gjson.GetBytes(body, "model").String())
				if mode == "redirect_307" || mode == "redirect_308" {
					w.Header().Set("Location", target.URL)
					status := 307
					if mode == "redirect_308" {
						status = 308
					}
					w.WriteHeader(status)
					return
				}
				if mode == "commit_then_EOF" {
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if mode == "typed_unknown_502" {
					w.WriteHeader(502)
					_, _ = io.WriteString(w, `{"error":{"code":"image_generation_outcome_unknown","message":"synthetic unknown","retryable":false}}`)
					return
				}
				_, _ = io.WriteString(w, `{"data":[{"b64_json":"AA==","size":"1.5K"}]}`)
			}))
			defer proxy.Close()
			h, upstream := seedreamHandlerFixture(t, proxy.URL, true)
			rec := runSeedreamHandler(t, h, `{"model":"gpt-image-1","prompt":"draw","size":"1K","n":1}`)
			require.Equal(t, 502, rec.Code)
			require.Equal(t, int32(1), calls.Load())
			require.Zero(t, targetCalls.Load(), "profile must not follow redirect/replay")
			require.Equal(t, []int64{1}, upstream.calls(), "must not replay/switch to second account")
			require.NotContains(t, rec.Body.String(), `"data"`)
			require.Contains(t, rec.Body.String(), "image_generation_outcome_unknown")
		})
	}
}
func TestSeedreamHandlerMissingProfileIsPrewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer proxy.Close()
	h, upstream := seedreamHandlerFixture(t, proxy.URL, false)
	rec := runSeedreamHandler(t, h, `{"model":"gpt-image-1","prompt":"draw","size":"1K"}`)
	require.Equal(t, 400, rec.Code)
	require.Contains(t, rec.Body.String(), "image_profile_required")
	require.Zero(t, calls.Load())
	require.Empty(t, upstream.calls())
}

// This is a prewrite policy regression, not a request to infer an Ark price from
// the generic native fallback. With only 1K priced, 2K must never be sent.
func TestSeedreamUnpricedTierMustNotReachSupplier(t *testing.T) {
	for _, tc := range []struct {
		name  string
		price *float64
	}{
		{"nil", nil}, {"zero", seedreamPricePointer(0)}, {"negative", seedreamPricePointer(-1)},
		{"NaN", seedreamPricePointer(math.NaN())}, {"positive_Inf", seedreamPricePointer(math.Inf(1))}, {"negative_Inf", seedreamPricePointer(math.Inf(-1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			var calls atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(502)
				_, _ = io.WriteString(w, `{"error":{"code":"synthetic_stop_after_observation","message":"no real vendor"}}`)
			}))
			defer proxy.Close()
			h, upstream := seedreamHandlerFixture(t, proxy.URL, true)
			p1 := 0.20
			rec := runSeedreamHandler(t, h, `{"model":"gpt-image-1","prompt":"draw","size":"2K"}`, &p1, tc.price)
			require.Zero(t, calls.Load(), "unpriced requested tier reached supplier")
			require.Empty(t, upstream.calls())
			require.Equal(t, 400, rec.Code)
			require.Equal(t, "image_pricing_required", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
		})
	}
}
func seedreamPricePointer(v float64) *float64 { return &v }
