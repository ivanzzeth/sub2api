package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type SpeechUpstreamProtocol string

const speechProtocolVolcengineTTS SpeechUpstreamProtocol = "volcengine-tts-v3-http"
const speechProtocolVolcengineArkText SpeechUpstreamProtocol = "volcengine-ark-text-v3"
const speechProfileV1 = "ark-agent-plan-tts-2-v1"
const speechVoiceV1 = "zh_female_vv_uranus_bigtts"
const speechPrivateCountHeader = "X-CLIProxy-Speech-Text-Words"
const speechPublicCountHeader = "X-Audio-Text-Words"

type SpeechPrewriteError struct{ Code string }

func (e *SpeechPrewriteError) Error() string { return e.Code }
func speechReject(code string) error         { return &SpeechPrewriteError{Code: code} }

type OpenAISpeechRequest struct {
	Model, Input, Voice string
	Body                []byte
}

func (r *OpenAISpeechRequest) AuditTextJSON() string { b, _ := json.Marshal(r.Input); return string(b) }

func ParseOpenAISpeechRequest(body []byte) (*OpenAISpeechRequest, error) {
	if len(body) > 32768 || !utf8.Valid(body) {
		return nil, speechReject("invalid_speech_request")
	}
	// Reuse the existing strict single-object/duplicate-key guard unchanged.
	fields, err := seedreamOriginalFields(body)
	if err != nil {
		return nil, speechReject("invalid_speech_request")
	}
	for key := range fields {
		switch key {
		case "model", "input", "voice", "response_format", "speed":
		default:
			return nil, speechReject("unsupported_speech_parameter")
		}
	}
	r := &OpenAISpeechRequest{Voice: speechVoiceV1, Body: append([]byte(nil), body...)}
	if json.Unmarshal(fields["model"], &r.Model) != nil || r.Model != "doubao-seed-tts-2.0" || json.Unmarshal(fields["input"], &r.Input) != nil || strings.TrimSpace(r.Input) == "" || utf8.RuneCountInString(r.Input) > 1000 {
		return nil, speechReject("invalid_speech_request")
	}
	if raw, ok := fields["voice"]; ok {
		var v *string
		if json.Unmarshal(raw, &v) != nil || v == nil || *v != speechVoiceV1 {
			return nil, speechReject("unsupported_speech_voice")
		}
	}
	if raw, ok := fields["response_format"]; ok {
		var v *string
		if json.Unmarshal(raw, &v) != nil || v == nil || *v != "mp3" {
			return nil, speechReject("unsupported_speech_format")
		}
	}
	if raw, ok := fields["speed"]; ok {
		var value *float64
		if json.Unmarshal(raw, &value) != nil || value == nil || *value != 1 {
			return nil, speechReject("unsupported_speech_speed")
		}
	}
	return r, nil
}

func IsSpeechControlHeader(name string) bool {
	return strings.EqualFold(name, speechPrivateCountHeader) || strings.EqualFold(name, speechPublicCountHeader) || strings.EqualFold(name, "Idempotency-Key") || strings.EqualFold(name, "X-Idempotency-Key")
}

func accountUpstreamProtocol(account *Account, final string) SpeechUpstreamProtocol {
	if account == nil {
		return ""
	}
	switch protocols := account.Credentials["upstream_protocols"].(type) {
	case map[string]any:
		value, present := protocols[final]
		if !present {
			return ""
		}
		protocol, ok := value.(string)
		if !ok {
			return "invalid"
		}
		return SpeechUpstreamProtocol(protocol)
	case map[string]string:
		return SpeechUpstreamProtocol(protocols[final])
	}
	return ""
}
func speechAccountProfile(account *Account, final string) string {
	if account == nil {
		return ""
	}
	var value string
	switch profiles := account.Credentials["speech_profiles"].(type) {
	case map[string]any:
		value, _ = profiles[final].(string)
	case map[string]string:
		value = profiles[final]
	}
	if value != speechProfileV1 {
		return ""
	}
	return value
}

