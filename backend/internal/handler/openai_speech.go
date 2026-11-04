package handler

import (
	"errors"
	"io"
	"net/http"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *OpenAIGatewayHandler) AudioSpeech(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil || apiKey.Group == nil || apiKey.Group.Platform != service.PlatformOpenAI {
		h.errorResponse(c, 404, "not_found_error", "Speech requires the original OpenAI group")
		return
	}
	if !h.ensureResponsesDependencies(c, nil) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 32769))
	if err != nil || len(body) > 32768 {
		h.errorResponse(c, 400, "invalid_request_error", "Invalid bounded speech request")
		return
	}
	parsed, err := service.ParseOpenAISpeechRequest(body)
	if err != nil {
		h.errorResponse(c, 400, "invalid_request_error", err.Error())
		return
	}
	for name := range c.Request.Header {
		if service.IsSpeechControlHeader(name) {
			h.errorResponse(c, 400, "invalid_request_error", "Unsupported speech control header")
			return
		}
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		status, code, message, _ := billingErrorDetails(err)
		h.errorResponse(c, status, code, message)
		return
	}
	subject, subjectOK := middleware2.GetAuthSubjectFromContext(c)
	if !subjectOK {
		h.errorResponse(c, 401, "authentication_error", "Missing authenticated subject")
		return
	}
	{
		auditBody := []byte(`{"messages":[{"role":"user","content":` + parsed.AuditTextJSON() + `}]}`)
		if decision := h.checkSecurityAudit(c, requestLogger(c, "handler.openai_gateway.speech"), apiKey, subject, service.ContentModerationProtocolOpenAIChat, parsed.Model, auditBody); decision != nil && !decision.AllowNextStage {
			h.openAISecurityAuditError(c, decision)
			return
		}
	}
	var userStreamStarted bool
	userRelease, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &userStreamStarted, requestLogger(c, "handler.openai_gateway.speech"))
	if !acquired {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}
	mapping, restricted := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, parsed.Model)
	if restricted {
		h.errorResponse(c, 403, "permission_error", "Speech model is restricted by the native channel")
		return
	}
	channelModel := openAIChannelForwardModel(mapping, parsed.Model)
	// An estimate reserves native balance only; the real bill never uses this count.
	inflightDone, err := reserveInflightBalance(c, h.billingCacheService, h.gatewayService, apiKey, subscription, service.InflightEstimateRequest{Model: parsed.Model, Kind: service.InflightEstimateAudio, AudioMode: "tts", AudioUnits: 0.001})
	if err != nil {
		status, code, message, _ := billingErrorDetails(err)
		h.errorResponse(c, status, code, message)
		return
	}
	defer inflightDone()
	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(c.Request.Context(), apiKey.GroupID, "", "", channelModel, map[int64]struct{}{}, service.OpenAIUpstreamTransportHTTPSSE, "", false, false, false, service.PlatformOpenAI)
	if err != nil || selection == nil || selection.Account == nil {
		h.errorResponse(c, 503, "api_error", "No available speech account")
		return
	}
	var started bool
	release, status := h.acquireResponsesAccountSlot(c, apiKey.GroupID, "", selection, false, &started, requestLogger(c, "handler.openai_gateway.speech"))
	if status != openAISlotAcquireOK {
		return
	}
	defer release()
	result, billingKey, err := h.gatewayService.ForwardOpenAISpeech(c.Request.Context(), c, apiKey, selection.Account, parsed, channelModel)
	if err != nil {
		var prewrite *service.SpeechPrewriteError
		if errors.As(err, &prewrite) {
			h.errorResponse(c, http.StatusBadRequest, prewrite.Code, prewrite.Error())
			return
		}
		if !c.Writer.Written() {
			h.errorResponse(c, 502, "speech_outcome_unknown", "Speech outcome is unknown; do not resubmit")
		}
		return
	}
	// Reuse the existing mandatory native audio usage/dedup boundary, not its xAI estimator.
	h.recordGrokVoiceUsage(c, billingKey, selection.Account, subscription, "tts", body, result)
}
