// tonus-navidrome-plugin: Navidrome plugin that mirrors ListenBrainz Weekly
// playlists and builds genre-mix playlists for Navidrome users.
//
// Architecture:
//   1. OnInit registers the global cron ("tonus-sync") and per-(user,mix)
//      crons, and creates the TaskQueue. No work yet.
//   2. OnCallback (every cron tick) reads the config (tonus URL/token,
//      schedule, user-mapping array) and enqueues jobs. For the global cron
//      it enqueues one (Discovery + Reconcile) pair per LB-Weekly slot per
//      user with LbWeeklyMirror==true. Returns in <1s — well below the
//      plugin-callback timeout.
//   3. OnTaskExecute runs each task asynchronously. LbWeeklyDiscovery hits
//      /api/plugin/lbweekly/discovery. Reconcile pulls
//      /api/plugin/finished-tracks and pushes the matched Subsonic IDs into
//      the user-owned playlist via host.SubsonicAPICall (which authenticates
//      as that user automatically thanks to the `users` permission).
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/lifecycle"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
	"github.com/navidrome/navidrome/plugins/pdk/go/scheduler"
	"github.com/navidrome/navidrome/plugins/pdk/go/taskworker"

	"tonus-navidrome-plugin/internal/tasks"
)

// scheduleName is the cron-job key for the global default-discovery cron.
// Mix-cron keys follow the pattern "mix-discovery/<user>/<mix>" and
// "mix-build/<user>/<mix>" — see scheduleKeyFor / parseMixScheduleKey below.
const scheduleName = "tonus-sync"

// Mix-cron key prefixes — used both at registration in OnInit and parsing in OnCallback.
const (
	mixDiscoveryKeyPrefix = "mix-discovery/"
	mixBuildKeyPrefix     = "mix-build/"
)

// Defaults mirror the JSON-Schema defaults; used when a key is set-but-empty.
const (
	defaultCron             = "0 7 * * *"
	defaultMixDiscoveryCron = "0 2 * * 3"
	defaultMixBuildCron     = "0 6 * * 5"
	defaultMixCount         = 25
	defaultMixDiscoveryRatio = 0.4
	defaultMixPlaylistTpl   = "{name}"
)

type plugin struct{}

// mixConfig mirrors one entry of the `users[].mixes[]` array in the manifest.
type mixConfig struct {
	Name             string  `json:"name"`
	Genre            string  `json:"genre"`
	DiscoveryCron    string  `json:"discovery_cron"`
	BuildCron        string  `json:"build_cron"`
	Count            int     `json:"count"`
	DiscoveryRatio   float64 `json:"discovery_ratio"`
	PlaylistTemplate string  `json:"playlist_template"`
}

// userMapping mirrors the `users` array in the manifest config. Read out of
// pdk.GetConfig("users") which returns a JSON-string of this array.
type userMapping struct {
	NavidromeUsername    string      `json:"navidrome_username"`
	ListenBrainzUsername string      `json:"listenbrainz_username"`
	Mixes                []mixConfig `json:"mixes,omitempty"`
	LbWeeklyMirror       bool        `json:"lb_weekly_mirror"`
}

func loadUsers() ([]userMapping, error) {
	raw, ok := pdk.GetConfig("users")
	if !ok || raw == "" {
		return nil, fmt.Errorf("`users` config is empty — add at least one user mapping in the plugin settings")
	}
	var out []userMapping
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("invalid `users` JSON: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("`users` array is empty")
	}
	return out, nil
}

