package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// DefaultGrokRealtimeDialTimeout bounds the pre-accept upstream handshake.
// The timeout only covers dialing; an established session is not interrupted.
const DefaultGrokRealtimeDialTimeout = 12 * time.Second

// supportedGrokVoiceHTTPEndpoints are xAI Voice HTTP paths we forward as-is.
var supportedGrokVoiceHTTPEndpoints = map[string]struct{}{
	"tts":           {},
	"stt":           {},
	"custom-voices": {},
}

// grokVoiceAttemptError preserves native error classification while preventing
// a second account from replaying an unsafe request after a possible wire write.
type grokVoiceAttemptError struct{ err error }

func (e *grokVoiceAttemptError) Error() string { return e.err.Error() }
func (e *grokVoiceAttemptError) Unwrap() error { return e.err }
func GrokVoiceRequestAttempted(err error) bool {
	var attempted *grokVoiceAttemptError
	return errors.As(err, &attempted)
}

// ForwardGrokVoice forwards the official xAI Voice HTTP APIs (/tts, /stt, and
// the custom-voices CRUD/audio subresources).
// The response is intentionally passed through because TTS returns audio bytes
// while STT returns JSON and xAI may add format-specific headers.
func (s *OpenAIGatewayService) ForwardGrokVoice(ctx context.Context, c *gin.Context, account *Account, endpoint string, body []byte, contentType string) (*OpenAIForwardResult, error) {
	return s.forwardGrokVoice(ctx, c, account, endpoint, body, contentType, "")
}

// ForwardGrokTTSForKey keeps model-less input in the original authorized Group.
// Only the explicit xAI profile may use an OpenAI account to reach its pinned Proxy.
func (s *OpenAIGatewayService) ForwardGrokTTSForKey(ctx context.Context, c *gin.Context, key *APIKey, selected *Account, selectionModel string, body []byte, contentType string) (*OpenAIForwardResult, *Account, error) {
	if s == nil || key == nil || key.Group == nil || selected == nil || selected.ID <= 0 || (key.Group.Platform != PlatformOpenAI && key.Group.Platform != PlatformGrok) {
		return nil, nil, speechReject("speech_account_scope_required")
	}
	resolved, _, qualified, err := s.resolveGrokTTSAccounts(ctx, key)
	if err != nil {
		return nil, nil, speechReject("speech_profile_unavailable")
	}
	// Absence from an exclusion set proves nothing. The selected ID must be
	// positively present in this request's current full-qualified candidate set.
	account, exists := qualified[selected.ID]
	if !exists || account == nil || resolved != selectionModel {
		return nil, nil, speechReject("speech_operation_profile_required")
	}
	// Use this full object for protocol, credentials, URL, pin and native usage;
	// never reuse the stale scheduler argument except for its selected ID.
	if account.Platform == PlatformGrok {
		result, forwardErr := s.ForwardGrokVoice(ctx, c, account, "tts", body, contentType)
		return result, account, forwardErr
	}
	pin, pinPresent := account.HeaderOverrideValue("x-cliproxy-auth-id")
	pin = strings.TrimSpace(pin)
	if !pinPresent || pin == "" || strings.ContainsAny(pin, ",\r\n") {
		return nil, nil, speechReject("speech_proxy_pin_required")
	}
	base, err := s.validateUpstreamBaseURL(account.GetOpenAIBaseURL())
	if err != nil {
		return nil, nil, speechReject("speech_proxy_origin_invalid")
	}
	target, err := url.Parse(base)
	if err != nil || strings.EqualFold(target.Hostname(), "api.x.ai") || isGrokCLIProxyTarget(base) {
		return nil, nil, speechReject("speech_proxy_origin_required")
	}
	proxyURL := strings.TrimSuffix(buildOpenAIResponsesURL(base), "/responses") + "/tts"
	result, forwardErr := s.forwardGrokVoice(ctx, c, account, "tts", body, contentType, proxyURL)
	return result, account, forwardErr
}

