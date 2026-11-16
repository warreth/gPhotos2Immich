# gPhotos2Immich
![Docker Downloads](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fipitio.github.io%2Fbackage%2Fwarreth%2FgPhotos2Immich%2Fgphotos2immich.json&query=%24.downloads&label=Total%20Downloads&color=blue)
![Docker Daily Downloads](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fipitio.github.io%2Fbackage%2Fwarreth%2FgPhotos2Immich%2Fgphotos2immich.json&query=%24.downloads_day&label=Daily%20Downloads&color=teal)

Sync photos from Google Photos shared albums to your [Immich](https://immich.app) instance automatically. Optionally upload new Immich additions back to Google Photos, or import albums from your own Google Photos account with a connected browser session.

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

2. Open **[http://localhost:8080](http://localhost:8080)** in your browser to configure your Immich API details and Google Photos shared album links via the Web UI! Settings save automatically, and most changes apply to a running sync without a restart.

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

For Immich notifications, also add `notification.create`. Immich currently treats notification creation as an admin-only action.

---

## Frequently Asked Questions

**Can this sync albums I don't own, or private albums?**
Public shared albums work without Google authentication; the link must be viewable in an incognito window. Private albums are supported experimentally when a browser session is connected, under **Private albums**. The importer downloads the originals into Immich. Live Photo motion is imported as the still only, and there is no schedule. See [Private album import](#private-album-import-experimental).

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
- **Web UI & Hot Reloading:** Manage every setting from the web interface on port 8080: connection, workers, dedup mode, metadata backfill, albums, storage and live logs. Settings save automatically, and most changes apply to a running sync without a restart.
- **Trigger Sync API:** Trigger a sync on demand outside the regular schedule via `POST /api/sync` or `GET /sync_now` (ideal for Home Assistant buttons, webhooks, or automation scripts). See [home-assistant.yaml](home-assistant.yaml) for a ready-to-use Home Assistant configuration.
- **Smart Syncing:** We pull down the full images and videos (no compressed thumbnails), extract the correct "taken" dates, and smoothly avoid Google's rate limits.
- **Photographer Metadata:** The person who added each photo to the shared album is written to the Immich description as `Photographer: <name>`, next to the source album. Enable `updateExistingMetadata` to backfill photos that were imported before this feature.
- **Speed & Deduping:** Concurrent workers speed through downloads, while a persistent local cache skips over photos that Immich already has, saving you bandwidth.
- **Deletion-aware Dedup:** The default `safe` mode respects photos you delete in Immich and automatically adopts compressed replacements created by tools such as `immich-compressor`. Use `repair` mode to re-upload anything that is missing.
- **Optional Immich to Google Photos upload:** Link an Immich album to a Google Photos album and upload new additions back on a schedule. Photos upload through the Google account linked to their Immich owner. Live Photos are uploaded as a linked still and video pair, and deletions never sync in either direction.
- **Three experience modes:** Switch between Simple, Standard, and Advanced at the top of the sidebar. Simple keeps the basics and shows a read-only default account. Standard adds two-way sync, Google upload, private albums, and notifications. Advanced exposes the per-album import owner and every setting. The mode is stored in `uiMode`.
- **Unified accounts:** One account combines an Immich user, that user's Immich API key, and an optional Google Photos credential with browser cookies. Add accounts under **Accounts & Users**, with the Add button under the account list.
- **Two independent defaults:** Choose a default import account for new photos and the Immich connection, and a default Google upload account for photos whose Immich owner has no linked Google account.
- **Automatic Immich album matching:** When you paste a shared album link, the app resolves the Google title, selects an existing Immich album with the same name, or suggests close matches. If none fits, it creates the album on the first import only when the Google album contains photos.
- **Clear import owner:** The per-album import owner is now an Advanced control labelled "Who owns the imported photos?", with the default import account preselected.
- **Reset accounts:** Clear every account, API key, Google credential, cookie file, and Immich album ID while keeping album URLs and intervals.
- **Live activity and structured logs:** The Activity page shows import and upload progress bars, counters, and a filterable event stream. The raw terminal log is still available for debugging.
- **Immich notifications:** Choose whether a failed sync, a finished import, or a finished Google Photos upload sends a notification, and choose the default import account or another linked account to send it.
- **ntfy notifications:** Send the same events to ntfy.sh or your own ntfy server as an independent second channel.
- **Private album import (experimental):** Import albums from your own Google Photos account into Immich from a separate tab. Originals are downloaded, items are deduplicated, and progress is shown live.
- **Guided setup:** The Overview page shows a first-run checklist and a short What's new card when a release adds features.
- **Re-upload control:** Photos deleted from Google Photos stay deleted by default. Turn on the check per Google upload if you want missing items restored.

## Accounts and experience modes

The Web UI has three modes, stored in `uiMode`:

- `simple` shows Overview, Connection, Albums, Accounts, and Logs. Accounts is read-only and shows only the default import account.
- `standard` adds sync behavior, deduplication, Google upload, private albums, notifications, and account management.
- `advanced` adds the per-album import owner and every setting.

The mode only changes what the interface shows. Accounts and settings created in another mode keep working in the background.

In **Accounts & Users**, one account combines:
- an Immich user and that user's Immich API key
- an optional Google Photos credential and browser cookies for shared albums

Choose two defaults at the top of the page:
- **Default import account** owns new photos imported from shared albums and provides the Immich server connection.
- **Default Google upload account** uploads photos whose Immich owner has no linked Google account.

Add account sits under the account list. Selecting an account shows its credential status on the right. **Edit account** replaces credentials explicitly; **Reset accounts** clears every account and credential while keeping album URLs and intervals.

Immich assigns every imported asset to the owner of the API key that calls `POST /assets`. The import owner is the default import account unless you choose another person under the album's Advanced **Import owner** control. In the other direction, the uploader reads each asset's `ownerId` and uses the Google account linked to that person. Set `googleUpload.strictAccountMatching` to skip owners without a linked Google account. See [CONFIGURATION.md](CONFIGURATION.md#accounts-and-routing) for the field reference.

## Google Photos upload

The upload direction is optional and per album. It uses the pinned [`gotohp`](https://github.com/xob0t/gotohp) CLI and the reverse-engineered Google Photos mobile API. Google credentials and browser cookies are managed per account under **Accounts & Users**. The default Google account uploads when the Immich owner has no linked Google account. Albums shared with you use the browser cookies of the account that owns the import, because Google's mobile API cannot add to an album you do not own. See [CONFIGURATION.md](CONFIGURATION.md#google-photos-upload) for setup.

- A Google credential is optional. Without it, the account can still import photos and receive notifications, but it cannot upload to Google Photos.
- Browser cookies are optional. They are only needed for shared albums someone else owns and for private album imports. Albums you own upload without cookies. The account that uploads the files uses its own browser session; the default Google account is the usual one.
- Immich v3 removed album assets from its album response. The app reads album contents through Immich search, so uploads work on v3.

### Unlimited storage

The optional `unlimited` storage mode makes the uploader identify as a Pixel phone. Google then stores the upload without charging your storage quota. This is an unofficial mobile-API behavior, not a Google feature. There are no known reports of accounts being banned for using it, but Google can change or enforce this at any time. The default is `quota`; enable `unlimited` only if you accept that risk.

## Private album import (experimental)

Private albums live in your own Google Photos account. They use the browser cookies of the default import account: open **Accounts & Users**, edit that account, and add a `cookies.txt` for `photos.google.com` under **Browser cookies**.

Open **Private albums**, choose an album, pick an existing Immich album or create a new one, and press **Import**. The importer downloads the original files, skips items it already imported, and shows progress on the same page. It is manual only, has no schedule, and does not propagate deletions.

Limitations: Live Photo motion is imported as the still only. If Google asks for a fresh browser session, export the cookies again and reconnect. The feature is experimental.

## Notifications

Notifications are optional and configured under **Notifications** in the Web UI. Two independent channels are available:

- **Immich** sends a notification through the admin notification API. Choose the default import account or another linked account with an Immich API key; the notification appears in that person's Immich account. The key needs `notification.create`, which Immich currently grants to admin keys.
- **ntfy** posts to ntfy.sh or your own server. Set the URL, topic, optional access token, and priority, then pick the events for that channel.

Each channel has its own event switches. A failure in one channel never blocks the other, and a failed notification never interrupts a sync. The test buttons show the exact error if a channel is not configured correctly.


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