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
  "uiMode": "advanced",
  "accounts": [
    {
      "id": "lotte",
      "name": "Lotte",
      "immichUserId": "immich-user-uuid",
      "immichApiKey": "optional-immich-api-key-for-imports",
      "googleEmail": "lotte@gmail.com",
      "isDefault": false
    }
  ],
  "googleUpload": {
    "enabled": false,
    "strictAccountMatching": false
  },
  "googlePhotos": [
    {
      "url": "https://photos.app.goo.gl/YourAlbumLink1",
      "albumName": "Vacation 2023",
      "syncInterval": "12h",
      "immichAccountId": "lotte"
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
| `apiKey` | string | none | Immich API key (required). |
| `apiURL` | string | none | Immich API URL, e.g. `http://localhost:2283/api` (required). |
| `debug` | bool | `false` | Enable verbose debug logging. When disabled, displays clean progress bars with speed and ETA. |
| `workers` | int | `1` | Number of concurrent download/upload workers **per album**. Controls how many photos within a single album are downloaded and uploaded in parallel. Higher values speed up large albums but use more bandwidth and memory. |
| `albumWorkers` | int | `1` | Number of albums processed **concurrently**. Controls how many albums are synced at the same time. Useful when you have many albums configured and want to process several in parallel. |
| `strictMetadata` | bool | `false` | Skip items with missing/invalid dates instead of uploading with current date. Skipped URLs are logged for manual review. |
| `skipVideos` | bool | `false` | Skip all video items entirely. Useful if you only want photos. |
| `dedupMode` | string | `safe` | How the persistent dedup state is reconciled with Immich. See [Dedup modes](#dedup-modes). |
| `updateExistingMetadata` | bool | `false` | Backfill the photographer/source-album description on assets that already exist in Immich. User-edited descriptions are left untouched. |
| `uiMode` | string | `standard` | Interface mode: `simple`, `standard`, or `advanced`. It only changes which Web UI sections are shown. A config with `accounts` opens in `advanced` when `uiMode` is empty. |
| `accounts` | array | `[]` | Immich user to Google account mappings. See [Accounts and routing](#accounts-and-routing). |
| `googleUpload.enabled` | bool | `false` | Enable the optional Immich → Google Photos direction. The per-album `uploadToGoogle` switch is the real opt-in. |
| `googleUpload.gotohpBinary` | string | `gotohp-cli` | Path to the pinned `gotohp-cli` binary. The Docker image ships `/app/gotohp-cli`. |
| `googleUpload.gotohpConfig` | string | `./data/gotohp.config` | Private file where `gotohp-cli` stores the Google credential. Mode 0600. |
| `googleUpload.webCookiesFile` | string | `./data/gphotos-web-cookies.txt` | Optional private cookies.txt for the photos.google.com browser session. Needed only for albums you do not own. Mode 0600. |
| `googleUpload.storageMode` | string | `quota` | `quota` counts uploads against Google storage. `unlimited` uses the unofficial mobile-API behaviour and is off by default. |
| `googleUpload.quality` | string | `original` | `original` or `saver`. |
| `googleUpload.skipLivePhotos` | bool | `false` | Upload only the still of a Live Photo instead of the linked still + video pair. |
| `googleUpload.reuploadDeleted` | bool | `false` | Off by default, so deleting a photo in Google Photos is respected. When on, each run checks the target album and uploads items that are no longer there. Requires a browser session. |
| `googleUpload.threads` | int | `1` | Concurrent uploads. Keep at 1 unless uploads are slow. |
| `googleUpload.strictAccountMatching` | bool | `false` | When on, Immich assets whose owner has no linked Google account are skipped. When off, they fall back to the account marked `isDefault`, or to the primary connected account. |
| `googleUpload.maxRetries` | int | `3` | Reserved. A failed asset is retried on the next scheduled run. |
| `googleUpload.autoUpdate` | bool | `false` | Reserved. Updating the pinned binary is done through the app image, not at runtime. |
| `notifications.immich` | bool | `false` | Send notifications through Immich's admin notification API. |
| `notifications.onSyncFailed` | bool | `false` | Notify when an album cannot be read or an item fails. |
| `notifications.onImportComplete` | bool | `false` | Notify when new photos were imported into Immich. |
| `notifications.onUploadComplete` | bool | `false` | Notify when an Immich album finished uploading to Google Photos. |

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
| `googlePhotos[].url` | string | none | Google Photos shared album link (required). |
| `googlePhotos[].albumName` | string | auto-detected | Override the album name in Immich. If omitted, uses the album title from Google Photos. |
| `googlePhotos[].syncInterval` | string | `24h` | How often to re-check this album (e.g. `12h`, `60m`, `1h30m`). |
| `googlePhotos[].immichAlbumId` | string | none | Link to an existing Immich album by UUID instead of creating a new one. Required when `uploadToGoogle` is enabled. |
| `googlePhotos[].immichAccountId` | string | primary | Import this album into Immich with the API key of one linked account. Use the account `id` or its Immich user UUID. Empty uses the primary `apiKey`. |
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

### Unlimited storage

The optional `unlimited` storage mode makes the uploader identify as a Pixel phone. Google then stores the upload without charging your storage quota. This is an unofficial mobile-API behavior, not a Google feature. There are no known reports of accounts being banned for using it, but Google can change or enforce this at any time. The default is `quota`; enable `unlimited` only if you accept that risk.

## Accounts and routing

The Web UI shows three experience modes, stored in `uiMode`:

- `simple`: Overview, Connection, Albums, and Logs only. Upload settings and account routing are hidden.
- `standard`: Adds sync behavior, deduplication, Google upload, private albums, and notifications for one Google account.
- `advanced`: Shows every section, including **Accounts & Users**, the per-album **Import into Immich as** selector, and strict contributor matching.

The mode only controls which sections the interface shows. Account mappings keep working in the background, and the UI shows a notice when mappings are active outside Advanced.

An account mapping links one Immich user to one Google Photos account.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `accounts[].id` | string | Immich user id | Stable mapping id. Used by `googlePhotos[].immichAccountId`. |
| `accounts[].name` | string | none | Display name, for example `Lotte`. |
| `accounts[].immichUserId` | string | none | Immich user UUID. It is matched against each asset's `ownerId` (required). |
| `accounts[].immichApiKey` | string | none | Optional Immich API key that imports Google to Immich albums as this person. |
| `accounts[].googleEmail` | string | none | Google account in `gotohp-cli` that uploads this person's photos. |
| `accounts[].webCookiesFile` | string | generated | Optional `cookies.txt` for shared albums that someone else owns. |
| `accounts[].isDefault` | bool | `false` | Fallback account for Immich users without a mapping. Only one mapping may be the default. |

A pasted `oauth_token` is stored by `gotohp-cli` in `googleUpload.gotohpConfig`, and a pasted `cookies.txt` is stored with mode 0600. The Web UI never shows those two values again. Account Immich API keys live in `config.json`, like the primary `apiKey`, so keep that file private.

### Why ownership matters

Immich assigns every imported asset to the owner of the API key that calls `POST /assets`. To import a shared album as your sister, use her Immich API key for that mapping, or choose her in `googlePhotos[].immichAccountId` for that album. The importer then uploads with her key and Immich records her as the owner.

In the other direction, the uploader reads each asset's `ownerId` and looks up the linked mapping. Photos owned by your sister upload with her Google account, and photos you own upload with yours. Set `googleUpload.strictAccountMatching` to skip owners without a mapping. When it is off, unmapped owners fall back to the account marked `isDefault`, or to the primary connected account when no mapping claims the fallback role.

## Notifications

Notifications are optional and sent through Immich's admin notification API. Enable **Notifications** in the Web UI and choose which events you want. The Immich API key needs `notification.create`, which Immich currently grants to admin keys with all permissions. The **Send test notification** button reports the exact error if the key is not allowed.

A failed notification never interrupts a sync. If Immich rejects the call, the error is written to the log and the sync continues.

## Private album import (experimental)

Private albums are the albums in your own Google Photos account. They use the same browser session as shared-album uploads: open **Google upload**, **Shared album access**, export a `cookies.txt` for `photos.google.com`, and connect it. The cookies file is stored at `googleUpload.webCookiesFile` with mode 0600.

Open **Private albums**, choose an album, pick an existing Immich album or create a new one, and press **Import**. The importer downloads the original files, skips items it already imported, and shows progress on the same page. It is manual only, has no schedule, and does not propagate deletions.

Limitations: Live Photo motion is imported as the still only. If Google asks for a fresh browser session, export the cookies again and reconnect. The feature is experimental.

## Web API Endpoints

When the Web UI is enabled, the embedded server provides the following endpoints:

| Endpoint | Method | Description |
| --- | --- | --- |
| `/api/sync` | `POST` / `GET` | Trigger an immediate sync run of all configured albums outside their normal sync interval. Returns `{"status":"ok","message":"Sync triggered"}`. |
| `/sync_now` | `POST` / `GET` | Alias for `/api/sync`, convenient for Home Assistant buttons or curl commands. |
| `/api/status` | `GET` | Return current album synchronization progress, Immich connection status, and `lastRun`/`nextRun` timestamps. |
| `/sync/status` | `GET` | Alias for `/api/status`. |
| `/api/logs` | `GET` | Return the latest raw application logs. |
| `/api/activity` | `GET` | Return the structured activity snapshot used by the dashboard. |
| `/api/live` | `GET` | Server-Sent Events stream with activity snapshots and structured events. |
| `/api/config` | `GET` / `POST` | Retrieve or update the application configuration. |
| `/api/immich/users` | `GET` | List Immich users for the account wizard dropdown. |
| `/api/accounts` | `GET` / `POST` / `DELETE` | Read account mappings and the routing preview, save one mapping, or delete one by `?id=<id>` or `{"id":"..."}`. |
| `/api/notifications/test` | `POST` | Send one test notification through Immich. |
| `/api/google/account` | `GET` / `POST` / `DELETE` | Read, connect, or remove the gotohp Google credential. |
| `/api/google/web-session` | `GET` / `POST` / `DELETE` | Read, connect, or remove the optional photos.google.com browser session. |
| `/api/google/albums` | `GET` | List Immich albums for the upload mapping dropdown. |
| `/api/google/verify` | `POST` | Resolve a share URL and classify the album as owned or shared. |
| `/api/google/upload` | `POST` | Run the Immich to Google Photos upload for one configured album. |
| `/api/google/private/albums` | `GET` | List private albums in the connected Google account. |
| `/api/google/private/import` | `POST` | Start importing one private album into Immich. |
| `/api/google/private/status` | `GET` | Return the private import job status. |
| `/api/google/private/cancel` | `POST` | Cancel the running private import. |
