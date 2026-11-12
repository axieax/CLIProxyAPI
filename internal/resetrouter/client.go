package resetrouter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	log "github.com/sirupsen/logrus"
)

const (
	claudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	codexUsageURL    = "https://chatgpt.com/backend-api/wham/usage"
	claudeUserAgent  = "claude-cli/2.1.280 (external, cli)"
	codexUserAgent   = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
	maxResponseBytes = 8 << 20
)

// Client talks to the CLIProxyAPI v8 management API.
//
// RequestTimeout bounds each management call. This is deliberate: the router
// is an out-of-process companion, not part of the proxy's upstream request
// path, and a hung call must not stall every later cycle. Upstream usage
// probes are additionally bounded by the management APICall timeout.
type Client struct {
	BaseURL        string
	ManagementKey  string
	HTTP           *http.Client
	RequestTimeout time.Duration
	// ClaudeUsageURL and CodexUsageURL override the provider usage endpoints (tests only).
	ClaudeUsageURL string
	CodexUsageURL  string
}

// ProbeError reports a failed usage probe. RetryAfter is set when the
// upstream asked the caller to wait.
type ProbeError struct {
	StatusCode int
	RetryAfter time.Duration
	Message    string
}

func (e *ProbeError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("usage probe returned HTTP %d: %s", e.StatusCode, e.Message)
	}
	return "usage probe failed: " + e.Message
}

// ListAccounts returns the Claude and Codex credentials CLIProxyAPI has loaded.
func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	body, errRequest := c.do(ctx, http.MethodGet, "/credentials", nil)
	if errRequest != nil {
		return nil, errRequest
	}
	files := ojson.ParseBytes(body).Get("files")
	if !files.IsArray() {
		return nil, errors.New("credential list response has no files array")
	}
	var accounts []Account
	for _, entry := range files.Array() {
		provider := strings.ToLower(strings.TrimSpace(entry.Get("provider").String()))
		if provider == "anthropic" {
			provider = ProviderClaude
		}
		if provider != ProviderClaude && provider != ProviderCodex {
			continue
		}
		account := Account{
			ID:             entry.Get("id").String(),
			Name:           entry.Get("name").String(),
			AuthIndex:      entry.Get("auth_index").String(),
			Provider:       provider,
			Priority:       int(entry.Get("priority").Int()),
			Disabled:       entry.Get("disabled").Bool(),
			RuntimeOnly:    entry.Get("runtime_only").Bool(),
			Unavailable:    entry.Get("unavailable").Bool(),
			ChatGPTAccount: entry.Get("id_token.chatgpt_account_id").String(),
		}
		if account.ID == "" {
			continue
		}
		if retry := entry.Get("next_retry_after"); retry.Exists() {
			account.NextRetryAfter = retry.Time()
		}
		quota := entry.Get("quota")
		if observed := quota.Get("observed_at"); observed.Exists() {
			signals := make(map[string]string)
			quota.Get("signals").ForEach(func(key, value ojson.Result) bool {
				signals[key.String()] = value.String()
				return true
			})
			account.Passive = ParsePassiveSignals(provider, signals, observed.Time())
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

// SetPriority writes a credential's routing priority.
func (c *Client) SetPriority(ctx context.Context, id string, priority int) error {
	doc := ojson.NewDocString(`{}`)
	if errSet := doc.Set("name", id); errSet != nil {
		return errSet
	}
	if errSet := doc.Set("priority", priority); errSet != nil {
		return errSet
	}
	_, errRequest := c.do(ctx, http.MethodPatch, "/credentials/fields", doc.Bytes())
	return errRequest
}

// ProbeUsage asks the provider for the account's current usage through
// CLIProxyAPI's api-call endpoint, which signs the request with the
// credential's own token. Reading usage does not consume quota.
func (c *Client) ProbeUsage(ctx context.Context, account Account, now time.Time) (*Snapshot, error) {
	headers := map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
	}
	var target string
	switch account.Provider {
	case ProviderClaude:
		target = firstNonEmpty(c.ClaudeUsageURL, claudeUsageURL)
		headers["User-Agent"] = claudeUserAgent
		headers["anthropic-beta"] = "oauth-2025-04-20"
	case ProviderCodex:
		target = firstNonEmpty(c.CodexUsageURL, codexUsageURL)
		headers["User-Agent"] = codexUserAgent
		if account.ChatGPTAccount != "" {
			headers["ChatGPT-Account-Id"] = account.ChatGPTAccount
		}
	default:
		return nil, fmt.Errorf("provider %q has no usage endpoint", account.Provider)
	}
	doc := ojson.NewDocString(`{"method":"GET","header":{}}`)
	if errSet := doc.Set("auth_index", account.AuthIndex); errSet != nil {
		return nil, errSet
	}
	if errSet := doc.Set("url", target); errSet != nil {
		return nil, errSet
	}
	for key, value := range headers {
		if errSet := doc.Set("header."+ojson.Escape(key), value); errSet != nil {
			return nil, errSet
		}
	}
	body, errRequest := c.do(ctx, http.MethodPost, "/requests/api-call", doc.Bytes())
	if errRequest != nil {
		return nil, &ProbeError{Message: errRequest.Error()}
	}
	response := ojson.ParseBytes(body)
	status := int(response.Get("status_code").Int())
	if status < 200 || status >= 300 {
		return nil, &ProbeError{
			StatusCode: status,
			RetryAfter: retryAfter(response.Get("header"), now),
			Message:    http.StatusText(status),
		}
	}
	payload := []byte(response.Get("body").String())
	var snapshot *Snapshot
	if account.Provider == ProviderClaude {
		snapshot = ParseClaudeUsage(payload, now)
	} else {
		snapshot = ParseCodexUsage(payload, now)
	}
	if snapshot == nil {
		return nil, &ProbeError{StatusCode: status, Message: "response had no recognizable quota windows"}
	}
	return snapshot, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if c.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.RequestTimeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, errRequest := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+"/v8/management"+path, reader)
	if errRequest != nil {
		return nil, errRequest
	}
	request.Header.Set("Authorization", "Bearer "+c.ManagementKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, errDo := client.Do(request)
	if errDo != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, errDo)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("reset-router: close management response body")
		}
	}()
	data, errRead := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if errRead != nil {
		return nil, fmt.Errorf("%s %s: read response: %w", method, path, errRead)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(ojson.GetBytes(data, "error").String())
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, response.StatusCode, message)
	}
	return data, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// retryAfter reads Retry-After from the api-call header map (header name to
// list of values), as seconds or an HTTP date.
func retryAfter(header ojson.Result, now time.Time) time.Duration {
	var value string
	header.ForEach(func(key, values ojson.Result) bool {
		if strings.EqualFold(key.String(), "Retry-After") {
			if values.IsArray() {
				if list := values.Array(); len(list) > 0 {
					value = list[0].String()
				}
			} else {
				value = values.String()
			}
			return false
		}
		return true
	})
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, errParse := strconv.ParseInt(value, 10, 64); errParse == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, errParse := http.ParseTime(value); errParse == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}
