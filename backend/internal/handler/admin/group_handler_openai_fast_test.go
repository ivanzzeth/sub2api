package admin

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"

	"github.com/stretchr/testify/require"
)

func TestGroupRequestsDecodeForceOpenAIFast(t *testing.T) {
	var createReq CreateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{"name":"fast","force_openai_fast":true,"free_openai_fast":true}`), &createReq))
	require.True(t, createReq.ForceOpenAIFast)
	require.True(t, createReq.FreeOpenAIFast)

	var updateReq UpdateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{"force_openai_fast":false,"free_openai_fast":false}`), &updateReq))
	require.NotNil(t, updateReq.ForceOpenAIFast)
	require.False(t, *updateReq.ForceOpenAIFast)
	require.NotNil(t, updateReq.FreeOpenAIFast)
	require.False(t, *updateReq.FreeOpenAIFast)

	var omitted UpdateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{}`), &omitted))
	require.Nil(t, omitted.ForceOpenAIFast)
	require.Nil(t, omitted.FreeOpenAIFast)
}

func TestGroupConditionalUpdateHTTPContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &stubAdminService{}
	h := NewGroupHandler(svc, nil, nil)
	r := gin.New()
	r.PUT("/groups/:id", h.Update)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/groups/42", strings.NewReader(`{"name":"unchanged-policy","expected_updated_at":"2026-11-04T10:00:00.123456Z"}`)))
	require.Equal(t, 200, w.Code)
	require.Len(t, svc.updatedGroups, 1)
	require.True(t, svc.updatedGroups[0].ExpectedUpdatedAt.Equal(time.Date(2026, 11, 4, 10, 0, 0, 123456000, time.UTC)))
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, true, body["data"].(map[string]any)["supports_conditional_update"])
	bad := httptest.NewRecorder()
	r.ServeHTTP(bad, httptest.NewRequest("PUT", "/groups/42", strings.NewReader(`{"expected_updated_at":"bad"}`)))
	require.Equal(t, 400, bad.Code)
	require.Len(t, svc.updatedGroups, 1)
	legacy := httptest.NewRecorder()
	r.ServeHTTP(legacy, httptest.NewRequest("PUT", "/groups/42", strings.NewReader(`{"name":"legacy"}`)))
	require.Equal(t, 200, legacy.Code)
	require.Nil(t, svc.updatedGroups[1].ExpectedUpdatedAt)
}

func TestGroupConditionalUpdateSimpleModePreservesPrecondition(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newStubAdminService()
	h := NewGroupHandlerWithConfig(svc, nil, nil, &config.Config{RunMode: config.RunModeSimple})
	r := gin.New()
	r.PUT("/groups/:id", h.Update)
	// Old versions are forwarded unchanged for repository CAS, never converted into legacy writes.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/groups/42", strings.NewReader(`{"name":"simple","rate_multiplier":99,"expected_updated_at":"2000-01-01T00:00:00Z"}`)))
	require.Equal(t, 200, w.Code)
	require.Len(t, svc.updatedGroups, 1)
	require.Equal(t, 2000, svc.updatedGroups[0].ExpectedUpdatedAt.Year())
	require.Nil(t, svc.updatedGroups[0].RateMultiplier)
	zero := httptest.NewRecorder()
	r.ServeHTTP(zero, httptest.NewRequest("PUT", "/groups/42", strings.NewReader(`{"expected_updated_at":"0001-01-01T00:00:00Z"}`)))
	require.Equal(t, 400, zero.Code)
	require.Len(t, svc.updatedGroups, 1)
}
