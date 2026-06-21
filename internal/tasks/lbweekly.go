// LB-Weekly-Mirror discovery task.
//
// runLbWeeklyDiscovery calls the tonus backend's POST
// /api/plugin/lbweekly/discovery for one (user, slot) and persists the
// returned existing-track list in KVStore so the build phase (the existing
// runReconcile, wired up in Task 6) can find in-library tracks without an
// additional search3 round-trip.
//
// KVStore key format: "lbweekly:<navidrome_user>:<playlist_name>".
// runReconcile already reads finished-tracks by playlist_name, so these two
// phases are loosely coupled through the shared playlist_name field in Job.
package tasks

import (
	"encoding/json"
	"fmt"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"

	"tonus-navidrome-plugin/internal/client"
)

// runLbWeeklyDiscovery triggers an LB-Weekly slot discovery: calls the
// backend, persists the existing-list in KVStore for the build phase (which
// reuses runReconcile, reading finished-tracks by playlist_name).
func runLbWeeklyDiscovery(j *Job) (string, error) {
	cl := client.New(j.TonusURL, j.TonusToken)

	// Health-check first so config errors surface immediately.
	h, err := cl.Health()
	if err != nil {
		return "", fmt.Errorf("lbweekly %q/%q: health: %w", j.NavidromeUser, j.PlaylistName, err)
	}
	if h.AuthRequired && j.TonusToken == "" {
		return fmt.Sprintf("lbweekly %q: backend requires token but none configured", j.NavidromeUser), nil
	}

	resp, err := cl.LbWeeklyDiscovery(client.LbWeeklyDiscoveryRequest{
		NavidromeUser:    j.NavidromeUser,
		ListenBrainzUser: j.ListenBrainzUser,
		SourcePatch:      j.LbSourcePatch,
		Occurrence:       j.LbOccurrence,
		PlaylistName:     j.PlaylistName,
	})
	if err != nil {
		return "", fmt.Errorf("lbweekly %q/%q: %w", j.NavidromeUser, j.PlaylistName, err)
	}

	// Persist the existing-list for the build phase. Exact pattern from
	// runMixDiscovery (mix.go): json.Marshal + host.KVStoreSet.
	kvKey := fmt.Sprintf("lbweekly:%s:%s", j.NavidromeUser, j.PlaylistName)
	if data, mErr := json.Marshal(resp.Existing); mErr != nil {
		pdk.Log(pdk.LogWarn, fmt.Sprintf("lbweekly marshal existing %s: %s", kvKey, mErr.Error()))
	} else if err := host.KVStoreSet(kvKey, data); err != nil {
		pdk.Log(pdk.LogWarn, fmt.Sprintf("lbweekly KVStoreSet %s: %s", kvKey, err.Error()))
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf(
		"lbweekly %q/%q accepted: existing=%d",
		j.NavidromeUser, j.PlaylistName, len(resp.Existing),
	))
	return "ok", nil
}
