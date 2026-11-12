package resetrouter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

const listResponse = `{"observed_at":"2026-11-13T12:00:00Z","files":[
 {"id":"claude-a.json","name":"claude-a.json","auth_index":"1","provider":"claude","priority":1001,"disabled":false,"runtime_only":false,"unavailable":false,
  "quota":{"observed_at":"2026-11-13T11:30:00Z","signals":{"Anthropic-Ratelimit-Unified-7d-Utilization":"0.5","Anthropic-Ratelimit-Unified-7d-Reset":"1794873600"}}},
 {"id":"codex-b.json","name":"codex-b.json","auth_index":"2","provider":"codex","unavailable":true,"next_retry_after":"2026-11-13T13:00:00Z",
  "id_token":{"chatgpt_account_id":"acct-123"},"quota":{"signals":{}}},
 {"id":"gemini.json","name":"gemini.json","auth_index":"3","provider":"gemini-cli"}
]}`

func TestClientListAccounts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v8/management/credentials" || r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, `{"error":"bad"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, listResponse)
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, ManagementKey: "secret"}
	accounts, err := client.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("expected claude and codex only, got %+v", accounts)
	}
	claude, codex := accounts[0], accounts[1]
	if claude.Priority != 1001 || claude.AuthIndex != "1" || claude.Passive == nil || claude.Passive.Weekly.UsedPercent != 50 {
		t.Fatalf("claude = %+v passive %+v", claude, claude.Passive)
	}
	if !codex.Unavailable || codex.NextRetryAfter.Hour() != 13 || codex.ChatGPTAccount != "acct-123" || codex.Passive != nil {
		t.Fatalf("codex = %+v", codex)
	}

	client.ManagementKey = "wrong"
	if _, err := client.ListAccounts(context.Background()); err == nil {
		t.Fatal("expected an auth error")
	}
}

func TestClientSetPriority(t *testing.T) {
	var got []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/v8/management/credentials/fields" {
			http.NotFound(w, r)
			return
		}
		got, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, ManagementKey: "k"}
	if err := client.SetPriority(context.Background(), "claude-a.json", 1003); err != nil {
		t.Fatal(err)
	}
	if ojson.GetBytes(got, "name").String() != "claude-a.json" || ojson.GetBytes(got, "priority").Int() != 1003 {
		t.Fatalf("body = %s", got)
	}
}

func TestClientProbeUsage(t *testing.T) {
	var request []byte
	reply := `{"status_code":200,"header":{},"body":"{\"rate_limit\":{\"primary_window\":{\"used_percent\":20,\"limit_window_seconds\":18000,\"reset_at\":1794578400},\"secondary_window\":{\"used_percent\":70,\"limit_window_seconds\":604800,\"reset_at\":1794873600}}}"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v8/management/requests/api-call" {
			http.NotFound(w, r)
			return
		}
		request, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, reply)
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, ManagementKey: "k"}
	account := Account{ID: "codex-b.json", AuthIndex: "2", Provider: ProviderCodex, ChatGPTAccount: "acct-123"}
	snap, err := client.ProbeUsage(context.Background(), account, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Weekly.UsedPercent != 70 || snap.FiveHour.UsedPercent != 20 {
		t.Fatalf("snapshot = %+v", snap)
	}
	checks := map[string]string{
		"auth_index":                "2",
		"method":                    "GET",
		"url":                       codexUsageURL,
		"header.Authorization":      "Bearer $TOKEN$",
		"header.ChatGPT-Account-Id": "acct-123",
	}
	for path, want := range checks {
		if got := ojson.GetBytes(request, path).String(); got != want {
			t.Fatalf("%s = %q, want %q (body %s)", path, got, want, request)
		}
	}

	account = Account{ID: "claude-a.json", AuthIndex: "1", Provider: ProviderClaude}
	reply = `{"status_code":200,"header":{},"body":"{\"seven_day\":{\"utilization\":10,\"resets_at\":\"2026-11-17T00:00:00Z\"}}"}`
	if _, err := client.ProbeUsage(context.Background(), account, testNow); err != nil {
		t.Fatal(err)
	}
	if ojson.GetBytes(request, "url").String() != claudeUsageURL || ojson.GetBytes(request, "header.anthropic-beta").String() != "oauth-2025-04-20" {
		t.Fatalf("claude request = %s", request)
	}
}

func TestClientProbeUsageUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status_code":429,"header":{"Retry-After":["120"]},"body":"slow down"}`)
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, ManagementKey: "k"}
	_, err := client.ProbeUsage(context.Background(), Account{AuthIndex: "1", Provider: ProviderClaude}, testNow)
	var probeErr *ProbeError
	if !errors.As(err, &probeErr) || probeErr.StatusCode != 429 || probeErr.RetryAfter.Seconds() != 120 {
		t.Fatalf("err = %v", err)
	}
}
