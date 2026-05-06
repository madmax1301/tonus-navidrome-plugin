# Migration: Tonus 0.2.0 — Multi-Source-Resolver

This file documents what changed in Tonus 0.2.0 from this plugin's perspective. **TL;DR: no plugin code change needed**, but the backend behaviour your plugin triggers is now meaningfully different.

## What changed in Tonus

Tonus 0.2.0 introduces a **multi-source resolver** that searches every track across YouTube, SoundCloud, and Bandcamp in parallel and downloads from the best match instead of YouTube-only with manual SoundCloud opt-in.

When this plugin POSTs `/api/download` (per track from `library/missing` or `mix/discovery`), the Tonus worker no longer goes "YouTube-only first, fail on age-gate, error out". It now:

1. Pre-searches all three sources in parallel (default `youtube,soundcloud,bandcamp` via `ENABLED_SOURCES`).
2. Scores candidates with the same title/artist matching used pre-0.2.0.
3. Iterates the ranked list — if YouTube hits an age-gate, captcha, or 403, it transparently retries SoundCloud, then Bandcamp.
4. Marks the job as completed with a `used_source` field indicating which source actually delivered the file.

Practical effect for users of this plugin: failure rate from age-gated YouTube tracks drops measurably without any cookies, login, or account configuration. Tracks that used to fail silently because the plugin queued them and YouTube refused are now likely to come through SoundCloud or Bandcamp instead.

## Plugin-side actions

**Required:** none. This plugin only uses these endpoints, and none of them changed:

- `GET /api/plugin/health`
- `GET /api/plugin/library/missing`
- `GET /api/plugin/finished-tracks`
- `POST /api/plugin/sync`
- `POST /api/plugin/mix/discovery`
- `POST /api/download`

The internal source-routing happens transparently after `/api/download` enters the queue.

**Recommended (optional):** Bump the documented "minimum supported Tonus" version in this plugin's README to **`>=0.2.0`** if you want to surface the multi-source benefit explicitly. The plugin keeps working against `>=0.1.x` (no breaking change), but discovery yield is meaningfully better when the backend is `>=0.2.0`.

## Hard cutover that doesn't affect this plugin (but might affect future plugins)

Tonus 0.2.0 removed the `source` parameter on `POST /api/url/search`. The parameter is silently ignored, and a `_deprecation` field appears in the response body when callers send it. The endpoint now searches all `ENABLED_SOURCES` and merges results.

**This plugin does not call `/api/url/search`**, so this cutover has zero impact here. It's documented for completeness in case a future plugin/script you write does use that endpoint — be aware it now ignores `source` regardless of value.

## How to verify after the Tonus upgrade

After the user upgrades their Tonus container to `:0.2.0` (or `:latest` once 0.2.0 is the latest), the plugin will simply yield more successful downloads on the next cron tick. To confirm the resolver is active in the user's Tonus deployment:

```bash
# On the Tonus host:
docker compose logs tonus 2>&1 | grep "Resolver ranking"
```

You should see lines like:

```
Resolver ranking (3 candidates above min_score):
  [   youtube] score=0.912 'Daft Punk - Get Lucky' https://...
  [soundcloud] score=0.870 'Daft Punk - Get Lucky (Radio Edit)' https://soundcloud.com/...
  [  bandcamp] score=0.711 'Daft Punk Tribute' https://...
Download succeeded via youtube (score=0.912)
```

If no `Resolver ranking` line appears, either the user's Tonus is still on `<0.2.0`, or `ENABLED_SOURCES` was set to a single source.

## Reference

- Tonus repo: <https://github.com/madmax1301/tonus>
- Tonus PR introducing the resolver: see commit `712d06a` on `dev` (or the v0.2.0 release tag once cut).
- Backend env-vars driving the resolver:
  - `ENABLED_SOURCES` (default `youtube,soundcloud,bandcamp`) — order = priority on score-tie
  - `MULTI_SOURCE_TIMEOUT_S` (default `10`)
  - `MULTI_SOURCE_MIN_SCORE` (default `0.65`)
  - `MULTI_SOURCE_CANDIDATES_PER_SOURCE` (default `3`)
