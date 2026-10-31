// Package tasks defines the asynchronous job format for the plugin's
// TaskQueue. The cron-callback enqueues jobs per Navidrome user per run;
// the OnTaskExecute hook deserializes and dispatches them.
//
// Job types:
//   - JobReconcile         → query /api/plugin/finished-tracks + push to Subsonic
//   - JobMixDiscovery      → POST /api/plugin/mix/discovery for one (user, mix) —
//                            gets the in-library "existing" list back, persists it
//                            in KVStore for the build phase, queues missing tracks
//                            for download in the background
//   - JobMixBuild          → reads KVStore "existing" + queries finished-tracks for
//                            the same (user, mix), then replaces the user's Subsonic
//                            playlist content with the merged result (Spotify-style
//                            weekly mix with stable playlist ID)
//   - JobLbWeeklyDiscovery → POST /api/plugin/lbweekly/discovery for one (user,
//                            slot) — gets the in-library "existing" list back,
//                            persists it in KVStore; build phase reuses runReconcile
//   - JobLbFreshDiscovery  → POST /api/plugin/lbfresh/discovery for one user —
//                            same flow as lbweekly, source is the user's LB
//                            Fresh Releases
//
// All jobs run in the same TaskQueue (concurrency 1) so per-user runs never
// overlap and Subsonic playlist updates are well-ordered.
package tasks

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"

	"tonus-navidrome-plugin/internal/client"
	"tonus-navidrome-plugin/internal/subsonic"
)

// QueueName must be created in OnInit via host.TaskCreateQueue and re-used
// by both Enqueue and the queue-config in main.go.
const QueueName = "tonus-sync"

type JobType string

const (
	JobReconcile         JobType = "reconcile"
	JobMixDiscovery      JobType = "mix-discovery"
	JobMixBuild          JobType = "mix-build"
	JobLbWeeklyDiscovery JobType = "lbweekly-discovery"
	JobLbFreshDiscovery  JobType = "lbfresh-discovery"
)

// LbFreshPlaylistName is the Navidrome-side name of the Fresh Releases playlist.
const LbFreshPlaylistName = "Fresh Releases"

// LbWeeklySlot describes one of the four mirrored LB playlists.
type LbWeeklySlot struct {
	PlaylistName string // Navidrome-side name
	SourcePatch  string // LB source_patch parameter
	Occurrence   int    // 0 = current week, 1 = previous week
}

// LbWeeklySlots — fixed order, mirrored per user.
var LbWeeklySlots = []LbWeeklySlot{
	{"Weekly Exploration", "weekly-exploration", 0},
	{"Weekly Jams", "weekly-jams", 0},
	{"Last Week's Exploration", "weekly-exploration", 1},
	{"Last Week's Jams", "weekly-jams", 1},
}

// Job is the canonical payload format. Marshal → bytes → host.TaskEnqueue
// in main.go; OnTaskExecute does Unmarshal → Dispatch.
type Job struct {
	Type             JobType `json:"type"`
	NavidromeUser    string  `json:"navidrome_user"`
	ListenBrainzUser string  `json:"listenbrainz_user"`
	PlaylistName     string  `json:"playlist_name"` // already date-substituted at enqueue time

	// Connection (passed in so each task is fully self-contained).
	TonusURL   string `json:"tonus_url"`
	TonusToken string `json:"tonus_token"`

	// Mix-specific (used by JobMixDiscovery + JobMixBuild). Empty for
	// reconcile/lbweekly jobs. Set in OnCallback when the schedule key
	// belongs to a per-(user, mix) cron.
	MixName             string  `json:"mix_name,omitempty"`
	MixGenre            string  `json:"mix_genre,omitempty"`
	MixCount            int     `json:"mix_count,omitempty"`
	MixDiscoveryRatio   float64 `json:"mix_discovery_ratio,omitempty"`
	MixPlaylistTemplate string  `json:"mix_playlist_template,omitempty"`

	// LB-Weekly-specific (used by JobLbWeeklyDiscovery).
	LbSourcePatch string `json:"lb_source_patch,omitempty"`
	LbOccurrence  int    `json:"lb_occurrence,omitempty"`

	// LB-Fresh-specific (used by JobLbFreshDiscovery). Optional.
	LbToken string `json:"lb_token,omitempty"`

	// ReplaceWindowDays > 0 makes runReconcile replace the playlist content
	// with the current in-library list plus tracks finished within that many
	// days, instead of only appending. Old entries drop out of the playlist;
	// the songs stay in the library.
	ReplaceWindowDays int `json:"replace_window_days,omitempty"`
}

// Marshal returns the task payload bytes for host.TaskEnqueue.
func (j *Job) Marshal() ([]byte, error) {
	return json.Marshal(j)
}

