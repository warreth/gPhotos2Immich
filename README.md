# gPhotos2Immich

Sync photos from Google Photos shared albums to your [Immich](https://immich.app) instance- automatically, on a schedule.

---

## Quick Start

1. Copy `config.example.json` to `config.json` and fill in your Immich API details and Google Photos shared album links.

2. Run with Docker Compose:

   ```yaml
   # compose.yml
   services:
     gphotos2immich:
       image: ghcr.io/warreth/gphotos2immich:latest
       container_name: gphotos2immich
       restart: unless-stopped
       volumes:
         - ./config.json:/app/config.json
         - ./data:/app/data # Persistent dedup cache (survives container restarts)
   ```

   ```bash
   docker compose up -d
   ```

> You can also configure via environment variables (`IMMICH_API_KEY`, `IMMICH_API_URL`) instead of mounting a config file.

---

## Configuration

### API Permissions

Your Immich API key needs these permissions (or use "All"):

`asset.read` · `asset.upload` · `album.create` · `album.read` · `album.update` · `albumAsset.create` · `user.read`

### Example `config.json`

```json
{
  "apiKey": "YOUR_IMMICH_API_KEY",
  "apiURL": "http://your-immich-ip:2283/api",
  "debug": false,
  "workers": 4,
  "albumWorkers": 3,
  "strictMetadata": false,
  "skipVideos": false,
  "googlePhotos": [
    {
      "url": "https://photos.app.goo.gl/YourAlbumLink1",
      "albumName": "Vacation 2023",
      "syncInterval": "12h"
    },
    {
      "url": "https://photos.app.goo.gl/ExistingAlbumLink",
      "immichAlbumId": "existing-album-uuid",
      "syncInterval": "1h"
    }
  ]
}
```

### Options

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `apiKey` | string | — | Immich API key (required). |
| `apiURL` | string | — | Immich API URL, e.g. `http://localhost:2283/api` (required). |
| `debug` | bool | `false` | Enable verbose debug logging. When disabled, displays clean progress bars with speed and ETA. |
| `workers` | int | `1` | Number of concurrent download/upload workers **per album**. Controls how many photos within a single album are downloaded and uploaded in parallel. Higher values speed up large albums but use more bandwidth and memory. |
| `albumWorkers` | int | `1` | Number of albums processed **concurrently**. Controls how many albums are synced at the same time. Useful when you have many albums configured and want to process several in parallel. |
| `strictMetadata` | bool | `false` | Skip items with missing/invalid dates instead of uploading with current date. Skipped URLs are logged for manual review. |
| `skipVideos` | bool | `false` | Skip all video items entirely. Useful if you only want photos. |

### Album Options

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `googlePhotos[].url` | string | — | Google Photos shared album link (required). |
| `googlePhotos[].albumName` | string | auto-detected | Override the album name in Immich. If omitted, uses the album title from Google Photos. |
| `googlePhotos[].syncInterval` | string | `24h` | How often to re-check this album (e.g. `12h`, `60m`, `1h30m`). |
| `googlePhotos[].immichAlbumId` | string | — | Link to an existing Immich album by UUID instead of creating a new one. |

---

## Features

- **No Google API key required.** Scrapes directly from shared album links.
- **Video support.** Downloads full videos, not just thumbnails. Disable with `skipVideos`.
- **Concurrent workers.** Parallel download/upload per album (`workers`) and parallel album processing (`albumWorkers`).
- **Live progress.** Progress bars with transfer speed and ETA; verbose structured logs in debug mode.
- **Smart date detection.** Extracts the original "taken" date from metadata.
- **Strict metadata mode.** Optionally skip items with missing dates instead of falling back to the current date.
- **Rate limit protection.** Jitter and exponential backoff to avoid Google Photos throttling.
- **Duplicate detection.** Pre-fetches existing album assets for O(1) dedup. Persistent local cache (`./data/sync-state.json`) survives container restarts. Respects Immich trash.

> **Note:** Motion/Live photos are imported as still images. The embedded video component is stripped so Immich handles them without errors.

---

## Development

```bash
# Run directly
go run main.go

# Run with Docker (build from source)
sudo docker compose up --build --remove-orphans
```
