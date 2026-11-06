package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"strings"
)

type geminiProxyGroupScopeKey struct{}

// WithGeminiProxyGroupScope locks the original authenticated Group; it does not
// change Group.platform or create a new scheduler/permission authority.
func WithGeminiProxyGroupScope(ctx context.Context, groupID int64) context.Context {
	return context.WithValue(ctx, geminiProxyGroupScopeKey{}, groupID)
}
func geminiProxyGroupScopeAllows(ctx context.Context, id *int64) bool {
	original, locked := ctx.Value(geminiProxyGroupScopeKey{}).(int64)
	return !locked || id != nil && *id == original
}

// GeminiProxyModelAllowed consumes the existing server-stored Source origin as
// an intake declaration. Desired publication never grants native membership.
// Actual Key/Group/model membership, price and current selected Account are
// checked separately immediately before forwarding.
func GeminiProxyModelAllowed(a *Account, model, operation string) bool {
	if a == nil || a.Platform != PlatformOpenAI || a.Type != AccountTypeAPIKey || a.ID <= 0 || a.Status != StatusActive || !a.Schedulable {
		return false
	}
	if operation != "generateContent" && operation != "streamGenerateContent" && operation != "chatCompletions" {
		return false
	}
	if a.Notes == nil || *a.Notes != "gateway-provider/v1:"+a.Name {
		return false
	}
	pin, ok := a.HeaderOverrideValue("x-cliproxy-auth-id")
	if !ok || strings.TrimSpace(pin) == "" || strings.ContainsAny(pin, ",\r\n") || strings.TrimSpace(a.GetCredential("api_key")) == "" {
		return false
	}
	mapping := a.GetModelMapping()
	target, exact := mapping[model]
	if !exact || model == "" || strings.ContainsAny(model, "*?") || strings.TrimSpace(target) == "" || !IsSafeGeminiModelPathSegment(target) {
		return false
	}
	raw, err := json.Marshal(a.Extra["gateway_supply_source_v1"])
	if err != nil {
		return false
	}
	var origin struct {
		Version     int    `json:"version"`
		SourceID    string `json:"source_id"`
		PrimaryID   int64  `json:"primary_account_id"`
		Role        string `json:"role"`
		Publication *struct {
			Enabled *bool `json:"availability_enabled"`
			Input   struct {
				Models []struct {
					Name        string  `json:"name"`
					Image       bool    `json:"image"`
					Protocol    string  `json:"upstream_protocol"`
					InputPrice  float64 `json:"input_price"`
					OutputPrice float64 `json:"output_price"`
				} `json:"models"`
			} `json:"publish_input"`
		} `json:"publication"`
	}
	if json.Unmarshal(raw, &origin) != nil || origin.Version != 1 || origin.SourceID != "apikey/gemini/"+a.Name || origin.PrimaryID != a.ID || origin.Role != "primary" || origin.Publication == nil || origin.Publication.Enabled == nil || !*origin.Publication.Enabled {
		return false
	}
	matches := 0
	for _, m := range origin.Publication.Input.Models {
		if m.Name == model && !m.Image && m.Protocol == "" && m.InputPrice > 0 && m.OutputPrice > 0 && !math.IsNaN(m.InputPrice) && !math.IsNaN(m.OutputPrice) && !math.IsInf(m.InputPrice, 0) && !math.IsInf(m.OutputPrice, 0) {
			matches++
		}
	}
	if matches != 1 {
		return false
	}
	_, err = geminiProxyBaseURL(a)
	return err == nil
}

