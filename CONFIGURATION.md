# Configuration Guide

For advanced setups, environments where a Web UI is undesirable, or fully automated deployments, you can configure `gPhotos2Immich` strictly via a `config.json` file.

If you don't use the Web UI, you can disable it entirely by setting the `DISABLE_WEBUI=true` environment variable. Every option below can also be edited from the built-in Web UI, which includes a connection test and live logs.

## Environment Variables

The `IMMICH_API_KEY` and `IMMICH_API_URL` environment variables override the corresponding `config.json` values at runtime. This is useful for key rotation: update the ENV var on your container and the next config read picks it up, even if a `config.json` with an older key exists. When neither ENV var is set, `config.json` is used unchanged. If neither the file nor the ENV vars provide credentials, the app waits for configuration via the Web UI (or exits if the Web UI is disabled).

## API Permissions

Your Immich API key needs these permissions (or use "All" for simplicity):

`asset.read` · `asset.upload` · `asset.update` · `album.create` · `album.read` · `album.update` · `albumAsset.create` · `user.read`

## Example `config.json`

```json
{
  "apiKey": "YOUR_IMMICH_API_KEY",
  "apiURL": "http://your-immich-ip:2283/api",
  "debug": false,
  "workers": 4,
  "albumWorkers": 3,
  "strictMetadata": false,
  "skipVideos": false,
  "dedupMode": "safe",
  "updateExistingMetadata": false,
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

## Options

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `apiKey` | string | — | Immich API key (required). |
| `apiURL` | string | — | Immich API URL, e.g. `http://localhost:2283/api` (required). |
| `debug` | bool | `false` | Enable verbose debug logging. When disabled, displays clean progress bars with speed and ETA. |
| `workers` | int | `1` | Number of concurrent download/upload workers **per album**. Controls how many photos within a single album are downloaded and uploaded in parallel. Higher values speed up large albums but use more bandwidth and memory. |
| `albumWorkers` | int | `1` | Number of albums processed **concurrently**. Controls how many albums are synced at the same time. Useful when you have many albums configured and want to process several in parallel. |
| `strictMetadata` | bool | `false` | Skip items with missing/invalid dates instead of uploading with current date. Skipped URLs are logged for manual review. |
| `skipVideos` | bool | `false` | Skip all video items entirely. Useful if you only want photos. |
| `dedupMode` | string | `safe` | How the persistent dedup state is reconciled with Immich. See [Dedup modes](#dedup-modes). |
| `updateExistingMetadata` | bool | `false` | Backfill the photographer/source-album description on assets that already exist in Immich. User-edited descriptions are left untouched. |
| `googleUpload.enabled` | bool | `false` | Enable the optional Immich → Google Photos direction. The per-album `uploadToGoogle` switch is the real opt-in. |
| `googleUpload.gotohpBinary` | string | `gotohp-cli` | Path to the pinned `gotohp-cli` binary. The Docker image ships `/app/gotohp-cli`. |
| `googleUpload.gotohpConfig` | string | `./data/gotohp.config` | Private file where `gotohp-cli` stores the Google credential. Mode 0600. |
| `googleUpload.webCookiesFile` | string | `./data/gphotos-web-cookies.txt` | Optional private cookies.txt for the photos.google.com browser session. Needed only for albums you do not own. Mode 0600. |
| `googleUpload.storageMode` | string | `quota` | `quota` counts uploads against Google storage. `unlimited` uses the unofficial mobile-API behaviour and is off by default. |
| `googleUpload.quality` | string | `original` | `original` or `saver`. |
| `googleUpload.skipLivePhotos` | bool | `false` | Upload only the still of a Live Photo instead of the linked still + video pair. |
| `googleUpload.threads` | int | `1` | Concurrent uploads. Keep at 1 unless uploads are slow. |
| `googleUpload.maxRetries` | int | `3` | Reserved. A failed asset is retried on the next scheduled run. |
| `googleUpload.autoUpdate` | bool | `false` | Reserved. Updating the pinned binary is done through the app image, not at runtime. |

## Dedup modes

The app keeps a persistent map of Google Photos item IDs to Immich asset IDs in `data/sync-state.json`. `dedupMode` controls what happens when that map points at an asset that no longer exists in Immich.

| Mode | Deleted by you | Replaced by immich-compressor | Best for |
| --- | --- | --- | --- |
| `safe` (default) | Stays deleted; the app does not re-upload it. | The compressed replacement is found by its `gp_<id>` filename and adopted automatically, so the original is not downloaded again. | Most users. Respects intentional deletions and compression. |
| `repair` | Re-downloaded and uploaded again. | The compressed replacement is adopted first, so no duplicate is created. | Restoring photos you deleted by accident. |
| `fast` | The app trusts the local state without checking Immich. | Not detected; the old asset ID stays in the state. | Very large libraries where the verification search is too expensive. |

What about duplicates? Immich deduplicates uploaded bytes by SHA-1 checksum. When you delete a duplicate in Immich, `safe` mode does **not** bring it back. If the deleted asset was the app's copy and the original remains, the app treats the item as intentionally deleted and skips it. If you want the app to restore anything missing, use `repair`.

If you compress originals with a tool such as [immich-compressor](https://github.com/Navilois/immich-compressor), the compressed asset gets a new Immich ID and a filename like `gp_<id>.cmp.jpg`. `safe` and `repair` both recognise that filename and adopt the new ID, so the original is not re-downloaded.

## Album Options

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `googlePhotos[].url` | string | — | Google Photos shared album link (required). |
| `googlePhotos[].albumName` | string | auto-detected | Override the album name in Immich. If omitted, uses the album title from Google Photos. |
| `googlePhotos[].syncInterval` | string | `24h` | How often to re-check this album (e.g. `12h`, `60m`, `1h30m`). |
| `googlePhotos[].immichAlbumId` | string | — | Link to an existing Immich album by UUID instead of creating a new one. Required when `uploadToGoogle` is enabled. |
| `googlePhotos[].uploadToGoogle` | bool | `false` | Upload new assets from the linked Immich album back to this shared Google Photos album. |
| `googlePhotos[].upload.storageMode` | string | inherited | Per-album override for `quota` or `unlimited`. |
| `googlePhotos[].upload.quality` | string | inherited | Per-album override for `original` or `saver`. |
| `googlePhotos[].upload.skipLivePhotos` | bool | inherited | Per-album override for Live Photo handling. |

## Google Photos upload

The upload feature is optional and per album. It uses the pinned [`gotohp`](https://github.com/xob0t/gotohp) CLI and the reverse-engineered Google Photos mobile API.

Connect the account once from the Web UI, **Google upload** section. The fastest route is Google Embedded Setup:

1. Open `https://accounts.google.com/EmbeddedSetup`.
2. Sign in and click **I agree**. The page may keep loading, that is expected.
3. In DevTools, open Application or Storage, Cookies, `accounts.google.com`, and copy the `oauth_token` value.
4. Paste it into the Web UI and connect.

The credential is stored in `googleUpload.gotohpConfig` with mode 0600 and grants full access to that Google account. Prefer a dedicated account, and do not expose the Web UI without authentication.

There are two access routes, because Google's mobile API and its web app do not allow the same things:

- **Albums you own** use the connected Google account. The album key is resolved from the public share URL and the upload is added with the mobile API.
- **Albums someone else owns and shared with you** use an optional browser session. Open **Google upload**, **Shared album access**, export a Netscape `cookies.txt` for `photos.google.com` from a signed-in browser, and paste it there. The file is stored at `googleUpload.webCookiesFile` with mode 0600. The app uploads the files to your library, then adds them to the shared album through the web session. Without this session, a shared album cannot receive uploads.

Browser cookies expire eventually. If **Verify mapping** or an upload reports that the session is expired, export the cookies again and reconnect. The session grants full access to the Google account, so keep the data directory private.

The upload direction also requires the Google album to be shared with you and **Collaborate** to be enabled. The album key is resolved from the public share URL and shown by **Verify mapping**. When a browser session is connected, verification also labels the album as owned or shared.

Live Photos are uploaded as a linked still + video pair by default. Set `skipLivePhotos` to upload only the still.

Deletions are never propagated. Removing a photo from Immich or Google Photos does not delete it on the other side.

## Web API Endpoints

When the Web UI is enabled, the embedded server provides the following endpoints:

| Endpoint | Method | Description |
| --- | --- | --- |
| `/api/sync` | `POST` / `GET` | Trigger an immediate sync run of all configured albums outside their normal sync interval. Returns `{"status":"ok","message":"Sync triggered"}`. |
| `/sync_now` | `POST` / `GET` | Alias for `/api/sync`, convenient for Home Assistant buttons or curl commands. |
| `/api/status` | `GET` | Return current album synchronization progress, Immich connection status, and `lastRun`/`nextRun` timestamps. |
| `/sync/status` | `GET` | Alias for `/api/status`. |
| `/api/logs` | `GET` | Return the latest application logs. |
| `/api/config` | `GET` / `POST` | Retrieve or update the application configuration. |