// ForwardOpenAISpeech returns a fresh native price snapshot for mandatory RecordUsage.
func (s *OpenAIGatewayService) ForwardOpenAISpeech(ctx context.Context, c *gin.Context, key *APIKey, account *Account, parsed *OpenAISpeechRequest, channelModel string) (*OpenAIForwardResult, *APIKey, error) {
	if key == nil || key.Group == nil || key.GroupID == nil || key.Group.Platform != PlatformOpenAI || account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey || parsed == nil {
		return nil, nil, speechReject("speech_profile_required")
	}
	final := account.GetMappedModel(channelModel)
	if accountUpstreamProtocol(account, final) != speechProtocolVolcengineTTS || speechAccountProfile(account, final) == "" {
		return nil, nil, speechReject("speech_profile_required")
	}
	if s.resolver == nil || s.channelService == nil || s.channelService.groupRepo == nil {
		return nil, nil, speechReject("speech_pricing_unavailable")
	}
	group, err := s.channelService.groupRepo.GetByIDLite(ctx, *key.GroupID)
	if err != nil || group == nil || group.ID != key.Group.ID {
		return nil, nil, speechReject("speech_pricing_unavailable")
	}
	price := group.AudioTTSPricePerMillionChars
	if price == nil || math.IsNaN(*price) || math.IsInf(*price, 0) || *price <= 0 {
		return nil, nil, speechReject("speech_pricing_required")
	}
	billingKey := *key
	billingKey.Group = group
	candidates := s.filterCNProviderBillingModelCandidates(ctx, account, &billingKey, usageBillingModelCandidates(forwardResultBillingModel(parsed.Model, final), channelModel, parsed.Model, final))
	for _, candidate := range candidates {
		if resolved := s.resolveOpenAIChannelPricing(ctx, candidate, &billingKey); resolved != nil && resolved.Mode == BillingModePerRequest {
			return nil, nil, speechReject("speech_pricing_conflict")
		}
	}
	payloadFields := map[string]any{"model": final, "input": parsed.Input, "voice": speechVoiceV1, "response_format": "mp3", "speed": 1}
	payload, _ := json.Marshal(payloadFields)
	token, _, err := s.getRequestCredential(ctx, c, account)
	if err != nil {
		return nil, nil, err
	}
	base, err := s.validateUpstreamBaseURL(account.GetOpenAIBaseURL())
	if err != nil {
		return nil, nil, speechReject("speech_proxy_origin_invalid")
	}
	target := strings.TrimSuffix(buildOpenAIResponsesURL(base), "/responses") + "/audio/speech"
	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()
	upstreamCtx = WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamVoiceSingleAttempt(upstreamCtx))
	request, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	account.ApplyHeaderOverrides(request.Header)
	for name := range request.Header {
		if IsSpeechControlHeader(name) {
			return nil, nil, speechReject("unsupported_speech_control_header")
		}
	}
	request.GetBody = nil
	proxy := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	started := time.Now()
	response, err := s.httpUpstream.Do(request, proxy, account.ID, account.Concurrency)
	if err != nil {
		return nil, nil, fmt.Errorf("speech_outcome_unknown")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("speech_outcome_unknown")
	}
	audio, err := ReadUpstreamResponseBody(response.Body, s.cfg, c, openAITooLargeError)
	if err != nil || len(audio) == 0 || len(audio) > 16<<20 {
		return nil, nil, fmt.Errorf("speech_outcome_unknown")
	}
	counts := response.Header.Values(speechPrivateCountHeader)
	if len(counts) != 1 || response.Header.Get("Content-Type") != "audio/mpeg" {
		return nil, nil, fmt.Errorf("speech_outcome_unknown")
	}
	words, err := strconv.ParseInt(counts[0], 10, 64)
	if err != nil || words < 0 || words > (1<<53) {
		return nil, nil, fmt.Errorf("speech_outcome_unknown")
	}
	// Neither request headers nor supplier NDJSON framing reach the customer.
	c.Header(speechPrivateCountHeader, "")
	c.Header("Content-Encoding", "")
	c.Header("Transfer-Encoding", "")
	c.Header("Content-Length", strconv.Itoa(len(audio)))
	c.Header(speechPublicCountHeader, strconv.FormatInt(words, 10))
	c.Header("Access-Control-Expose-Headers", speechPublicCountHeader)
	c.Data(http.StatusOK, "audio/mpeg", audio)
	logger.L().Info("speech.completed", zap.String("model", parsed.Model), zap.String("upstream_protocol", string(speechProtocolVolcengineTTS)), zap.Int64("text_words", words))
	return &OpenAIForwardResult{RequestID: StableGrokAudioBillingRequestID(uuid.NewString()), Model: parsed.Model, UpstreamModel: final, Duration: time.Since(started), AudioUsage: &AudioUsage{Mode: "tts", DurationOrUnits: float64(words) / 1e6}}, &billingKey, nil
}