// Both observed Proxy-root and existing OpenAI /v1 account bases reach the same
// origin's native /v1beta route via the existing URL builder.
func geminiProxyBaseURL(a *Account) (string, error) {
	u, err := url.Parse(strings.TrimSpace(a.GetCredential("base_url")))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid Gemini Proxy base")
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func (s *GatewayService) ValidateGeminiProxyAccountForKey(ctx context.Context, key *APIKey, selected *Account, originalModel, selectedModel, action string) (*Account, error) {
	if s == nil || s.accountRepo == nil || selected == nil || key == nil || key.Group == nil || key.GroupID == nil || key.Group.ID != *key.GroupID || key.Group.Platform != PlatformOpenAI || key.Group.Status != StatusActive || originalModel != selectedModel {
		return nil, errors.New("Gemini Proxy original Group scope is required")
	}
	// Scheduler snapshots are not credential authority. Reload the full selected
	// Account and use this same object for URL/pin, model mapping and native usage.
	account, err := s.accountRepo.GetByID(ctx, selected.ID)
	if err != nil || !GeminiProxyModelAllowed(account, selectedModel, action) {
		return nil, errors.New("Gemini Proxy model/operation is not authorized")
	}
	if key.Group.ModelAllowlistEnabled() && !key.Group.ModelAllowlist.Allows(originalModel) {
		return nil, errors.New("Gemini Proxy model is not allowed by original Group")
	}
	member := false
	for _, id := range account.GroupIDs {
		member = member || id == *key.GroupID
	}
	for _, binding := range account.AccountGroups {
		member = member || binding.GroupID == *key.GroupID
	}
	if !member {
		return nil, ErrPrivateGroupScope
	}
	price := s.resolveChannelPricing(ctx, originalModel, key)
	if !geminiProxyExactTokenPrice(key.Group, originalModel) || price == nil || price.Source != PricingSourceGroup || price.Mode != BillingModeToken || !geminiProxyPositiveTokenPrice(price.BasePricing) {
		return nil, errors.New("exact original Group positive token pricing is required")
	}
	if len(price.Intervals) > 0 {
		if s.resolver == nil {
			return nil, errors.New("native interval pricing authority is unavailable")
		}
		for _, interval := range price.Intervals {
			// Ask the native resolver at each piecewise boundary. No copied tier
			// arithmetic or Source reference price becomes a billing authority.
			if !geminiProxyPositiveTokenPrice(s.resolver.GetIntervalPricing(price, interval.MinTokens)) {
				return nil, errors.New("native interval token price is not positive and finite")
			}
			if interval.MaxTokens != nil && !geminiProxyPositiveTokenPrice(s.resolver.GetIntervalPricing(price, *interval.MaxTokens)) {
				return nil, errors.New("native interval boundary price is not positive and finite")
			}
		}
	}
	return account, nil
}

func geminiProxyFinitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
func geminiProxyPositiveTokenPrice(price *ModelPricing) bool {
	return price != nil && geminiProxyFinitePositive(price.InputPricePerToken) && geminiProxyFinitePositive(price.OutputPricePerToken)
}
func geminiProxyExactTokenPrice(group *Group, model string) bool {
	if group == nil || strings.TrimSpace(model) == "" {
		return false
	}
	matches := 0
	for _, entry := range group.ModelPricing {
		for _, pattern := range entry.Models {
			if strings.ContainsAny(pattern, "*?") || normalizeChannelPricingModelName(pattern) != normalizeChannelPricingModelName(model) {
				continue
			}
			if entry.BillingMode != "" && entry.BillingMode != BillingModeToken || entry.InputPrice == nil || entry.OutputPrice == nil || !geminiProxyFinitePositive(*entry.InputPrice) || !geminiProxyFinitePositive(*entry.OutputPrice) {
				return false
			}
			matches++
		}
	}
	return matches == 1
}

func chatConversionUnsupportedMedia(body []byte) string {
	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	found := ""
	for _, message := range request.Messages {
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(message.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			if part.Type == "video_url" {
				return "video_url"
			}
			if part.Type == "input_audio" {
				found = "input_audio"
			}
		}
	}
	return found
}

// This scope is installed only after full current Account/Key/Group/model-price
// validation. It cannot be supplied by a client header or inferred from a URL.
type geminiProxyChatExecutionScopeKey struct{}
type geminiProxyChatExecutionScope struct {
	account     *Account
	publicModel string
	target      string
	groupID     int64
}

func withGeminiProxyChatExecutionScope(ctx context.Context, account *Account, publicModel string, groupID int64) context.Context {
	return context.WithValue(ctx, geminiProxyChatExecutionScopeKey{}, geminiProxyChatExecutionScope{
		account: account, publicModel: publicModel, groupID: groupID,
		target: normalizeOpenAIModelForUpstream(account, resolveOpenAIForwardModel(account, publicModel, "")),
	})
}

func geminiProxyChatExecutionPolicy(ctx context.Context, account *Account, route string) (bool, error) {
	scope, present := ctx.Value(geminiProxyChatExecutionScopeKey{}).(geminiProxyChatExecutionScope)
	if !present {
		return false, nil
	}
	if scope.account != account || scope.groupID <= 0 || scope.publicModel == "" || scope.target == "" || scope.target != route || !GeminiProxyModelAllowed(account, scope.publicModel, "chatCompletions") || normalizeOpenAIModelForUpstream(account, resolveOpenAIForwardModel(account, scope.publicModel, "")) != scope.target {
		return false, errors.New("validated Gemini Proxy Chat execution scope differs from outbound request")
	}
	return true, nil
}

func geminiProxyChatTextOutputOnly(body []byte) bool {
	var request struct {
		Modalities []string        `json:"modalities"`
		Audio      json.RawMessage `json:"audio"`
	}
	if json.Unmarshal(body, &request) != nil || request.Audio != nil {
		return false
	}
	for _, modality := range request.Modalities {
		if modality != "text" {
			return false
		}
	}
	return true
}
func geminiProxyChatAudioInputSupported(body []byte) bool {
	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	found := false
	for _, message := range request.Messages {
		var parts []struct {
			Type  string `json:"type"`
			Audio struct {
				Data   string `json:"data"`
				Format string `json:"format"`
			} `json:"input_audio"`
		}
		if json.Unmarshal(message.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			if part.Type == "input_audio" {
				if strings.TrimSpace(part.Audio.Data) == "" || part.Audio.Format != "wav" && part.Audio.Format != "mp3" {
					return false
				}
				found = true
			}
		}
	}
	return found
}