// OnInit registers the cron and creates the task queue. We don't validate
// the config deeply here — defects surface at the first OnCallback.
func (p *plugin) OnInit() error {
	tonusURL, _ := pdk.GetConfig("tonus_url")
	cronExpr, ok := pdk.GetConfig("cron_expression")
	if !ok || cronExpr == "" {
		cronExpr = defaultCron
	}

	if tonusURL == "" {
		return fmt.Errorf("tonus_url is required (set it on the plugin settings page)")
	}

	// TaskQueue: concurrency 1 keeps user-runs serialized so the Subsonic
	// playlist-update is well-ordered and never races against itself.
	if err := host.TaskCreateQueue(tasks.QueueName, host.QueueConfig{
		Concurrency: 1,
		MaxRetries:  3,
		BackoffMs:   30_000,
		// RetentionMs is "how long terminated tasks are kept" — 1h is plenty
		// for log inspection without bloating the queue store.
		RetentionMs: 3600_000,
	}); err != nil {
		// CreateQueue is idempotent on most implementations; log but proceed.
		pdk.Log(pdk.LogWarn, "TaskCreateQueue: "+err.Error())
	}

	if _, err := host.SchedulerScheduleRecurring(cronExpr, scheduleName, scheduleName); err != nil {
		return fmt.Errorf("failed to schedule '%s' (%s): %w", scheduleName, cronExpr, err)
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf(
		"tonus-sync initialized: url=%s, cron=%q, queue=%s",
		tonusURL, cronExpr, tasks.QueueName,
	))

	// Mix-Crons: pro (User × Mix × Phase) ein eigener Cron-Schedule mit
	// pfadartigem Key. OnCallback parst den Key zurück + dispatcht den
	// passenden Job-Type (mix-discovery oder mix-build).
	users, err := loadUsers()
	if err != nil {
		// Initial users may be empty if the user hasn't configured anything yet —
		// not fatal at OnInit time. Mix-Crons get registered on next plugin
		// reload after the user adds Mixes.
		pdk.Log(pdk.LogInfo, "no users configured yet — Mix crons will be registered on next reload")
		return nil
	}
	mixCount := 0
	for _, u := range users {
		if u.NavidromeUsername == "" {
			continue
		}
		for _, m := range u.Mixes {
			if m.Name == "" || m.Genre == "" {
				continue
			}
			discCron := m.DiscoveryCron
			if discCron == "" {
				discCron = defaultMixDiscoveryCron
			}
			buildCron := m.BuildCron
			if buildCron == "" {
				buildCron = defaultMixBuildCron
			}
			discKey := mixDiscoveryKeyPrefix + u.NavidromeUsername + "/" + m.Name
			buildKey := mixBuildKeyPrefix + u.NavidromeUsername + "/" + m.Name
			if _, err := host.SchedulerScheduleRecurring(discCron, discKey, discKey); err != nil {
				pdk.Log(pdk.LogError, fmt.Sprintf("schedule %q (%s): %s", discKey, discCron, err.Error()))
				continue
			}
			if _, err := host.SchedulerScheduleRecurring(buildCron, buildKey, buildKey); err != nil {
				pdk.Log(pdk.LogError, fmt.Sprintf("schedule %q (%s): %s", buildKey, buildCron, err.Error()))
				continue
			}
			mixCount++
			pdk.Log(pdk.LogInfo, fmt.Sprintf(
				"mix scheduled: user=%q name=%q genre=%q discovery=%q build=%q",
				u.NavidromeUsername, m.Name, m.Genre, discCron, buildCron,
			))
		}
	}
	if mixCount > 0 {
		pdk.Log(pdk.LogInfo, fmt.Sprintf("registered %d genre-mix cron pairs", mixCount))
	}
	return nil
}

// parseMixScheduleKey splits a mix-cron key like
// "mix-discovery/<user>/<mix>" or "mix-build/<user>/<mix>" into its parts.
// Returns (jobType, navidromeUser, mixName, ok). ok=false if the key
// doesn't match any mix-cron pattern.
//
// User and mix names may contain slashes — they get re-joined from the
// remaining parts. In practice users won't put slashes there, but it's
// safer than splitting on N=3.
func parseMixScheduleKey(key string) (jobType, user, mix string, ok bool) {
	for _, prefix := range []string{mixDiscoveryKeyPrefix, mixBuildKeyPrefix} {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		// rest = "<user>/<mix>" — split on first "/" only
		idx := strings.Index(rest, "/")
		if idx <= 0 || idx >= len(rest)-1 {
			return "", "", "", false
		}
		user = rest[:idx]
		mix = rest[idx+1:]
		jobType = strings.TrimSuffix(prefix, "/")
		return jobType, user, mix, true
	}
	return "", "", "", false
}

// OnCallback (cron tick): dispatches based on schedule key.
//   - "tonus-sync"            → classic per-user discovery (trigger+reconcile)
//   - "mix-discovery/u/m"     → mix-discovery task for that user×mix
//   - "mix-build/u/m"         → mix-build task for that user×mix
//
// All paths return fast (<1 s) — actual work happens in TaskQueue handlers
// outside the 30-s plugin-callback timeout.
func (p *plugin) OnCallback(req scheduler.SchedulerCallbackRequest) error {
	key := req.ScheduleID
	if key == "" {
		key = req.Payload
	}

	tonusURL, _ := pdk.GetConfig("tonus_url")
	tonusToken, _ := pdk.GetConfig("tonus_token")

	// Mix-cron path?
	if jobType, user, mix, ok := parseMixScheduleKey(key); ok {
		return p.dispatchMixCallback(jobType, user, mix, tonusURL, tonusToken)
	}

	// Classic discovery path
	if key != scheduleName {
		pdk.Log(pdk.LogWarn, "OnCallback got unexpected schedule: id="+req.ScheduleID+" payload="+req.Payload)
		return nil
	}

	users, err := loadUsers()
	if err != nil {
		pdk.Log(pdk.LogError, "tonus-sync: "+err.Error())
		return nil
	}

	enqueuedDiscovery := 0
	enqueuedReconcile := 0

	for _, u := range users {
		if u.NavidromeUsername == "" || !u.LbWeeklyMirror {
			continue
		}
		for _, slot := range tasks.LbWeeklySlots {
			base := tasks.Job{
				NavidromeUser:    u.NavidromeUsername,
				ListenBrainzUser: u.ListenBrainzUsername,
				PlaylistName:     slot.PlaylistName,
				LbSourcePatch:    slot.SourcePatch,
				LbOccurrence:     slot.Occurrence,
				TonusURL:         tonusURL,
				TonusToken:       tonusToken,
			}

			// 1) Discovery → backend queues missing LB-Weekly tracks for download.
			disc := base
			disc.Type = tasks.JobLbWeeklyDiscovery
			if payload, err := disc.Marshal(); err == nil {
				if _, err := host.TaskEnqueue(tasks.QueueName, payload); err != nil {
					pdk.Log(pdk.LogError, fmt.Sprintf(
						"enqueue lbweekly-discovery %q/%q: %s",
						u.NavidromeUsername, slot.PlaylistName, err.Error(),
					))
				} else {
					enqueuedDiscovery++
				}
			}

			// 2) Reconcile → pull finished tracks + push to user playlist.
			// Catches tracks from earlier runs that finished in the meantime.
			rec := base
			rec.Type = tasks.JobReconcile
			if payload, err := rec.Marshal(); err == nil {
				if _, err := host.TaskEnqueue(tasks.QueueName, payload); err != nil {
					pdk.Log(pdk.LogError, fmt.Sprintf(
						"enqueue reconcile %q/%q: %s",
						u.NavidromeUsername, slot.PlaylistName, err.Error(),
					))
				} else {
					enqueuedReconcile++
				}
			}
		}
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf(
		"tonus-sync: enqueued %d lbweekly-discovery + %d reconcile tasks (users=%d)",
		enqueuedDiscovery, enqueuedReconcile, len(users),
	))
	return nil
}

