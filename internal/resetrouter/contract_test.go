package resetrouter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// TestContractWithManagementAPI drives the router against CLIProxyAPI's real
// management handlers, so a change to the credential list, api-call or
// priority patch contracts breaks this test rather than production routing.
func TestContractWithManagementAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("MANAGEMENT_PASSWORD", "")
	dir := t.TempDir()
	now := time.Now().UTC()

	// Fake provider usage endpoints reached through api-call. They check that
	// CLIProxyAPI substituted the credential's own token.
	var mu sync.Mutex
	seenTokens := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenTokens[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		switch r.URL.Path {
		case "/codex":
			if r.Header.Get("ChatGPT-Account-Id") != "acct-b" {
				http.Error(w, "missing account id", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(w, `{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_at":%d},"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_at":%d}}}`,
				now.Add(2*time.Hour).Unix(), now.Add(30*time.Hour).Unix())
		case "/claude":
			_, _ = fmt.Fprintf(w, `{"five_hour":{"utilization":1,"resets_at":%q},"seven_day":{"utilization":2,"resets_at":%q}}`,
				now.Add(4*time.Hour).Format(time.RFC3339), now.Add(140*time.Hour).Format(time.RFC3339))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	register := func(auth *coreauth.Auth) {
		path := filepath.Join(dir, auth.ID)
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		auth.FileName = auth.ID
		if auth.Attributes == nil {
			auth.Attributes = map[string]string{}
		}
		auth.Attributes["path"] = path
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	// Passive data: resets in 10 hours, so it should rank first.
	register(&coreauth.Auth{
		ID: "claude-passive.json", Provider: "claude", Status: coreauth.StatusActive,
		Metadata: map[string]any{"type": "claude", "access_token": "tok-passive"},
		Quota: coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.4",
			"Anthropic-Ratelimit-Unified-7d-Reset":       fmt.Sprint(now.Add(10 * time.Hour).Unix()),
		}},
	})
	// No data: probed, resets in 140 hours.
	register(&coreauth.Auth{
		ID: "claude-idle.json", Provider: "claude", Status: coreauth.StatusActive,
		Metadata: map[string]any{"type": "claude", "access_token": "tok-idle"},
	})
	// Pinned by hand: never touched.
	register(&coreauth.Auth{
		ID: "claude-pinned.json", Provider: "claude", Status: coreauth.StatusActive,
		Metadata:   map[string]any{"type": "claude", "access_token": "tok-pinned", "priority": float64(5)},
		Attributes: map[string]string{"priority": "5"},
	})
	register(&coreauth.Auth{
		ID: "codex-b.json", Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "tok-b", "id_token": testIDToken("acct-b")},
	})

	handler := management.NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	engine := gin.New()
	v8 := engine.Group("/v8/management", func(c *gin.Context) { c.Set(management.ConfigV8ContextKey, true) })
	v8.GET("/credentials", handler.ListAuthFiles)
	v8.PATCH("/credentials/fields", handler.PatchAuthFileFields)
	v8.POST("/requests/api-call", handler.APICall)
	server := httptest.NewServer(engine)
	defer server.Close()

	client := &Client{BaseURL: server.URL, ManagementKey: "unused", ClaudeUsageURL: upstream.URL + "/claude", CodexUsageURL: upstream.URL + "/codex"}
	router := &Router{API: client, Config: DefaultConfig(), State: NewState()}
	router.Config.MaxProbesPerCycle = 10
	result, err := router.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Probed != 2 {
		t.Fatalf("expected the idle claude and codex accounts to be probed, got %d", result.Probed)
	}
	mu.Lock()
	if seenTokens["/claude"] != "Bearer tok-idle" || seenTokens["/codex"] != "Bearer tok-b" {
		t.Fatalf("api-call did not sign with the credential token: %v", seenTokens)
	}
	mu.Unlock()

	priority := func(id string) string {
		auth, ok := manager.GetByID(id)
		if !ok {
			t.Fatalf("auth %s missing", id)
		}
		return auth.Attributes["priority"]
	}
	if got := priority("claude-passive.json"); got != "1001" {
		t.Fatalf("claude-passive priority = %q, want 1001", got)
	}
	if got := priority("claude-idle.json"); got != "1000" {
		t.Fatalf("claude-idle priority = %q, want 1000", got)
	}
	if got := priority("claude-pinned.json"); got != "5" {
		t.Fatalf("pinned priority changed to %q", got)
	}
	if got := priority("codex-b.json"); got != "1000" {
		t.Fatalf("codex priority = %q, want 1000", got)
	}

	// The list endpoint reports the new priorities, so the next cycle is a no-op.
	result, err = router.Cycle(context.Background())
	if err != nil || len(result.Changes) != 0 {
		t.Fatalf("second cycle changed %d priorities (err %v)", len(result.Changes), err)
	}
}

// testIDToken builds an unsigned Codex id_token carrying the ChatGPT account ID.
func testIDToken(accountID string) string {
	encode := base64.RawURLEncoding.EncodeToString
	payload := fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_account_id":%q}}`, accountID)
	return encode([]byte(`{"alg":"none"}`)) + "." + encode([]byte(payload)) + ".sig"
}
