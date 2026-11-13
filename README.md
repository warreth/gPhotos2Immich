# gPhotos2Immich
![Docker Downloads](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fipitio.github.io%2Fbackage%2Fwarreth%2FgPhotos2Immich%2Fgphotos2immich.json&query=%24.downloads&label=Total%20Downloads&color=blue)
![Docker Daily Downloads](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fipitio.github.io%2Fbackage%2Fwarreth%2FgPhotos2Immich%2Fgphotos2immich.json&query=%24.downloads_day&label=Daily%20Downloads&color=teal)

Sync photos from Google Photos shared albums to your [Immich](https://immich.app) instance automatically, with an optional per-album upload back to Google Photos.

---

## Quick Start

1. Run with Docker Compose:

   ```yaml
   # compose.yml
   services:
     gphotos2immich:
       image: ghcr.io/warreth/gphotos2immich:latest
       container_name: gphotos2immich
       network_mode: "host" # Fixes DNS resolution and natively exposes port 8080 for the Web UI
       restart: unless-stopped
       environment:
         - PORT=8080 # Port for the Web UI
         - DISABLE_WEBUI=false # Set to true to fully disable the Web UI
         # - IMMICH_API_KEY=your-key # Overrides config.json when set (e.g. for key rotation)
         # - IMMICH_API_URL=http://immich-server:2283/api # Overrides config.json when set
       volumes:
         - ./config.json:/app/config.json # Optional, will be created by Web UI if omitted
         - ./data:/app/data # Persistent dedup cache (survives container restarts)
   ```

   ```bash
   docker compose up -d
   ```

2. Open **[http://localhost:8080](http://localhost:8080)** in your browser to configure your Immich API details and Google Photos shared album links via the Web UI! The app will automatically hot-reload when you save changes.

### Docker image tags

The image is published to `ghcr.io/warreth/gphotos2immich` with these tags:

| Tag | Description |
| --- | --- |
| `latest` | Newest **stable** release. Pre-releases never get this tag. |
| `beta` | Newest **pre-release** (e.g. `v1.7.0-beta1`). Updated on every pre-release. |
| `vX.Y.Z` | Exact release version, e.g. `v1.7.0`. |
| `sha-<hash>` | Build pinned to a specific commit. |

To test a pre-release, use `ghcr.io/warreth/gphotos2immich:beta` in your compose file. Stable users should stay on `latest` or a pinned `vX.Y.Z` tag.

<br/>

> **Permissions Note:** The container runs as an unprivileged user (`app`, UID 1000).
> - **Default behavior:** If you run the container without specifying a `user:`, it will automatically fix permissions on mounted files at startup and drop to the `app` user.
> - **Explicit user:** If you specify `user: "1000:1000"` in your compose file, ensure the mounted `config.json` and `data/` on the host are writable by UID 1000 (`chown 1000:1000 config.json data`).

> [!IMPORTANT]
> The built-in Web configuration UI is not password-protected. Do **NOT** expose the port to the public internet or untrusted networks!

> **Heads Up!** If you prefer not to use a Web UI, you can fully disable it by setting `DISABLE_WEBUI=true` and configuring via `config.json`. See the [Configuration Document](CONFIGURATION.md) for details on formatting and settings.

---

## API Key Permissions

To sync photos, generate an Immich API key with the following permissions (or just select "All"):
`asset.read` · `asset.upload` · `asset.update` · `album.create` · `album.read` · `album.update` · `albumAsset.create` · `user.read`

---

## Frequently Asked Questions

**Can this sync albums I don't own, or private albums?**
Only public shared album links work. The tool scrapes shared-album URLs without any Google authentication by design, so an album must be viewable by anyone with the link (test it in an incognito window). Private albums and links that require signing in return 404 and cannot be supported without full Google account authentication, which is out of scope.

**Why do I get duplicates in Immich when I already have the same photos?**
Google Photos serves re-encoded (compressed) copies for shared albums, so their checksums differ from your originals and Immich's content-hash duplicate detection cannot match them. Nothing can be fixed on this tool's side; use Immich's built-in duplicate review after syncing to clean them up.

**Why does the log show `connect: connection refused` to Immich after a restart?**
The sync container started before Immich finished booting. The app now retries the connection with backoff until Immich is reachable, so this resolves itself; no restart is needed. If it keeps retrying, verify `apiURL` in `config.json` (or `IMMICH_API_URL`) and the API key. A rejected key or a wrong URL is reported immediately instead of being retried.

**What happens if I delete a photo in Immich?**
By default (`dedupMode: "safe"`) the app respects the deletion and does not upload the photo again. If you want the sync to restore anything that is missing, set `dedupMode: "repair"`. See [Dedup modes](CONFIGURATION.md#dedup-modes).

**What happens if I delete duplicates in Immich?**
`safe` mode does not bring a deleted duplicate back. If the app's copy was deleted and the original remains, the app treats the item as intentionally removed. `repair` mode re-uploads missing items and may recreate a duplicate you deliberately deleted, so use it only when you want a full mirror of the Google Photos album.

**What happens if I compress photos with immich-compressor?**
The compressor creates a new asset with a new Immich ID and a filename such as `gp_<id>.cmp.jpg`. `safe` and `repair` both find that replacement by its stable `gp_<id>` name, update the local state to the new ID, and skip downloading the original again. See [Dedup modes](CONFIGURATION.md#dedup-modes).

**Does the app download the original file?**
It downloads the `=d` variant from Google Photos, which is the full-size file Google serves for a shared album, not a thumbnail. Whether that file is byte-identical to the uploader's original depends on Google: shared albums can be re-encoded or compressed. The app cannot obtain a better copy than Google exposes. If Google serves a re-encoded copy, its checksum differs from your original, so Immich's checksum-based duplicate detection will not merge them; use Immich's duplicate review or import the originals yourself.

## Features

- **No Google API Keys:** We scrape directly from shared album URLs, so there's no complex Google Cloud setup required!
- **Web UI & Hot Reloading:** Manage every setting from the web interface on port 8080: connection, workers, dedup mode, metadata backfill, albums, storage and live logs. Changes apply instantly without restarting the container.
- **Trigger Sync API:** Trigger a sync on demand outside the regular schedule via `POST /api/sync` or `GET /sync_now` (ideal for Home Assistant buttons, webhooks, or automation scripts). See [home-assistant.yaml](home-assistant.yaml) for a ready-to-use Home Assistant configuration.
- **Smart Syncing:** We pull down the full images and videos (no compressed thumbnails), extract the correct "taken" dates, and smoothly avoid Google's rate limits.
- **Photographer Metadata:** The person who added each photo to the shared album is written to the Immich description as `Photographer: <name>`, next to the source album. Enable `updateExistingMetadata` to backfill photos that were imported before this feature.
- **Speed & Deduping:** Concurrent workers speed through downloads, while a persistent local cache skips over photos that Immich already has, saving you bandwidth.
- **Deletion-aware Dedup:** The default `safe` mode respects photos you delete in Immich and automatically adopts compressed replacements created by tools such as `immich-compressor`. Use `repair` mode to re-upload anything that is missing.
- **Optional Immich to Google Photos upload:** Link an Immich album to a Google Photos album and upload new additions back on a schedule. Albums you own use the connected Google account; albums shared with you use an optional browser session. Live Photos are uploaded as a linked still and video pair, and deletions never sync in either direction.


---

## Development

```bash
# Run directly
go run main.go

# Run with Docker (build from source)
sudo docker compose up --build --remove-orphans

# Run tests (offline; integration tests are skipped)
go test ./...

# Run tests including live integration tests against the real test album
GP2IMMICH_INTEGRATION=1 go test ./... -timeout 15m
```