// Mix-job handlers for the Genre-Mix feature.
//
// Two phases per (user, mix), wired up by main.go to two separate cron
// schedules (default Wed 02:00 / Fri 06:00):
//
//   1. JobMixDiscovery: hits the tonus backend's /api/plugin/mix/discovery,
//      receives the in-library "existing" track list, persists it in KVStore
//      under a mix-specific key. Backend in parallel queues missing tracks
//      for download with mix markers in the payload.
//
//   2. JobMixBuild: reads the persisted "existing" list, queries the backend
//      for tracks that finished downloading since discovery (filtered by
//      mix marker), then replaces the user's Subsonic playlist with the
//      merged result. Playlist ID stays stable across runs — users can copy
//      tracks they want to keep into their own playlists.
//
// KVStore key format: "mix-discovery:<navidrome_user>:<mix_name>" → JSON of
// []MixExistingTrack. Stays alive across plugin restarts; only overwritten
// by the next discovery cron of the same mix.
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

// MixExistingTrack mirrors what /api/plugin/mix/discovery returns in the
// existing[] array — Subsonic-ID is server-side-resolved so the build phase
// can directly use it without another search3 call.
type MixExistingTrack struct {
	SubsonicID string `json:"subsonic_id"`
	Artist     string `json:"artist"`
	Title      string `json:"title"`
}

func mixKVKey(navidromeUser, mixName string) string {
	return fmt.Sprintf("mix-discovery:%s:%s", navidromeUser, mixName)
}

// substituteMixTemplate resolves {name}/{genre}/{user}/{date} in the
// playlist-name template. Called at build time so {date} reflects the build
// date, not the discovery date — though for the replace pattern the date
// is rarely meaningful (template default `{name}` produces a stable name).
func substituteMixTemplate(tpl, mixName, genre, navidromeUser string) string {
	if tpl == "" {
		tpl = "{name}"
	}
	out := tpl
	out = strings.ReplaceAll(out, "{name}", mixName)
	out = strings.ReplaceAll(out, "{genre}", genre)
	out = strings.ReplaceAll(out, "{user}", navidromeUser)
	out = strings.ReplaceAll(out, "{date}", time.Now().Format("2006-01-02"))
	return out
}

// ---- Mix Discovery -------------------------------------------------------

func runMixDiscovery(j *Job) (string, error) {
	if j.MixName == "" || j.MixGenre == "" {
		return fmt.Sprintf("mix-discovery: missing mix_name=%q or mix_genre=%q", j.MixName, j.MixGenre), nil
	}

	cl := client.New(j.TonusURL, j.TonusToken)

	// Health-Check first so config errors surface immediately.
	h, err := cl.Health()
	if err != nil {
		return "", fmt.Errorf("mix-discovery %q/%q: health: %w", j.NavidromeUser, j.MixName, err)
	}
	if h.AuthRequired && j.TonusToken == "" {
		return fmt.Sprintf("mix-discovery %q/%q: backend requires token but none configured", j.NavidromeUser, j.MixName), nil
	}

	resp, err := cl.MixDiscovery(client.MixDiscoveryRequest{
		NavidromeUser:  j.NavidromeUser,
		MixName:        j.MixName,
		Genre:          j.MixGenre,
		Count:          j.MixCount,
		DiscoveryRatio: j.MixDiscoveryRatio,
	})
	if err != nil {
		return "", fmt.Errorf("mix-discovery %q/%q: %w", j.NavidromeUser, j.MixName, err)
	}

	// Persist existing-list for the build phase. Even if empty — overwriting
	// stale data from a previous discovery run is intentional.
	existingBytes, err := json.Marshal(resp.Existing)
	if err != nil {
		return "", fmt.Errorf("mix-discovery %q/%q: marshal existing: %w", j.NavidromeUser, j.MixName, err)
	}
	kvKey := mixKVKey(j.NavidromeUser, j.MixName)
	if err := host.KVStoreSet(kvKey, existingBytes); err != nil {
		// Not fatal — backend already started queueing missing tracks. Build
		// phase will see empty existing-list and fall back to finished-tracks.
		pdk.Log(pdk.LogWarn, fmt.Sprintf("mix-discovery KVStoreSet %s: %s", kvKey, err.Error()))
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf(
		"mix-discovery %q/%q (genre=%s) accepted: existing=%d, started=%v %s",
		j.NavidromeUser, j.MixName, j.MixGenre, len(resp.Existing), resp.Started, resp.Message,
	))
	return "", nil
}

// ---- Mix Build -----------------------------------------------------------

