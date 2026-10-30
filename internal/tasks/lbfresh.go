// LB-Fresh-Releases discovery task.
//
// Same shape as runLbWeeklyDiscovery: calls the tonus backend's POST
// /api/plugin/lbfresh/discovery for one user and persists the returned
// existing-track list under the lbweekly KVStore key, so the shared
// runReconcile builds the "Fresh Releases" playlist without changes.
package tasks

import (
	"encoding/json"
	"fmt"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"

	"tonus-navidrome-plugin/internal/client"
)

func runLbFreshDiscovery(j *Job) (string, error) {
	cl := client.New(j.TonusURL, j.TonusToken)

	h, err := cl.Health()
	if err != nil {
		return "", fmt.Errorf("lbfresh %q: health: %w", j.NavidromeUser, err)
	}
	if h.AuthRequired && j.TonusToken == "" {
		return fmt.Sprintf("lbfresh %q: backend requires token but none configured", j.NavidromeUser), nil
	}

	resp, err := cl.LbFreshDiscovery(client.LbFreshDiscoveryRequest{
		NavidromeUser:     j.NavidromeUser,
		ListenBrainzUser:  j.ListenBrainzUser,
		ListenBrainzToken: j.LbToken,
		PlaylistName:      j.PlaylistName,
	})
	if err != nil {
		return "", fmt.Errorf("lbfresh %q: %w", j.NavidromeUser, err)
	}

	// runReconcile reads "lbweekly:<user>:<playlist>" — reuse that key.
	kvKey := fmt.Sprintf("lbweekly:%s:%s", j.NavidromeUser, j.PlaylistName)
	if data, mErr := json.Marshal(resp.Existing); mErr != nil {
		pdk.Log(pdk.LogWarn, fmt.Sprintf("lbfresh marshal existing %s: %s", kvKey, mErr.Error()))
	} else if err := host.KVStoreSet(kvKey, data); err != nil {
		pdk.Log(pdk.LogWarn, fmt.Sprintf("lbfresh KVStoreSet %s: %s", kvKey, err.Error()))
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf(
		"lbfresh %q/%q accepted: existing=%d",
		j.NavidromeUser, j.PlaylistName, len(resp.Existing),
	))
	return "ok", nil
}
