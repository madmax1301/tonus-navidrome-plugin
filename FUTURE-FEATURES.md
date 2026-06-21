# Future Features — Plugin Roadmap

Stand 2026-05-07. Notizen für künftige Plugin-Iterationen — nicht implementiert, nur dokumentiert.

---

## Spotify-History-Playlists fortschreiben (Idee 2026-05-07)

### Kontext

Tonus v0.3.0 unterstützt einen einmaligen Import aus Spotifys "Extended Streaming History"-Datenexport. Beim Import werden automatisch Subsonic-Playlists pro Jahr und pro Monat angelegt:

- `Spotify History · 2024`
- `Spotify History · 2024-12`
- `Spotify History · 2025-01`
- ...

Das bringt die historische Listening-History rückwirkend in Navidrome. Was es **nicht** tut: laufend pflegen wenn der User weiter Spotify nutzt.

### Idee

Das Plugin könnte den Spotify-History-Pfad fortschreiben:

1. Plugin pollt Spotify (über User-OAuth oder Last.fm-Backup) periodisch (z.B. nightly)
2. Holt neue Listening-Events seit dem letzten Run
3. Aggregiert pro Track + leitet `Spotify History · YYYY` und `Spotify History · YYYY-MM` ab
4. Ruft Tonus-API mit `import_playlist_names` (Phase I) auf
5. Tonus matched/queued/reconciliert wie beim manuellen Import

### Architektur-Skizze

```
Plugin (Cron, nightly)
  ↓
Spotify Recently-Played API
(oder Last.fm /user.getRecentTracks)
  ↓
Per-Track-Aggregation
+ Year/Month-Playlist-Names
  ↓
POST /api/url/search per Track     # Tonus-API, existing
  + payload: { import_playlist_names: ["Spotify History · 2025",
                                        "Spotify History · 2025-01"] }
  ↓
Tonus: Library-Match-Check, Provider-Lookup, Queue, Download
  ↓
_reconcile_imported_playlists      # existing
  ↓
Subsonic-Playlist auto-update
```

### Offene Architektur-Fragen

- **Auth-Modus**: Spotify-OAuth im Plugin (Plugin würde Spotify-Client-ID + Secret + User-Refresh-Token brauchen)? Oder Last.fm-Scrobbles als History-Backup (kein OAuth, nur User+API-Key)?
  - Tonus-Hauptprojekt hat sich gegen Spotify-OAuth entschieden (Account-Risk). Plugin könnte das anders bewerten weil es selbst mit Subsonic-API arbeitet die schon User-Auth braucht.
- **Idempotenz**: Plugin muss tracken, welche Listening-Events schon verarbeitet wurden (z.B. `last_seen_ts` im Plugin-State), damit nicht jede Nacht alles neu durchrennt.
- **Scope**: Nur Tracks die noch nicht in der Library sind (Library-Match-Check kommt sowieso aus Phase H), oder aggressiver auch Library-Tracks zur Playlist hinzufügen?
- **Frequency**: Daily Cron reicht. Bei mehreren Spotify-Logins pro Stunde wäre der Plugin-Run aber ggf. überflüssig — Trigger via Spotify-Webhook gibt's nicht, also pull-basiert.

### Verworfene Alternative

- **Direct Tonus-Backend-Cron** statt Plugin-Cron: das würde aber Spotify-OAuth ins Tonus-Backend zwingen (was wir aus Architektur-Gründen vermeiden wollten — Tonus bleibt cookie-/auth-frei für Provider-Lookups). Plugin-side ist semantisch sauberer: Plugin = Navidrome-Welt, da darf User-Auth-State leben.

### Status

- ⬜ Not implemented
- ⬜ Architektur-Entscheidung (Spotify-OAuth vs Last.fm) offen
- ⬜ Plugin-State-Schema für `last_seen_ts` offen
- Notiert: 2026-05-07