func runMixBuild(j *Job) (string, error) {
	if j.MixName == "" {
		return fmt.Sprintf("mix-build: missing mix_name"), nil
	}

	// 1. Existing-list aus KVStore (Discovery-Phase persistierte sie).
	kvKey := mixKVKey(j.NavidromeUser, j.MixName)
	existingBytes, ok, err := host.KVStoreGet(kvKey)
	if err != nil {
		pdk.Log(pdk.LogWarn, fmt.Sprintf(
			"mix-build %q/%q: KVStoreGet %s failed (continuing with empty existing): %s",
			j.NavidromeUser, j.MixName, kvKey, err.Error(),
		))
	}
	var existing []MixExistingTrack
	if ok && len(existingBytes) > 0 {
		if err := json.Unmarshal(existingBytes, &existing); err != nil {
			pdk.Log(pdk.LogWarn, fmt.Sprintf(
				"mix-build %q/%q: parse KV %s: %s",
				j.NavidromeUser, j.MixName, kvKey, err.Error(),
			))
		}
	}

	// 2. Tracks die seit Discovery heruntergeladen wurden — filter by mix
	// marker (plugin_mix_name + plugin_mix_navidrome_user) im Backend.
	cl := client.New(j.TonusURL, j.TonusToken)
	finished, err := cl.FinishedTracksByMix(j.NavidromeUser, j.MixName, 14)
	if err != nil {
		// Nicht fatal: build mit existing-only ergibt eine kleinere aber
		// gültige Playlist. Discovery-Run ohne Build-Recovery ist OK.
		pdk.Log(pdk.LogWarn, fmt.Sprintf(
			"mix-build %q/%q: finished-tracks failed (using existing-only): %s",
			j.NavidromeUser, j.MixName, err.Error(),
		))
		finished = &client.FinishedTracksResponse{}
	}

	// 3. Subsonic-IDs sammeln.
	sub := subsonic.New(j.NavidromeUser)
	songIDs := make([]string, 0, len(existing)+finished.Count)
	for _, e := range existing {
		if e.SubsonicID != "" {
			songIDs = append(songIDs, e.SubsonicID)
		}
	}
	unresolved := 0
	for _, ft := range finished.Items {
		sid := ft.SubsonicTrackID
		if sid == "" {
			s, err := sub.SearchTrackID(ft.Artist, ft.Title)
			if err != nil {
				pdk.Log(pdk.LogWarn, fmt.Sprintf(
					"mix-build %q/%q: search3 %s/%s: %s",
					j.NavidromeUser, j.MixName, ft.Artist, ft.Title, err.Error(),
				))
				continue
			}
			sid = s
		}
		if sid == "" {
			unresolved++
			continue
		}
		songIDs = append(songIDs, sid)
	}

	// Dedupe: same Subsonic-ID kann in existing UND in finished landen, falls
	// ein Track während der Discovery noch nicht im Index war und beim Build
	// aufgetaucht ist. Set-Semantik der Reihenfolge nach erstes Vorkommen.
	songIDs = dedupePreserveOrder(songIDs)

	if len(songIDs) == 0 {
		pdk.Log(pdk.LogWarn, fmt.Sprintf(
			"mix-build %q/%q: no tracks to add (existing=%d, finished=%d, unresolved=%d)",
			j.NavidromeUser, j.MixName, len(existing), finished.Count, unresolved,
		))
		writeUserStatus(j.NavidromeUser, userStatus{
			LastReconcileMs:         time.Now().UnixMilli(),
			LastReconcileTotal:      0,
			LastReconcileUnresolved: unresolved,
			PlaylistName:            substituteMixTemplate(j.MixPlaylistTemplate, j.MixName, j.MixGenre, j.NavidromeUser),
		})
		return "", nil
	}

	playlistName := substituteMixTemplate(j.MixPlaylistTemplate, j.MixName, j.MixGenre, j.NavidromeUser)

	// 4. Find or create + replace pattern.
	pl, err := sub.FindPlaylistByName(playlistName)
	if err != nil {
		return "", fmt.Errorf("mix-build %q/%q: find playlist: %w", j.NavidromeUser, j.MixName, err)
	}
	if pl == nil {
		pl, err = sub.CreatePlaylist(playlistName, songIDs)
		if err != nil {
			return "", fmt.Errorf("mix-build %q/%q: create playlist: %w", j.NavidromeUser, j.MixName, err)
		}
		pdk.Log(pdk.LogInfo, fmt.Sprintf(
			"mix-build %q/%q: created playlist %q (id=%s) with %d tracks (unresolved=%d)",
			j.NavidromeUser, j.MixName, pl.Name, pl.ID, len(songIDs), unresolved,
		))
	} else {
		if err := sub.ReplaceTracks(pl, songIDs); err != nil {
			return "", fmt.Errorf("mix-build %q/%q: replace tracks: %w", j.NavidromeUser, j.MixName, err)
		}
		pdk.Log(pdk.LogInfo, fmt.Sprintf(
			"mix-build %q/%q: replaced playlist %q (id=%s) with %d tracks (unresolved=%d)",
			j.NavidromeUser, j.MixName, pl.Name, pl.ID, len(songIDs), unresolved,
		))
	}

	writeUserStatus(j.NavidromeUser, userStatus{
		LastReconcileMs:         time.Now().UnixMilli(),
		LastReconcileAdded:      len(songIDs),
		LastReconcileUnresolved: unresolved,
		LastReconcileTotal:      len(existing) + finished.Count,
		PlaylistName:            pl.Name,
		PlaylistID:              pl.ID,
	})
	return "", nil
}

func dedupePreserveOrder(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