// Dispatch routes the job to its handler. Return value matches the
// taskworker.OnTaskExecute contract: (unrecoverableErr string, retryErr error).
//   - non-empty string → unrecoverable, log + drop
//   - non-nil error    → host should retry (per QueueConfig.MaxRetries)
//   - both empty       → success
func (j *Job) Dispatch() (string, error) {
	switch j.Type {
	case JobReconcile:
		return runReconcile(j)
	case JobMixDiscovery:
		return runMixDiscovery(j)
	case JobMixBuild:
		return runMixBuild(j)
	case JobLbWeeklyDiscovery:
		return runLbWeeklyDiscovery(j)
	case JobLbFreshDiscovery:
		return runLbFreshDiscovery(j)
	default:
		return fmt.Sprintf("unknown job type %q", j.Type), nil
	}
}

// lbWeeklyExistingTrack is a local mirror of client.LbWeeklyExistingTrack used
// to unmarshal the KVStore-persisted existing-list without a circular import.
// Must stay in sync with client.LbWeeklyExistingTrack (json tags are canonical).
type lbWeeklyExistingTrack struct {
	SubsonicID string `json:"subsonic_id"`
	Artist     string `json:"artist"`
	Title      string `json:"title"`
}

// ---- Reconcile -----------------------------------------------------------

func runReconcile(j *Job) (string, error) {
	if j.PlaylistName == "" {
		// Without a playlist target there's nothing to reconcile.
		return "", nil
	}
	cl := client.New(j.TonusURL, j.TonusToken)

	// 0. Read the LB-Weekly existing-list from KVStore (persisted by
	// runLbWeeklyDiscovery). Must happen BEFORE the finished.Count==0 check
	// so that a playlist consisting entirely of in-library tracks still gets
	// built even when no downloads finished yet.
	var existingTracks []lbWeeklyExistingTrack
	kvKey := fmt.Sprintf("lbweekly:%s:%s", j.NavidromeUser, j.PlaylistName)
	if kvBytes, ok, kvErr := host.KVStoreGet(kvKey); kvErr != nil {
		pdk.Log(pdk.LogWarn, fmt.Sprintf(
			"reconcile %q/%q: KVStoreGet %s failed (continuing without existing): %s",
			j.NavidromeUser, j.PlaylistName, kvKey, kvErr.Error(),
		))
	} else if ok && len(kvBytes) > 0 {
		if err := json.Unmarshal(kvBytes, &existingTracks); err != nil {
			pdk.Log(pdk.LogWarn, fmt.Sprintf(
				"reconcile %q/%q: parse KV %s: %s",
				j.NavidromeUser, j.PlaylistName, kvKey, err.Error(),
			))
		}
	}

	// 1. Pull a small batch of finished tracks (limit=25 keeps backend
	// search3-resolution + plugin Subsonic-write under the 30s task timeout).
	// Remaining tracks come along on subsequent cron triggers.
	//
	// Replace mode: the playlist is rebuilt from scratch each run, so we need
	// the whole current set (limit 200) instead of a 25-track batch.
	//   - Fresh Releases: rolling ReplaceWindowDays window.
	//   - LB-Weekly slots: everything queued since the last LB rotation. The
	//     rotation time comes from the discovery; without it (older tonus)
	//     we stay in append mode.
	sinceDays := 60
	var sinceMs int64
	limit := 0
	replace := false
	if j.ReplaceWindowDays > 0 {
		sinceDays = j.ReplaceWindowDays
		limit = 200
		replace = true
	} else if j.LbSourcePatch != "" {
		if sinceMs = readLbWeeklyRotated(j.NavidromeUser, j.PlaylistName); sinceMs > 0 {
			limit = 200
			replace = true
		}
	}
	finished, err := cl.FinishedTracks(j.NavidromeUser, sinceDays, j.PlaylistName, sinceMs, limit)
	if err != nil {
		return "", fmt.Errorf("reconcile %q: finished-tracks: %w", j.NavidromeUser, err)
	}
	if finished.Count == 0 && len(existingTracks) == 0 {
		pdk.Log(pdk.LogDebug, fmt.Sprintf("reconcile %q: no finished tracks and no existing tracks yet", j.NavidromeUser))
		return "", nil
	}

	// 2. Subsonic client bound to this user — host injects auth.
	sub := subsonic.New(j.NavidromeUser)

	// 3. Find or create the playlist.
	pl, err := sub.FindPlaylistByName(j.PlaylistName)
	if err != nil {
		return "", fmt.Errorf("reconcile %q: find playlist: %w", j.NavidromeUser, err)
	}
	if pl == nil {
		pl, err = sub.CreatePlaylist(j.PlaylistName, nil)
		if err != nil {
			return "", fmt.Errorf("reconcile %q: create playlist: %w", j.NavidromeUser, err)
		}
		pdk.Log(pdk.LogInfo, fmt.Sprintf(
			"reconcile %q: created playlist %q (id=%s)", j.NavidromeUser, pl.Name, pl.ID,
		))
	}

	// 4. Build the deduplicated song-ID list: existing in-library tracks first,
	// then finished (downloaded) tracks. Existing IDs are already resolved
	// server-side; finished tracks fall back to a search3 call if needed.
	seen := make(map[string]struct{}, len(existingTracks)+finished.Count)
	songIDs := make([]string, 0, len(existingTracks)+finished.Count)
	for _, e := range existingTracks {
		if e.SubsonicID == "" {
			continue
		}
		if _, dup := seen[e.SubsonicID]; dup {
			continue
		}
		seen[e.SubsonicID] = struct{}{}
		songIDs = append(songIDs, e.SubsonicID)
	}

	// 5. Backend already resolved Subsonic-IDs serverside (admin-auth search3,
	// cached in payload). For the rare case where backend couldn't resolve
	// (Subsonic indexer not yet caught up at queue-time) we fall back to a
	// single search3 call here. Misses are retried on the next cron tick.
	unresolved := 0
	for _, ft := range finished.Items {
		sid := ft.SubsonicTrackID
		if sid == "" {
			s, err := sub.SearchTrackID(ft.Artist, ft.Title)
			if err != nil {
				pdk.Log(pdk.LogWarn, fmt.Sprintf(
					"reconcile %q: search3 failed for %s/%s: %s",
					j.NavidromeUser, ft.Artist, ft.Title, err.Error(),
				))
				continue
			}
			sid = s
		}
		if sid == "" {
			unresolved++
			continue
		}
		if _, dup := seen[sid]; dup {
			continue
		}
		seen[sid] = struct{}{}
		songIDs = append(songIDs, sid)
	}

	var added int
	if replace {
		if len(songIDs) == 0 {
			return "", nil
		}
		if err := sub.ReplaceTracks(pl, songIDs); err != nil {
			return "", fmt.Errorf("reconcile %q: replace tracks: %w", j.NavidromeUser, err)
		}
		added = len(songIDs)
	} else {
		added, err = sub.AddTracksIdempotent(pl, songIDs)
		if err != nil {
			return "", fmt.Errorf("reconcile %q: add tracks: %w", j.NavidromeUser, err)
		}
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf(
		"reconcile %q: playlist=%q +%d tracks (already in playlist: %d, existing: %d, unresolved: %d, total finished: %d)",
		j.NavidromeUser, pl.Name, added, len(songIDs)-added, len(existingTracks), unresolved, finished.Count,
	))
	writeUserStatus(j.NavidromeUser, userStatus{
		LastReconcileMs:        time.Now().UnixMilli(),
		LastReconcileAdded:     added,
		LastReconcileUnresolved: unresolved,
		LastReconcileTotal:     finished.Count,
		ListenBrainzUser:       j.ListenBrainzUser,
		PlaylistName:           j.PlaylistName,
		PlaylistID:             pl.ID,
	})
	return "", nil
}

