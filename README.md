# gPhotos2Immich
![Docker Downloads](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fipitio.github.io%2Fbackage%2Fwarreth%2FgPhotos2Immich%2Fgphotos2immich.json&query=%24.downloads&label=Total%20Downloads&color=blue)
![Docker Daily Downloads](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fipitio.github.io%2Fbackage%2Fwarreth%2FgPhotos2Immich%2Fgphotos2immich.json&query=%24.downloads_day&label=Daily%20Downloads&color=teal)

Sync photos from Google Photos shared albums to your [Immich](https://immich.app) instance- automatically, on a schedule.

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

> **Permissions Note:** The container runs as an unprivileged user (`app`, UID 1000). At startup, the entrypoint automatically fixes ownership of mounted `config.json` and `data/` directories. If you encounter permission errors with bind mounts, you can also manually set ownership on the host: `chown 1000:1000 config.json data`

> [!IMPORTANT]
> The built-in Web configuration UI is not password-protected. Do **NOT** expose the port to the public internet or untrusted networks!

> **Heads Up!** If you prefer not to use a Web UI, you can fully disable it by setting `DISABLE_WEBUI=true` and configuring via `config.json`. See the [Configuration Document](CONFIGURATION.md) for details on formatting and settings.

---

## API Key Permissions

To sync photos, generate an Immich API key with the following permissions (or just select "All"):
`asset.read` · `asset.upload` · `album.create` · `album.read` · `album.update` · `albumAsset.create` · `user.read`

---

## Frequently Asked Questions

**Can this sync albums I don't own, or private albums?**
Only public shared album links work. The tool scrapes shared-album URLs without any Google authentication by design, so an album must be viewable by anyone with the link (test it in an incognito window). Private albums and links that require signing in return 404 and cannot be supported without full Google account authentication, which is out of scope.

**Why do I get duplicates in Immich when I already have the same photos?**
Google Photos serves re-encoded (compressed) copies for shared albums, so their checksums differ from your originals and Immich's content-hash duplicate detection cannot match them. Nothing can be fixed on this tool's side; use Immich's built-in duplicate review after syncing to clean them up.

## Features

- **No Google API Keys:** We scrape directly from shared album URLs, so there's no complex Google Cloud setup required!
- **Web UI & Hot Reloading:** Manage your albums from a sleek web interface on port 8080. Check live logs, trigger an instant sync with "Sync Now", and tweak settings; everything applies instantly without restarting the container.
- **Trigger Sync API:** Trigger a sync on demand outside the regular schedule via `POST /api/sync` or `GET /sync_now` (ideal for Home Assistant buttons, webhooks, or automation scripts). See [home-assistant.yaml](home-assistant.yaml) for a ready-to-use Home Assistant configuration.
- **Smart Syncing:** We pull down the full images and videos (no compressed thumbnails), extract the correct "taken" dates, and smoothly avoid Google's rate limits.
- **Speed & Deduping:** Concurrent workers speed through downloads, while a persistent local cache skips over photos that Immich already has, saving you bandwidth.


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