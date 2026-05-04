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
       volumes:
         - ./config.json:/app/config.json # Optional, will be created by Web UI if omitted
         - ./data:/app/data # Persistent dedup cache (survives container restarts)
   ```

   ```bash
   docker compose up -d
   ```

2. Open **[http://localhost:8080](http://localhost:8080)** in your browser to configure your Immich API details and Google Photos shared album links via the Web UI! The app will automatically hot-reload when you save changes.

<br/>

> [!IMPORTANT]
> The built-in Web configuration UI is not password-protected. Do **NOT** expose the port to the public internet or untrusted networks!

> **Heads Up!** If you prefer not to use a Web UI, you can fully disable it by setting `DISABLE_WEBUI=true` and configuring via `config.json`. See the [Configuration Document](CONFIGURATION.md) for details on formatting and settings.

---

## API Key Permissions

To sync photos, generate an Immich API key with the following permissions (or just select "All"):
`asset.read` · `asset.upload` · `album.create` · `album.read` · `album.update` · `albumAsset.create` · `user.read`

---

## Features

- **No Google API Keys:** We scrape directly from shared album URLs, so there's no complex Google Cloud setup required!
- **Web UI & Hot Reloading:** Manage your albums from a sleek web interface on port 8080. Check live logs and tweak settings; everything applies instantly without restarting the container.
- **Smart Syncing:** We pull down the full images and videos (no compressed thumbnails), extract the correct "taken" dates, and smoothly avoid Google's rate limits.
- **Speed & Deduping:** Concurrent workers speed through downloads, while a persistent local cache skips over photos that Immich already has, saving you bandwidth.

> [!NOTE]
> Motion/Live photos are imported as still images. The embedded video component is stripped so Immich handles them without errors.

---

## Development

```bash
# Run directly
go run main.go

# Run with Docker (build from source)
sudo docker compose up --build --remove-orphans
```