// ---- KVStore-Status (per user) -------------------------------------------

type userStatus struct {
	LastTriggerMs           int64  `json:"last_trigger_ms,omitempty"`
	LastTriggerOK           bool   `json:"last_trigger_ok,omitempty"`
	LastTriggerInfo         string `json:"last_trigger_info,omitempty"`
	LastReconcileMs         int64  `json:"last_reconcile_ms,omitempty"`
	LastReconcileAdded      int    `json:"last_reconcile_added,omitempty"`
	LastReconcileUnresolved int    `json:"last_reconcile_unresolved,omitempty"`
	LastReconcileTotal      int    `json:"last_reconcile_total,omitempty"`
	ListenBrainzUser        string `json:"listenbrainz_user,omitempty"`
	PlaylistName            string `json:"playlist_name,omitempty"`
	PlaylistID              string `json:"playlist_id,omitempty"`
}

// writeUserStatus stores per-user status snapshots. Key = "status:<user>",
// merged on each write so trigger + reconcile fields don't overwrite each
// other.
func writeUserStatus(user string, partial userStatus) {
	if user == "" {
		return
	}
	key := "status:" + strings.TrimSpace(user)

	// Merge with existing.
	var current userStatus
	if existing, ok, err := host.KVStoreGet(key); err == nil && ok {
		_ = json.Unmarshal(existing, &current)
	}
	if partial.LastTriggerMs > 0 {
		current.LastTriggerMs = partial.LastTriggerMs
		current.LastTriggerOK = partial.LastTriggerOK
		current.LastTriggerInfo = partial.LastTriggerInfo
	}
	if partial.LastReconcileMs > 0 {
		current.LastReconcileMs = partial.LastReconcileMs
		current.LastReconcileAdded = partial.LastReconcileAdded
		current.LastReconcileUnresolved = partial.LastReconcileUnresolved
		current.LastReconcileTotal = partial.LastReconcileTotal
	}
	if partial.ListenBrainzUser != "" {
		current.ListenBrainzUser = partial.ListenBrainzUser
	}
	if partial.PlaylistName != "" {
		current.PlaylistName = partial.PlaylistName
	}
	if partial.PlaylistID != "" {
		current.PlaylistID = partial.PlaylistID
	}

	blob, err := json.Marshal(current)
	if err != nil {
		pdk.Log(pdk.LogWarn, "marshal user-status: "+err.Error())
		return
	}
	if err := host.KVStoreSet(key, blob); err != nil {
		pdk.Log(pdk.LogWarn, "KVStoreSet "+key+": "+err.Error())
	}
}
