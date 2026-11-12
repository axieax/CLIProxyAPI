# reset-router: spend the quota that resets soonest

`reset-router` runs alongside CLIProxyAPI and keeps Claude and Codex OAuth
accounts ordered so the proxy uses up the weekly quota that resets first.
Quota still left on an account when its week resets is lost. Spending the
soonest-expiring quota first wastes less of it.

The router only reads quota and writes each account's routing `priority`
through the v8 management API. It does not change the proxy and it never
redeems reset credits.

## How it works

CLIProxyAPI routes each request to the highest available priority. Every 5
minutes the router:

1. Lists credentials (`GET /v8/management/credentials`). For each account this
   includes the quota headers CLIProxyAPI recorded from its latest real
   response, so accounts serving traffic need no extra calls.
2. Probes the few accounts whose data is missing or stale. It reads the
   provider's usage endpoint through `POST /v8/management/requests/api-call`,
   which signs the call with the account's own token. This is the same request
   the management UI's quota refresh makes, and reading usage does not consume
   quota:
   - Claude: `GET https://api.anthropic.com/api/oauth/usage`
   - Codex: `GET https://chatgpt.com/backend-api/wham/usage`
3. Ranks each provider's accounts and writes the priorities that changed
   (`PATCH /v8/management/credentials/fields`).

### Ranking

From most preferred to least:

1. **Quota available, weekly reset known.** The earliest weekly reset comes
   first. Resets are compared to the minute, and the 5h reset breaks ties.
2. **Quota available, weekly reset unknown.** The earliest 5h reset comes first.
3. **Blocked:** an exhausted 5h or weekly window, or a proxy cooldown. These
   accounts are ordered by when they recover.

The top account takes all traffic until it hits a limit. CLIProxyAPI then
cools it down and moves to the next account by itself, without waiting for
the router.

Claude's model-specific weekly windows (Opus, Sonnet, Fable) are not used for
ranking. Only the general `seven_day` and `five_hour` windows count.

### Keeping upstream calls rare

- Passive data comes first. An account is probed only when its newest data
  (passive or probed) is older than `--fresh-for` (45m, plus 0-20% per-account
  jitter) or a reset time in it has passed.
- At most `--max-probes-per-cycle` (2) probes run per cycle, stalest first.
- Each account is probed at most once per `--min-probe-gap` (15m).
- An account with an exhausted window is not probed again until that window's
  reset, because its usage cannot drop before then.
- A failed probe backs that account off exponentially, from 5m up to 6h, and
  honours `Retry-After`.
- Probe history is saved to `--state-file`, so restarts do not trigger a burst
  of probes.

With about ten accounts this is a handful of usage calls per hour, mostly for
idle accounts.

### Failure behaviour

- If the credential list cannot be read, the cycle changes nothing.
- If no account of a provider has any quota data yet, that provider's
  priorities are left alone.
- A failed priority write is logged and retried on the next cycle.
- If the router stops, the last priorities stay in place. CLIProxyAPI's own
  cooldown and failover keep working either way.

## Which accounts it manages

Claude and Codex OAuth credentials that are enabled and backed by a file,
whose priority is either `0` or inside the managed range
`[--base-priority, --base-priority + --managed-span)` (default `[1000, 2000)`).
Any other priority counts as pinned by hand and is left alone:

- To keep an account in front of all managed ones, give it a priority of
  `2000` or more.
- To keep it as a fallback, give it a priority between `1` and `999`.
- Disabled accounts and API-key credentials are never touched.

## Setup

1. Enable the management API in `config.yaml` (localhost access is enough):

   ```yaml
   remote-management:
     secret-key: "<choose a key>"
   ```

2. Build:

   ```bash
   go build -o reset-router ./cmd/reset-router
   ```

3. Preview the order without writing anything:

   ```bash
   CLIPROXY_MANAGEMENT_KEY=<key> ./reset-router --once --dry-run --verbose
   ```

4. Run it as a service. A launchd example is in
   [`examples/reset-router/com.cliproxyapi.reset-router.plist`](../examples/reset-router/com.cliproxyapi.reset-router.plist).
   Put the key in a file with mode `600` and pass `--management-key-file`.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--base-url` | `http://127.0.0.1:8317` | CLIProxyAPI address |
| `--management-key-file` | `$CLIPROXY_MANAGEMENT_KEY` | file holding the management key |
| `--providers` | `claude,codex` | providers to manage |
| `--interval` | `5m` | time between cycles |
| `--fresh-for` | `45m` | probe when data is older than this |
| `--min-probe-gap` | `15m` | minimum time between probes of one account |
| `--max-probes-per-cycle` | `2` | probe cap per cycle |
| `--base-priority` / `--managed-span` | `1000` / `1000` | managed priority range |
| `--state-file` | `~/.local/state/cliproxy-reset-router/state.json` | probe history |
| `--once`, `--dry-run`, `--verbose` | off | one cycle; no writes; debug ranking logs |

## Limits

- CLIProxyAPIHome mode disables the local management API, so the router cannot
  run there.
- Ranking assumes each account has its own weekly window. Accounts that share
  one upstream subscription are ranked as if they were separate.