// dispatchMixCallback handles a mix-discovery or mix-build cron tick. Finds
// the user/mix in the current config, builds the corresponding Job, and
// enqueues it. Returns fast — actual work runs in the TaskQueue handlers.
func (p *plugin) dispatchMixCallback(jobType, navidromeUser, mixName, tonusURL, tonusToken string) error {
	users, err := loadUsers()
	if err != nil {
		pdk.Log(pdk.LogError, "mix-callback: "+err.Error())
		return nil
	}

	// Find matching (user, mix) pair from the current config.
	var found *mixConfig
	for _, u := range users {
		if u.NavidromeUsername != navidromeUser {
			continue
		}
		for i := range u.Mixes {
			if u.Mixes[i].Name == mixName {
				found = &u.Mixes[i]
				break
			}
		}
		break
	}
	if found == nil {
		pdk.Log(pdk.LogWarn, fmt.Sprintf(
			"mix-callback: no matching mix for user=%q name=%q (config changed since cron registered?)",
			navidromeUser, mixName,
		))
		return nil
	}

	// Defaults applied here, mirroring the JSON-Schema defaults — keeps the
	// task payload self-contained even if the config is sparse.
	count := found.Count
	if count <= 0 {
		count = defaultMixCount
	}
	ratio := found.DiscoveryRatio
	if ratio <= 0 {
		ratio = defaultMixDiscoveryRatio
	}
	tpl := found.PlaylistTemplate
	if tpl == "" {
		tpl = defaultMixPlaylistTpl
	}

	job := tasks.Job{
		NavidromeUser:       navidromeUser,
		TonusURL:            tonusURL,
		TonusToken:          tonusToken,
		MixName:             found.Name,
		MixGenre:            found.Genre,
		MixCount:            count,
		MixDiscoveryRatio:   ratio,
		MixPlaylistTemplate: tpl,
	}

	switch jobType {
	case strings.TrimSuffix(mixDiscoveryKeyPrefix, "/"):
		job.Type = tasks.JobMixDiscovery
	case strings.TrimSuffix(mixBuildKeyPrefix, "/"):
		job.Type = tasks.JobMixBuild
	default:
		pdk.Log(pdk.LogError, "mix-callback: unknown jobType="+jobType)
		return nil
	}

	payload, err := job.Marshal()
	if err != nil {
		pdk.Log(pdk.LogError, "mix-callback: marshal: "+err.Error())
		return nil
	}
	if _, err := host.TaskEnqueue(tasks.QueueName, payload); err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf(
			"mix-callback: enqueue %s for %q/%q: %s", jobType, navidromeUser, mixName, err.Error(),
		))
		return nil
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf(
		"mix-callback: enqueued %s for user=%q mix=%q (genre=%s)",
		jobType, navidromeUser, mixName, found.Genre,
	))
	return nil
}

// OnTaskExecute is the taskworker hook — runs each enqueued job.
//
// Returns (string, error). A non-empty string is treated as an unrecoverable
// error (logged + dropped). A non-nil error triggers the queue's retry/backoff.
func (p *plugin) OnTaskExecute(req taskworker.TaskExecuteRequest) (string, error) {
	var job tasks.Job
	if err := json.Unmarshal(req.Payload, &job); err != nil {
		msg := fmt.Sprintf("OnTaskExecute: unmarshal failed: %v (payload=%s)", err, string(req.Payload))
		pdk.Log(pdk.LogError, msg)
		return msg, nil // unrecoverable — don't retry
	}
	return job.Dispatch()
}

func main() {}

func init() {
	p := &plugin{}
	lifecycle.Register(p)
	scheduler.Register(p)
	taskworker.Register(p)
}
