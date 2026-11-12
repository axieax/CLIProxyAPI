// Command reset-router keeps CLIProxyAPI spending the Claude and Codex quota
// that resets soonest. Each cycle it reads every account's quota through the
// v8 management API, ranks the accounts, and writes their routing priority.
// CLIProxyAPI routes to the highest available priority, so the account whose
// weekly window resets first is drained first, and its own cooldown moves
// traffic to the next account when one runs out.
//
// Usage:
//
//	CLIPROXY_MANAGEMENT_KEY=... go run ./cmd/reset-router [flags]
//
// See docs/reset-router.md for setup and behaviour.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/resetrouter"
	log "github.com/sirupsen/logrus"
)

func main() {
	if errRun := run(); errRun != nil {
		log.WithError(errRun).Error("reset-router: exiting")
		os.Exit(1)
	}
}

func run() error {
	defaults := resetrouter.DefaultConfig()
	cfg := defaults

	baseURL := flag.String("base-url", "http://127.0.0.1:8317", "CLIProxyAPI base URL")
	keyFile := flag.String("management-key-file", "", "file holding the management key (default: $CLIPROXY_MANAGEMENT_KEY)")
	statePath := flag.String("state-file", defaultStatePath(), "where probe history is kept across restarts")
	providers := flag.String("providers", strings.Join(defaults.Providers, ","), "comma-separated providers to manage (claude, codex)")
	interval := flag.Duration("interval", 5*time.Minute, "time between ranking cycles")
	requestTimeout := flag.Duration("request-timeout", 90*time.Second, "timeout for one management API call")
	once := flag.Bool("once", false, "run one cycle and exit")
	verbose := flag.Bool("verbose", false, "log debug detail")
	flag.BoolVar(&cfg.DryRun, "dry-run", false, "log planned priorities without writing them")
	flag.IntVar(&cfg.BasePriority, "base-priority", defaults.BasePriority, "lowest priority the router assigns")
	flag.IntVar(&cfg.ManagedSpan, "managed-span", defaults.ManagedSpan, "size of the managed priority range starting at base-priority")
	flag.DurationVar(&cfg.FreshFor, "fresh-for", defaults.FreshFor, "probe an account when its quota data is older than this")
	flag.DurationVar(&cfg.MinProbeGap, "min-probe-gap", defaults.MinProbeGap, "minimum time between probes of one account")
	flag.IntVar(&cfg.MaxProbesPerCycle, "max-probes-per-cycle", defaults.MaxProbesPerCycle, "maximum usage probes per cycle")
	flag.Parse()

	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	cfg.Providers = nil
	for _, provider := range strings.Split(*providers, ",") {
		provider = strings.ToLower(strings.TrimSpace(provider))
		switch provider {
		case "":
		case resetrouter.ProviderClaude, resetrouter.ProviderCodex:
			cfg.Providers = append(cfg.Providers, provider)
		default:
			return fmt.Errorf("unsupported provider %q", provider)
		}
	}
	if len(cfg.Providers) == 0 {
		return errors.New("no providers selected")
	}
	if cfg.ManagedSpan <= 0 || cfg.MaxProbesPerCycle < 0 || *interval <= 0 {
		return errors.New("managed-span and interval must be positive and max-probes-per-cycle non-negative")
	}

	key, errKey := managementKey(*keyFile)
	if errKey != nil {
		return errKey
	}
	state, errState := resetrouter.LoadState(*statePath)
	if errState != nil {
		return errState
	}
	router := &resetrouter.Router{
		API: &resetrouter.Client{
			BaseURL:        *baseURL,
			ManagementKey:  key,
			HTTP:           &http.Client{},
			RequestTimeout: *requestTimeout,
		},
		Config: cfg,
		State:  state,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for {
		errCycle := cycle(ctx, router, *statePath)
		if *once {
			return errCycle
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

// cycle runs one ranking pass and logs it. It returns the pass's error so
// --once can exit non-zero; the long-running loop just retries next interval.
func cycle(ctx context.Context, router *resetrouter.Router, statePath string) error {
	result, errCycle := router.Cycle(ctx)
	if errCycle != nil {
		log.WithError(errCycle).Warn("reset-router: cycle incomplete; priorities it could not write stay as they were")
	}
	for provider, ranked := range result.Ranked {
		for index, entry := range ranked {
			log.WithFields(log.Fields{
				"provider":     provider,
				"rank":         index + 1,
				"credential":   entry.Account.Name,
				"tier":         entry.Tier.String(),
				"weekly_reset": formatReset(entry.Quota.Weekly),
				"weekly_used":  formatUsed(entry.Quota.Weekly),
				"5h_reset":     formatReset(entry.Quota.FiveHour),
				"5h_used":      formatUsed(entry.Quota.FiveHour),
				"priority":     entry.Desired,
			}).Debug("reset-router: ranking")
		}
	}
	for _, change := range result.Changes {
		verb := "set"
		if !change.Applied {
			verb = "would set"
		}
		log.WithFields(log.Fields{
			"provider":     change.Account.Provider,
			"credential":   change.Account.Name,
			"from":         change.Previous,
			"to":           change.Desired,
			"tier":         change.Tier.String(),
			"weekly_reset": formatReset(change.Quota.Weekly),
		}).Infof("reset-router: %s priority", verb)
	}
	if errSave := router.State.Save(statePath); errSave != nil {
		log.WithError(errSave).Warn("reset-router: save state failed")
	}
	return errCycle
}

func managementKey(path string) (string, error) {
	if path != "" {
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			return "", fmt.Errorf("read management key file: %w", errRead)
		}
		if key := strings.TrimSpace(string(data)); key != "" {
			return key, nil
		}
		return "", fmt.Errorf("management key file %s is empty", path)
	}
	if key := strings.TrimSpace(os.Getenv("CLIPROXY_MANAGEMENT_KEY")); key != "" {
		return key, nil
	}
	return "", errors.New("set CLIPROXY_MANAGEMENT_KEY or pass --management-key-file")
}

func defaultStatePath() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "cliproxy-reset-router", "state.json")
	}
	if home, errHome := os.UserHomeDir(); errHome == nil {
		return filepath.Join(home, ".local", "state", "cliproxy-reset-router", "state.json")
	}
	return "reset-router-state.json"
}

func formatReset(window *resetrouter.Window) string {
	if window == nil || window.ResetAt.IsZero() {
		return "unknown"
	}
	return window.ResetAt.Local().Format(time.RFC3339)
}

func formatUsed(window *resetrouter.Window) string {
	if window == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.0f%%", window.UsedPercent)
}
