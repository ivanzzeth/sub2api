package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
)

// SeedreamImageProfileError is a pre-write qualification failure, never an
// unknown provider result. Its fields contain no account or credential values.
type SeedreamImageProfileError struct{}

func (*SeedreamImageProfileError) Error() string {
	return "explicit compatible account image profile and priced Seedream operation required"
}

const OpenAIImagesCapabilitySeedream OpenAIImagesCapability = "images-ark-seedream"

func seedreamImageModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(m, "doubao-seedream-") || strings.HasPrefix(m, "seedream-")
}

// Only recognition strips the Source prefix. Profile lookup NEVER strips it.
func seedreamMappedModel(model string) bool {
	_, tail, ok := strings.Cut(strings.TrimSpace(model), "/")
	if ok {
		return seedreamImageModel(tail)
	}
	return seedreamImageModel(model)
}
func seedreamProfileValue(profile string) bool {
	return profile == "ark-agent-plan-seedream-5pro-v1" || profile == "ark-agent-plan-seedream-5flash-v1"
}
func accountSeedreamProfile(a *Account, mapped string) string {
	if a == nil || a.Type != AccountTypeAPIKey || !a.IsOpenAI() {
		return ""
	}
	var profiles map[string]any
	switch value := a.Credentials["image_profiles"].(type) {
	case map[string]any:
		profiles = value
	case map[string]string:
		profiles = map[string]any{}
		for k, v := range value {
			profiles[k] = v
		}
	}
	profile, _ := profiles[mapped].(string)
	if !seedreamProfileValue(profile) {
		return ""
	}
	return profile
}

// Inspect ORIGINAL customer JSON after trusted profile selection, before any
// shared Proxy handler removes stream or rewrites model. Generic paths never call it.
func seedreamOriginalFields(body []byte) (map[string]json.RawMessage, error) {
	fields := make(map[string]json.RawMessage)
	decoder := json.NewDecoder(bytes.NewReader(body))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, &SeedreamImageProfileError{}
	}
	for decoder.More() {
		token, readErr := decoder.Token()
		key, ok := token.(string)
		if readErr != nil || !ok {
			return nil, &SeedreamImageProfileError{}
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, &SeedreamImageProfileError{}
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, &SeedreamImageProfileError{}
		}
		fields[key] = value
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return nil, &SeedreamImageProfileError{}
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, &SeedreamImageProfileError{}
	}
	for _, key := range []string{"stream", "watermark"} {
		if raw, ok := fields[key]; ok {
			var value *bool
			if json.Unmarshal(raw, &value) != nil || value == nil || (key == "stream" && *value) {
				return nil, &SeedreamImageProfileError{}
			}
		}
	}
	return fields, nil
}
func qualifySeedreamImageProfile(a *Account, parsed *OpenAIImagesRequest, channelModel string) error {
	// Reset on EVERY selected-account boundary: no stale result crosses a retry.
	parsed.SeedreamProfile = ""
	selected := strings.TrimSpace(channelModel)
	if selected == "" {
		selected = parsed.Model
	}
	if a == nil {
		return &SeedreamImageProfileError{}
	}
	final := a.GetMappedModel(selected)
	profile := accountSeedreamProfile(a, final)
	explicit := false
	switch values := a.Credentials["image_profiles"].(type) {
	case map[string]any:
		_, explicit = values[final]
	case map[string]string:
		_, explicit = values[final]
	}
	required := seedreamMappedModel(parsed.Model) || seedreamMappedModel(selected) || seedreamMappedModel(final) || explicit
	if !required {
		return nil
	}
	if profile == "" || !seedreamMappedModel(final) || parsed.IsEdits() || parsed.Stream || parsed.Multipart || parsed.N != 1 || (parsed.Size != "1K" && parsed.Size != "2K") {
		return &SeedreamImageProfileError{}
	}
	fields, rawErr := seedreamOriginalFields(parsed.Body)
	if rawErr != nil {
		return rawErr
	}
	if n, ok := fields["n"]; ok && strings.TrimSpace(string(n)) != "1" {
		return &SeedreamImageProfileError{}
	}
	parsed.SeedreamProfile = profile // trusted Account+EXACT final route, never body/header
	parsed.SizeTier = parsed.Size
	return nil
}
func (a *Account) hasSeedreamProfile() bool {
	if a == nil || a.Type != AccountTypeAPIKey || !a.IsOpenAI() {
		return false
	}
	switch profiles := a.Credentials["image_profiles"].(type) {
	case map[string]any:
		for key := range profiles {
			if accountSeedreamProfile(a, key) != "" {
				return true
			}
		}
	case map[string]string:
		for key := range profiles {
			if accountSeedreamProfile(a, key) != "" {
				return true
			}
		}
	}
	return false
}
func validateSeedreamMetering(body []byte, parsed *OpenAIImagesRequest) error {
	var data struct {
		Data []struct {
			URL  string `json:"url"`
			B64  string `json:"b64_json"`
			Size string `json:"size"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &data) != nil || len(data.Data) != 1 {
		return fmt.Errorf("unverified single image output")
	}
	row := data.Data[0]
	if (row.URL == "") == (row.B64 == "") {
		return fmt.Errorf("missing single image")
	}
	tier := map[string]string{"1K": "1K", "1024x1024": "1K", "2K": "2K", "2048x2048": "2K"}[row.Size]
	if tier == "" || tier != parsed.Size {
		return fmt.Errorf("unverified image tier; no 1.5K fallback")
	}
	return nil
}

// SeedreamImagePriceError is a prewrite refusal. The original customer's native
// Group is the price authority; nil is not permission to use generic fallback.
type SeedreamImagePriceError struct{}

func (*SeedreamImagePriceError) Error() string {
	return "Explicit positive group price required for the requested Seedream image tier"
}
func qualifySeedreamImagePrice(c interface{ Get(string) (any, bool) }, parsed *OpenAIImagesRequest) error {
	if parsed.SeedreamProfile == "" {
		return nil
	}
	key := getAPIKeyFromContext(c)
	if key == nil || key.Group == nil {
		return &SeedreamImagePriceError{}
	}
	price := key.Group.GetImagePrice(parsed.Size)
	if price == nil || math.IsNaN(*price) || math.IsInf(*price, 0) || *price <= 0 {
		return &SeedreamImagePriceError{}
	}
	return nil
}
