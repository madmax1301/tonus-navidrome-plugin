# tonus-navidrome-plugin

A [Navidrome](https://www.navidrome.org/) plugin that mirrors your [ListenBrainz](https://listenbrainz.org/) **"Created For You"** playlists into Navidrome as personal, per-user playlists — and drives the [tonus](https://github.com/madmax1301/tonus) backend to download the tracks you don't own yet.

**Status:** v0.5.0 — LB Weekly Mirror is the primary feature. Optional Genre Mixes on top. Multi-user from the ground up.

## What it does

### LB Weekly Mirror (primary)

Every ListenBrainz user gets four auto-generated playlists — *Weekly Exploration*, *Weekly Jams*, *Last Week's Exploration*, *Last Week's Jams*. This plugin **mirrors all four into Navidrome** as playlists owned by the mapped Navidrome user, under the same names, and keeps them fresh:

- Tracks you already have in your library are added to the Navidrome playlist immediately.
- Tracks you don't have are queued in tonus for download and reconciled into the playlist as they finish (rate-limited downloads can trickle in over hours).
- Each run **replaces the playlist in place** — same behaviour as ListenBrainz, the playlist just updates itself weekly. The playlist ID stays stable, so anything you copy out of it into your own playlists is unaffected.

Enabled per user via the `lb_weekly_mirror` toggle (on by default).

### Genre Mixes (optional)

On top of the weekly mirror, each user can define any number of **Genre Mixes** — Spotify-style weekly playlists per genre (`metalcore`, `hip-hop`, `shoegaze`, …). Each mix has its own two-phase cron:

- **Discovery** (default Wed 02:00) — fetches genre tracks from ListenBrainz, queues the missing ones for download.
- **Build** (default Fri 06:00, 48 h later) — builds/replaces the Subsonic playlist from whatever finished downloading in the meantime.

A `discovery_ratio` (default 0.4 = 40 % new / 60 % familiar) controls the new-vs-known mix. Same replace-in-place pattern — favourites you copy elsewhere are safe.

## What it does NOT do

- The plugin downloads nothing itself — that's tonus's job. It's a thin **trigger + Subsonic-playlist scheduler**.
- No live queue inside Navidrome. The plugin UI is limited to its settings page; use tonus's own web UI for live download status.
- No listen-tracking / scrobble ingestion — it reads ListenBrainz's *output* (the Created-For-You playlists), not your raw listens.

## Architecture (one paragraph)

The plugin is a **thin trigger**; the backend is the brain. A TaskQueue with concurrency 1 receives, per Navidrome user per cron tick, one *discovery* task and one *reconcile* task per playlist slot. Discovery fires `POST /api/plugin/lbweekly/discovery` (or `/api/plugin/mix/discovery` for Genre Mixes) — tonus returns the in-library tracks synchronously and queues the missing ones for download in the background. Reconcile pulls `GET /api/plugin/finished-tracks` for the user, merges those with the persisted in-library IDs, and pushes the result into a Subsonic playlist via `host.SubsonicAPICall("/rest/createPlaylist?u=USERNAME")` — Navidrome injects the user's auth automatically because the plugin holds the `users` permission. tonus resolves Subsonic track IDs server-side (admin-auth `search3`, cached in the job payload) so the reconcile stays well under the 30 s plugin-callback timeout.

## Requirements

- **Navidrome ≥ 0.60** (Extism-based plugin system).
- A reachable **tonus** backend (≥ v0.6.1) exposing `/api/plugin/health`, `/api/plugin/lbweekly/discovery`, `/api/plugin/mix/discovery`, `/api/plugin/finished-tracks`, `/api/plugin/sync-status`.
- A public **ListenBrainz** username per Navidrome user you want to mirror (the Created-For-You playlists must be publicly readable).
- For building: [TinyGo](https://tinygo.org/getting-started/install/) `≥ 0.40` and [Go](https://golang.org/dl/) `≥ 1.23`. The Makefile falls back to plain `go` if TinyGo is missing, but the resulting WASM is much larger.

## Install

### Option 1 — Pre-built release (recommended)

Grab the latest `tonus-navidrome-plugin.ndp` from the [GitHub Releases](https://github.com/madmax1301/tonus-navidrome-plugin/releases) page. Each release is built by the `release.yml` GitHub Action whenever a `v*.*.*` tag is pushed.

### Option 2 — Build from source

```bash
git clone https://github.com/madmax1301/tonus-navidrome-plugin.git
cd tonus-navidrome-plugin
make package    # → tonus-navidrome-plugin.ndp
```

### Install into Navidrome

1. Copy `tonus-navidrome-plugin.ndp` into your Navidrome `plugins/` folder (or upload it via the admin **Plugins** page), then Rescan / restart.
2. Enable the plugin. Required permissions:
   - **HTTP** to your tonus host (manifest uses `"*"` — Navidrome may prompt you to confirm).
   - **Scheduler** for the cron registration.
   - **KVStore** for per-user status + in-library-track persistence.
   - **SubsonicAPI** for playlist create/replace.
   - **Users** — flip on `Allow all users`, or pick the users you want mirrored.
   - **TaskQueue** for the async per-user pipelines.
3. Open the plugin settings and fill in:
   - **Connection**: `tonus_url` (e.g. `http://192.168.1.200:8000`) and `tonus_token` (a tonus **PAT** or the legacy `TONUS_API_TOKEN`; leave empty if tonus runs tokenless).
   - **Schedule**: the global `cron_expression` for the weekly mirror (default `0 7 * * *`).
   - **Users**: click `+` per Navidrome user, set `navidrome_username` + `listenbrainz_username`, leave `lb_weekly_mirror` on, and optionally add one or more `mixes`.

## How it works under the hood

1. **OnInit** registers the global cron with `host.SchedulerScheduleRecurring(...)`, plus one discovery- and one build-cron per configured Genre Mix, and creates the TaskQueue.
2. **OnCallback** (cron tick) reads the user-mapping array and enqueues the relevant tasks — for the weekly mirror, a discovery + reconcile pair for each of the four LB slots per user. Returns in under a second.
3. **OnTaskExecute** runs one task:
   - `lbweekly-discovery` / `mix-discovery`: health-check + discovery POST. tonus returns the in-library tracks (persisted to `KVStore`) and queues missing tracks for download.
   - `reconcile` / `mix-build`: `GET /api/plugin/finished-tracks`, merge with the persisted in-library IDs, dedupe, and `findOrCreate` + replace the Subsonic playlist.
4. **Status**: per-user state (timestamps, counts, last error) is persisted in `KVStore`. tonus's web UI also shows a global status card.

## Cold-start note

The first time the plugin authenticates as a fresh Navidrome user you may see one `module closed with context deadline exceeded` warning — that's Navidrome registering a new player context for that user. The built-in TaskQueue retry (`MaxRetries=3, BackoffMs=30000`) succeeds within seconds. After 1–2 successful reconciles the cold start is over.

## Architecture decisions worth knowing

- **Plugin = trigger, backend = brain.** All discovery + download stay in tonus where they're easy to debug. The plugin knows only a handful of tonus endpoints + Subsonic.
- **Reconcile pattern, not polling.** Tracks queued today may not finish for hours. Each run reconciles again, idempotently picking up newly finished tracks, never duplicating.
- **In-library IDs persisted in KVStore.** The reconcile merges tracks you already own (returned by discovery) with freshly downloaded ones — so a mirror is complete even when nothing new had to be downloaded.
- **Per-user playlist ownership** via `host.SubsonicAPICall("?u=USERNAME")`. No user passwords stored anywhere — Navidrome injects auth from the plugin's `users` permission.
- **ListenBrainz `createdfor` returns metadata only.** The actual tracks live behind `/playlist/<mbid>`; tonus does the two-step fetch server-side so the plugin never sees the LB API directly.

## License

MIT — see [LICENSE](./LICENSE).