func (s *OpenAIGatewayService) forwardGrokVoice(ctx context.Context, c *gin.Context, account *Account, endpoint string, body []byte, contentType string, proxyTTSURL string) (*OpenAIForwardResult, error) {
	if s == nil || account == nil {
		return nil, fmt.Errorf("grok voice service/account is required")
	}
	if account.Platform != PlatformGrok && !(account.Platform == PlatformOpenAI && endpoint == "tts" && proxyTTSURL != "") {
		return nil, fmt.Errorf("account platform %s is not supported for grok voice", account.Platform)
	}
	endpoint = strings.Trim(strings.TrimSpace(endpoint), "/")
	parts := strings.Split(endpoint, "/")
	baseEndpoint := parts[0]
	if _, ok := supportedGrokVoiceHTTPEndpoints[baseEndpoint]; !ok {
		return nil, fmt.Errorf("unsupported grok voice endpoint: %s", endpoint)
	}
	if len(parts) > 1 && baseEndpoint != "custom-voices" {
		return nil, fmt.Errorf("unsupported grok voice endpoint: %s", endpoint)
	}
	if baseEndpoint == "custom-voices" {
		if len(parts) > 3 || (len(parts) == 3 && parts[2] != "audio") {
			return nil, fmt.Errorf("unsupported grok voice endpoint: %s", endpoint)
		}
	}
	for _, part := range parts[1:] {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "?#\\") {
			return nil, fmt.Errorf("invalid grok voice endpoint path")
		}
	}
	token, _, err := s.getRequestCredential(ctx, c, account)
	if err != nil {
		return nil, err
	}
	targetURL := proxyTTSURL
	if targetURL == "" {
		targetURL, err = buildGrokVoiceURL(account, s.cfg, endpoint)
		if err != nil {
			return nil, err
		}
	}
	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()
	method := http.MethodPost
	if c != nil && c.Request != nil && strings.TrimSpace(c.Request.Method) != "" {
		method = c.Request.Method
	}
	unsafe := method == http.MethodPost || method == http.MethodPatch || method == http.MethodDelete
	upstreamCtx = WithHTTPUpstreamRedirectsDisabled(upstreamCtx)
	if unsafe {
		upstreamCtx = WithHTTPUpstreamVoiceSingleAttempt(upstreamCtx)
	}
	req, err := http.NewRequestWithContext(upstreamCtx, method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json, audio/*")
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	// Match media path: CLI identity headers only on the CLI chat proxy.
	// Official api.x.ai voice rejects or mistreats OAuth when CLI headers are stamped.
	if account.IsGrokOAuth() && isGrokCLIProxyTarget(targetURL) {
		applyGrokCLIHeaders(req.Header)
	}
	account.ApplyHeaderOverrides(req.Header)
	// Fresh server requests never copy the customer's pin or authentication headers.
	// Canonical vendor/CLI targets must not receive an internal Proxy credential ID.
	target, _ := url.Parse(targetURL)
	if target != nil && (strings.EqualFold(target.Hostname(), "api.x.ai") || isGrokCLIProxyTarget(targetURL)) {
		for key := range req.Header {
			if strings.EqualFold(key, "X-CLIProxy-Auth-ID") {
				delete(req.Header, key)
			}
		}
	}
	if unsafe {
		headers := []http.Header{req.Header}
		if c != nil && c.Request != nil {
			headers = append(headers, c.Request.Header)
		}
		for _, header := range headers {
			for key := range header {
				if strings.EqualFold(key, "Idempotency-Key") || strings.EqualFold(key, "X-Idempotency-Key") {
					MarkResponseCommitted(c)
					writeGrokMediaErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Idempotency headers are not supported for unsafe Voice requests")
					return nil, fmt.Errorf("unsafe voice idempotency header rejected")
				}
			}
		}
		req.GetBody = nil
	}
	wrapAttempt := func(err error) error {
		if unsafe && err != nil {
			return &grokVoiceAttemptError{err: err}
		}
		return err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	started := time.Now()
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(started).Milliseconds())
	if err != nil {
		return nil, wrapAttempt(s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		data, readErr := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
		if readErr != nil {
			return nil, wrapAttempt(readErr)
		}
		MarkResponseCommitted(c)
		if c != nil {
			for _, name := range []string{"Location", "Retry-After", "x-request-id"} {
				if value := resp.Header.Get(name); value != "" {
					c.Header(name, value)
				}
			}
		}
		writeGrokMediaResponse(c, resp, data, s.responseHeaderFilter)
		return nil, wrapAttempt(fmt.Errorf("voice upstream returned terminal redirect status %d", resp.StatusCode))
	}
	if resp.StatusCode >= 400 {
		result, responseErr := s.handleGrokMediaErrorResponse(ctx, resp, c, account, resp.Header.Get("x-request-id"), endpoint)
		return result, wrapAttempt(responseErr)
	}
	data, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, wrapAttempt(err)
	}
	writeGrokMediaResponse(c, resp, data, s.responseHeaderFilter)
	audioUsage := estimateGrokVoiceAudioUsage(baseEndpoint, body, contentType, data, time.Since(started))
	upstreamID := firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id"))
	return &OpenAIForwardResult{
		// Forced durable money-event id so usage_billing_dedup cannot collapse under a reused client id.
		RequestID:       StableGrokAudioBillingRequestID(upstreamID),
		UpstreamHeaders: resp.Header,
		Model:           baseEndpoint,
		UpstreamModel:   baseEndpoint,
		Duration:        time.Since(started),
		AudioUsage:      audioUsage,
	}, nil
}

// ProxyGrokRealtime relays JSON Realtime events to xAI's native Voice WS.
// Audio is carried as base64 inside JSON events, so preserving the JSON bytes
// is sufficient and avoids translating protocol event types.
func (s *OpenAIGatewayService) ProxyGrokRealtime(ctx context.Context, c *gin.Context, client *coderws.Conn, account *Account, token, model string) (bool, error) {
	if s == nil || client == nil || account == nil {
		return false, fmt.Errorf("realtime service, client, and account are required")
	}
	if account.Platform != PlatformGrok {
		return false, fmt.Errorf("account platform %s is not supported for grok realtime", account.Platform)
	}
	upstream, err := s.OpenGrokRealtime(ctx, account, token, model)
	if err != nil {
		return false, err
	}
	defer func() { _ = upstream.Close() }()
	return s.ProxyGrokRealtimeConn(ctx, c, client, upstream)
}

type GrokRealtimeUpstream struct{ conn openAIWSClientConn }

// GrokRealtimeDialError preserves an HTTP status returned before WebSocket
// upgrade so handlers can apply the normal Grok account policy.
type GrokRealtimeDialError struct {
	StatusCode int
	Err        error
}

func (e *GrokRealtimeDialError) Error() string { return e.Err.Error() }
func (e *GrokRealtimeDialError) Unwrap() error { return e.Err }

func (u *GrokRealtimeUpstream) Close() error {
	if u == nil || u.conn == nil {
		return nil
	}
	return u.conn.Close()
}

func (s *OpenAIGatewayService) OpenGrokRealtime(ctx context.Context, account *Account, token, model string) (*GrokRealtimeUpstream, error) {
	if s == nil || account == nil || account.Platform != PlatformGrok {
		return nil, fmt.Errorf("grok realtime account is required")
	}
	base, err := buildGrokVoiceURL(account, s.cfg, "realtime")
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	u.Scheme = "wss"
	q := u.Query()
	q.Set("model", firstNonEmpty(model, "grok-voice-latest"))
	u.RawQuery = q.Encode()
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	if account.IsGrokOAuth() && isGrokCLIProxyTarget(u.String()) {
		applyGrokCLIHeaders(headers)
	}
	account.ApplyHeaderOverrides(headers)
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	conn, status, _, err := s.getOpenAIWSPassthroughDialer().Dial(ctx, u.String(), headers, proxyURL)
	if err != nil {
		return nil, &GrokRealtimeDialError{StatusCode: status, Err: err}
	}
	return &GrokRealtimeUpstream{conn: conn}, nil
}

// HandleGrokRealtimeUpstreamError applies the shared Grok account policy to a
// failed pre-accept WebSocket handshake.
func (s *OpenAIGatewayService) HandleGrokRealtimeUpstreamError(ctx context.Context, account *Account, statusCode int, body []byte) {
	if statusCode <= 0 {
		statusCode = http.StatusBadGateway
	}
	s.handleGrokAccountUpstreamError(ctx, account, statusCode, nil, body)
}

func (s *OpenAIGatewayService) ProxyGrokRealtimeConn(ctx context.Context, c *gin.Context, client *coderws.Conn, upstream *GrokRealtimeUpstream) (bool, error) {
	if s == nil || client == nil || upstream == nil || upstream.conn == nil {
		return false, fmt.Errorf("realtime connection is required")
	}
	conn := upstream.conn

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	var audioObserved atomic.Bool

	// Upstream → client
	go func() {
		for {
			msg, readErr := conn.ReadMessage(ctx)
			if readErr != nil {
				errCh <- readErr
				return
			}
			if grokRealtimeEventHasAudio(msg) {
				audioObserved.Store(true)
			}
			if writeErr := client.Write(ctx, coderws.MessageText, msg); writeErr != nil {
				errCh <- writeErr
				return
			}
		}
	}()

	// Client → upstream (JSON events only)
	go func() {
		for {
			kind, msg, readErr := client.Read(ctx)
			if readErr != nil {
				errCh <- readErr
				return
			}
			if kind != coderws.MessageText && kind != coderws.MessageBinary {
				continue
			}
			if grokRealtimeEventHasAudio(msg) {
				audioObserved.Store(true)
			}
			var raw json.RawMessage
			if unmarshalErr := json.Unmarshal(msg, &raw); unmarshalErr != nil {
				errCh <- fmt.Errorf("invalid realtime event: %w", unmarshalErr)
				return
			}
			if writeErr := conn.WriteJSON(ctx, raw); writeErr != nil {
				errCh <- writeErr
				return
			}
		}
	}()

	return awaitGrokRealtimeAudioObserved(errCh, &audioObserved)
}

// ProbeGrokRealtime performs the upstream WebSocket handshake without sending
// any client-visible events. Handlers use it before accepting the downstream
// upgrade so authentication and endpoint failures remain ordinary HTTP errors.
func (s *OpenAIGatewayService) ProbeGrokRealtime(ctx context.Context, account *Account, token, model string) error {
	if s == nil || account == nil {
		return fmt.Errorf("realtime service and account are required")
	}
	if account.Platform != PlatformGrok {
		return fmt.Errorf("account platform %s is not supported for grok realtime", account.Platform)
	}
	base, err := buildGrokVoiceURL(account, s.cfg, "realtime")
	if err != nil {
		return err
	}
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	u.Scheme = "wss"
	q := u.Query()
	q.Set("model", firstNonEmpty(model, "grok-voice-latest"))
	u.RawQuery = q.Encode()
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	if account.IsGrokOAuth() && isGrokCLIProxyTarget(u.String()) {
		applyGrokCLIHeaders(headers)
	}
	account.ApplyHeaderOverrides(headers)
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	dialer := s.getOpenAIWSPassthroughDialer()
	conn, _, _, err := dialer.Dial(ctx, u.String(), headers, proxyURL)
	if err != nil {
		return err
	}
	return conn.Close()
}

func awaitGrokRealtimeAudioObserved(errCh <-chan error, audioObserved *atomic.Bool) (bool, error) {
	err := <-errCh
	if audioObserved == nil {
		return false, err
	}
	return audioObserved.Load(), err
}

func grokRealtimeEventHasAudio(msg []byte) bool {
	if !gjson.ValidBytes(msg) {
		return false
	}
	eventType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(msg, "type").String()))
	if !strings.Contains(eventType, "audio") || strings.Contains(eventType, "transcript") {
		return false
	}
	for _, path := range []string{"audio", "delta", "data"} {
		value := gjson.GetBytes(msg, path)
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			return true
		}
	}
	return false
}

// estimateGrokVoiceAudioUsage derives billing units from the request/response.
// TTS: million characters of input text; STT: hours approximated from request body size
// when duration is unknown; custom-voices: no units (nil).
func estimateGrokVoiceAudioUsage(endpoint string, reqBody []byte, contentType string, respBody []byte, elapsed time.Duration) *AudioUsage {
	switch strings.TrimSpace(endpoint) {
	case "tts":
		// Prefer JSON "input" / "text" fields; fallback to raw body length.
		chars := 0
		if gjson.ValidBytes(reqBody) {
			for _, key := range []string{"input", "text", "prompt"} {
				if s := strings.TrimSpace(gjson.GetBytes(reqBody, key).String()); s != "" {
					chars = len([]rune(s))
					break
				}
			}
		}
		if chars <= 0 {
			chars = len(reqBody)
		}
		if chars <= 0 {
			return nil
		}
		return &AudioUsage{Mode: "tts", DurationOrUnits: float64(chars) / 1_000_000.0}
	case "stt":
		// Prefer response duration when present; do not trust client duration_seconds alone
		// (under-report would underbill). Floor against body-size heuristic and elapsed.
		secs := 0.0
		if gjson.ValidBytes(respBody) {
			for _, path := range []string{"duration", "duration_seconds", "audio_duration", "usage.seconds"} {
				if v := gjson.GetBytes(respBody, path); v.Exists() && v.Type == gjson.Number && v.Float() > 0 {
					secs = v.Float()
					break
				}
			}
		}
		// Multipart / body size heuristic: ~16KB/s for compressed speech (lower bound).
		sizeFloor := 0.0
		if len(reqBody) > 0 {
			sizeFloor = float64(len(reqBody)) / 16000.0
		}
		clientSecs := 0.0
		if gjson.ValidBytes(reqBody) {
			if v := gjson.GetBytes(reqBody, "duration_seconds"); v.Exists() && v.Type == gjson.Number {
				clientSecs = v.Float()
			}
		}
		if secs <= 0 {
			secs = elapsed.Seconds()
		}
		if secs <= 0 {
			secs = clientSecs
		}
		if secs <= 0 {
			secs = sizeFloor
		}
		// Cap untrusted client under-report: if client duration is much smaller than
		// size/elapsed floors, bill the larger of floors (anti underbill).
		if clientSecs > 0 && secs == clientSecs {
			floor := sizeFloor
			if elapsed.Seconds() > floor {
				floor = elapsed.Seconds()
			}
			if floor > 0 && clientSecs < floor*0.5 {
				secs = floor
			}
		}
		if secs <= 0 {
			return nil
		}
		return &AudioUsage{Mode: "stt", DurationOrUnits: secs / 3600.0}
	case "realtime":
		mins := elapsed.Minutes()
		if mins <= 0 {
			return nil
		}
		return &AudioUsage{Mode: "realtime", DurationOrUnits: mins}
	default:
		return nil
	}
}

// ResolveGrokTTSSelectionModel derives the existing model-less operation's identity
// from this Group's actual authorized public aliases and exact Account mappings.
// It does not choose a default model or create an entitlement.
func (s *OpenAIGatewayService) ResolveGrokTTSSelectionModel(ctx context.Context, key *APIKey) (string, map[int64]struct{}, error) {
	model, excluded, _, err := s.resolveGrokTTSAccounts(ctx, key)
	return model, excluded, err
}

// resolveGrokTTSAccounts is a request-local proof, not another scheduler/cache.
func (s *OpenAIGatewayService) resolveGrokTTSAccounts(ctx context.Context, key *APIKey) (string, map[int64]struct{}, map[int64]*Account, error) {
	excluded := map[int64]struct{}{}
	qualified := map[int64]*Account{}
	if s == nil || key == nil || key.GroupID == nil || key.Group == nil || (key.Group.Platform != PlatformGrok && key.Group.Platform != PlatformOpenAI) {
		return "", excluded, nil, speechReject("speech_group_required")
	}
	accounts, err := s.listSchedulableAccounts(ctx, key.GroupID, key.Group.Platform)
	if err != nil {
		return "", excluded, nil, err
	}
	identities := map[string]string{}
	for i := range accounts {
		candidate := &accounts[i]
		excluded[candidate.ID] = struct{}{}
		// Candidate metadata intentionally omits protocol/profile fields. Use the
		// existing full-account hydration before treating them as admission evidence.
		a, hydrateErr := s.hydrateSelectedAccount(ctx, candidate)
		if hydrateErr != nil {
			return "", excluded, nil, speechReject("speech_profile_full_account_unavailable")
		}
		if a == nil || a.ID <= 0 || a.ID != candidate.ID || a.Platform != key.Group.Platform || a.Type != AccountTypeAPIKey || !a.IsSchedulable() || !SpeechAccountBelongsToGroup(key, a) {
			continue
		}
		for alias := range a.GetModelMapping() {
			if alias == "" || strings.ContainsAny(alias, "*?") {
				continue
			}
			if key.Group.ModelAllowlistEnabled() && !key.Group.ModelAllowlist.Allows(alias) {
				continue
			}
			mapping, restricted := s.ResolveChannelMappingAndRestrict(ctx, key.GroupID, alias)
			if restricted {
				continue
			}
			channelModel := alias
			if mapping.Mapped {
				channelModel = mapping.MappedModel
			}
			final := a.GetMappedModel(channelModel)
			if accountUpstreamProtocol(a, final) != speechProtocolXAI || speechAccountProfile(a, final) != speechProfileXAI {
				continue
			}
			// Public aliases are authorization identities. If existing native mappings
			// cannot prove a single one, model-less input cannot select between them.
			identity := alias + "\x00" + channelModel + "\x00" + speechProfileXAI + "\x00tts"
			identities[identity] = channelModel
			delete(excluded, a.ID)
			qualified[a.ID] = a
		}
	}
	if len(identities) != 1 {
		return "", excluded, nil, speechReject("speech_operation_profile_missing_or_ambiguous")
	}
	for _, model := range identities {
		return model, excluded, qualified, nil
	}
	return "", excluded, nil, speechReject("speech_operation_profile_required")
}
