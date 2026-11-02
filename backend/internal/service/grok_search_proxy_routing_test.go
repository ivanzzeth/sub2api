//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

type grokSearchRoutingUpstream struct {
	HTTPUpstream
	t           *testing.T
	wantAccount *Account
	wantBody    map[string]any
	wantModel   string
	calls       int
}

func (u *grokSearchRoutingUpstream) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	u.t.Helper()
	u.calls++
	require.Equal(u.t, http.MethodPost, req.Method)
	require.Equal(u.t, "https://relay.example/v1/responses", req.URL.String())
	require.Equal(u.t, "Bearer "+u.wantAccount.GetCredential("api_key"), req.Header.Get("Authorization"))
	var pins []string
	for name, values := range req.Header {
		if strings.EqualFold(name, "X-CLIProxy-Auth-ID") {
			pins = append(pins, values...)
		}
	}
	require.Equal(u.t, []string{u.wantAccount.GetCredential("test_pin")}, pins, "native raw header casing must preserve exactly one selected-Account pin")
	require.Equal(u.t, u.wantAccount.ID, accountID)
	require.Equal(u.t, u.wantAccount.Concurrency, concurrency)
	require.Equal(u.t, u.wantAccount.Proxy.URL(), proxyURL)
	body, err := io.ReadAll(req.Body)
	require.NoError(u.t, err)
	var got map[string]any
	require.NoError(u.t, json.Unmarshal(body, &got))
	require.Equal(u.t, u.wantModel, got["model"], "selected Account mapping must be applied; no foreign target fallback")
	delete(got, "model")
	require.Equal(u.t, u.wantBody, got, "only model may change; all tools/include/body controls must survive")
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","output":[],"usage":{"input_tokens":12,"output_tokens":8}}`))}, nil
}

func TestDoGrokNativeResponsesJSONSelectedAccountMapping(t *testing.T) {
	cases := []struct {
		name   string
		source string
		model  *string
		mapped bool
		want   string
	}{
		{name: "A_explicit", source: "a", model: searchRoutingString("logical-search"), mapped: true, want: "source-a/grok-search-upstream"},
		{name: "B_explicit", source: "b", model: searchRoutingString("logical-search"), mapped: true, want: "source-b/grok-search-upstream"},
		{name: "A_omitted_default", source: "a", mapped: true, want: "source-a/grok-search-default"},
		{name: "B_omitted_default", source: "b", mapped: true, want: "source-b/grok-search-default"},
		{name: "A_blank_default", source: "a", model: searchRoutingString("  "), mapped: true, want: "source-a/grok-search-default"},
		{name: "unmapped_explicit", source: "a", model: searchRoutingString("logical-search"), want: "logical-search"},
		{name: "unmapped_default", source: "a", want: xai.DefaultTextModel},
		{name: "A_exact_empty_default", source: "a", model: searchRoutingString(""), mapped: true, want: "source-a/grok-search-default"},
		{name: "A_padded_explicit", source: "a", model: searchRoutingString("  logical-search  "), mapped: true, want: "source-a/grok-search-upstream"},
		{name: "native_default_alias", source: "a", model: searchRoutingString("grok-build"), want: "grok-build-0.1"},
	}
	for _, tc := range cases {
		for _, tool := range []string{"web_search", "x_search"} {
			t.Run(tc.name+"/"+tool, func(t *testing.T) {
				proxyID := int64(91)
				account := &Account{ID: 17, Platform: PlatformGrok, Type: AccountTypeAPIKey, Concurrency: 3, ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Protocol: "http", Host: "proxy.example", Port: 8080}, Credentials: map[string]any{"api_key": "source-" + tc.source + "-private", "base_url": "https://relay.example/v1", "test_pin": "native-" + tc.source, "header_override_enabled": true, "header_overrides": map[string]any{"x-cliproxy-auth-id": "native-" + tc.source}}}
				if tc.mapped {
					account.Credentials["model_mapping"] = map[string]any{"logical-search": "source-" + tc.source + "/grok-search-upstream", xai.DefaultTextModel: "source-" + tc.source + "/grok-search-default"}
				}
				body := map[string]any{"input": "search fixture", "tools": []any{map[string]any{"type": tool}}, "include": []any{tool + "_call.action.sources"}, "tool_choice": "required", "store": false, "stream": false, "parallel_tool_calls": true, "metadata": map[string]any{"logical_billing_model": "logical-search"}}
				if tc.model != nil {
					body["model"] = *tc.model
				}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				original := bytes.Clone(raw)
				delete(body, "model")
				upstream := &grokSearchRoutingUpstream{t: t, wantAccount: account, wantBody: body, wantModel: tc.want}
				svc := &GatewayService{httpUpstream: upstream}
				got, err := svc.DoGrokNativeResponsesJSON(context.Background(), account, raw)
				require.NoError(t, err)
				require.JSONEq(t, `{"id":"fixture","output":[],"usage":{"input_tokens":12,"output_tokens":8}}`, string(got))
				require.Equal(t, 1, upstream.calls)
				require.Equal(t, original, raw, "caller's logical request must remain unchanged for billing")
			})
		}
	}
}

func searchRoutingString(s string) *string { return &s }

// Empty selected-Account targets are configuration errors, never a reason to
// send an empty model or retry another Source.
type grokSearchNoWireUpstream struct {
	HTTPUpstream
	calls int
}

func (u *grokSearchNoWireUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
}

func TestDoGrokNativeResponsesJSONRejectsEmptyMappedTargetBeforeWire(t *testing.T) {
	for _, entry := range []struct {
		name       string
		model      *string
		mappingKey string
	}{
		{name: "exact", model: searchRoutingString("logical-search"), mappingKey: "logical-search"},
		{name: "default", mappingKey: xai.DefaultTextModel},
		{name: "wildcard", model: searchRoutingString("logical-search"), mappingKey: "*"},
	} {
		for _, target := range []string{"", "  "} {
			for _, tool := range []string{"web_search", "x_search"} {
				t.Run(entry.name+"/"+tool+"/target="+target, func(t *testing.T) {
					account := &Account{ID: 17, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "server-fixture", "base_url": "https://relay.example/v1", "model_mapping": map[string]any{entry.mappingKey: target}}}
					body := map[string]any{"input": "fixture", "tools": []any{map[string]any{"type": tool}}, "include": []any{tool + "_call.action.sources"}}
					if entry.model != nil {
						body["model"] = *entry.model
					}
					raw, err := json.Marshal(body)
					require.NoError(t, err)
					original := bytes.Clone(raw)
					upstream := &grokSearchNoWireUpstream{}
					svc := &GatewayService{httpUpstream: upstream}
					got, err := svc.DoGrokNativeResponsesJSON(context.Background(), account, raw)
					require.ErrorContains(t, err, "grok responses model mapping is empty")
					require.Nil(t, got)
					require.Zero(t, upstream.calls, "invalid mapping must reject before any Provider dispatch")
					require.Equal(t, original, raw)
				})
			}
		}
	}
}
