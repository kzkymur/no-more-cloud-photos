# NMCP Photos (mobile)

Automatically uploads the photos and videos on a phone to your own
no-more-cloud-photos server (`POST /media` of the Core API). Originals are sent
byte-for-byte: no re-encoding, no metadata stripping (HEIC stays HEIC, GPS EXIF
is kept).

- UI: React Native (Expo SDK 58), `App.tsx`.
- Upload core: local Expo module `modules/nmcp-uploader`, native Kotlin
  (`android/`) and Swift (`ios/`). Scanning, the "already uploaded" ledger,
  background scheduling and the HTTP upload all run natively so they keep working
  when the UI is not running.
- JS ↔ native contract: `modules/nmcp-uploader/src/NmcpUploader.types.ts`.

## Using the app

1. Install the APK (see "Android build" below). Android warns about installing
   from outside the Play Store; allow it for the app you downloaded it with.
2. Enter the server URL, e.g. `https://photos.<tailnet>.ts.net`. The app calls
   `GET /readyz`; you can only save the URL once that answers 2xx.
3. Allow access to **all** photos and videos. On Android also allow location
   access for photos (otherwise Android removes GPS from the files the app
   reads). If you pick "selected photos only" the app shows a warning and only
   uploads what you selected.
4. The dashboard shows total / uploaded / pending / failed, the file in
   progress, and the last error. "今すぐ同期" (sync now) runs immediately and
   ignores the Wi-Fi/charging conditions; "失敗を再試行" (retry failed) queues
   every failed item again.

The server has no authentication: it must only be reachable over your tailnet
(or LAN). The phone needs to be on the tailnet (Tailscale app connected) to
upload.

## How uploading works

Each library item is one upload (Live Photos on iOS are two: the still and the
paired video).

1. The file is read once to compute its SHA-256 and exact length.
2. It is sent as a single-part `multipart/form-data` body:

   ```
   --nmcp-<32 hex>\r\n
   Content-Disposition: form-data; name="file"; filename="<ascii fallback>"; filename*=UTF-8''<RFC 8187 percent-encoded name>\r\n
   Content-Type: <media type>\r\n
   \r\n
   <original bytes>\r\n
   --nmcp-<32 hex>--\r\n
   ```

   Server-side contract test: `internal/httpapi/mobile_client_contract_test.go`.
3. `Idempotency-Key: nmcp1-<hex sha256("<platform>|<item id>|<content sha256 hex>")>`.
   The key is stable for the same item and content. A retry after a lost
   response is therefore replayed by the server (`Idempotency-Replayed: true`)
   instead of creating a duplicate. If a file is edited in place its content
   hash changes, so it gets a new key and is uploaded as a new original instead
   of hitting `409 idempotency_conflict`.
4. How responses are handled:
   - `2xx`: uploaded.
   - `409 duplicate_media`: the same bytes are already on the server, so the
     item counts as uploaded.
   - `401`/`403`/`404`/`405` (wrong URL or a proxy in front of the API) and
     `429`/`502`/`503`/`504` (server busy or down): the run stops. The item stays
     pending without using up an attempt, and the error is shown on the dashboard.
   - `408`, other `5xx`, connection drop mid-transfer: transient failure,
     retried automatically on later runs (up to 10 attempts).
   - other `4xx` (e.g. `415 unsupported_media_type`, `422 invalid_media`,
     `413`): marked "rejected by server". It is not retried automatically,
     only by "retry failed".
   - server unreachable: the run stops and is retried with backoff. The item
     stays pending.
5. The local SQLite ledger records every item and its state. An uploaded item
   is never sent again unless the file changes.

### Android

- Scan: MediaStore images + videos (`VOLUME_EXTERNAL`, `IS_PENDING = 0`).
  Bytes are read through `MediaStore.setRequireOriginal()` when
  `ACCESS_MEDIA_LOCATION` is granted.
- Scheduling (WorkManager):
  - `nmcp-trigger`: a MediaStore content-URI trigger that fires after new
    photos/videos appear.
  - `nmcp-periodic`: an hourly safety net that survives reboots.
  - `nmcp-auto`: the upload run, with the Wi-Fi-only / charging-only
    constraints.
  - `nmcp-manual`: "sync now", which only needs a network.

  Runs are serialized. A run shows a foreground notification (type `dataSync`)
  with progress. The app also syncs when it comes to the foreground.
- Permissions:
  - Android 13+: `READ_MEDIA_IMAGES`/`READ_MEDIA_VIDEO`.
  - Android 14+: `READ_MEDIA_VISUAL_USER_SELECTED`, which grants partial
    access and triggers the warning.
  - Android ≤ 12: `READ_EXTERNAL_STORAGE`.
  - Also requested: `ACCESS_MEDIA_LOCATION` and `POST_NOTIFICATIONS`.
- Cleartext HTTP is allowed, because tailnet traffic is already encrypted by
  WireGuard and `http://100.x.y.z:port` must work.

### iOS

- Scan: PhotoKit. Only the original resources are sent: `.photo` (plus
  `.alternatePhoto` for RAW+JPEG), `.pairedVideo` for Live Photos, and
  `.video`. Edits (`fullSize*`) are never sent.
- Export: `PHAssetResourceManager.writeData` with `isNetworkAccessAllowed`,
  so iCloud-only originals are downloaded first.
- The multipart body is written to a temporary file and sent with a
  background `URLSession` upload task, which continues while the app is
  suspended.
- `BGTaskScheduler` refresh/processing tasks and a photo library change
  observer pick up new photos. The app also syncs when it comes to the
  foreground.
- Workflow **Mobile iOS compile** (`.github/workflows/mobile-ios.yml`)
  builds the app for the simulator without signing, so every change to the
  Swift code is compiled in CI.
- Build and distribution for iOS are decided separately (Apple Developer
  account required).

## Android build

The APK is built by the GitHub Actions workflow **Mobile Android**
(`.github/workflows/mobile-android.yml`) on every PR or `main` push that
touches `mobile/`, and on manual dispatch. Download the artifact
`nmcp-photos-android` from the run.

The APK is always signed with the permanent release key. The workflow fails
instead of producing a debug-signed build. Repository secrets:

| Secret | Content |
|---|---|
| `NMCP_ANDROID_KEYSTORE_BASE64` | base64 of the PKCS12 release keystore |
| `NMCP_ANDROID_KEYSTORE_PASSWORD` | store password (also the key password) |
| `NMCP_ANDROID_KEY_ALIAS` | key alias |

Release certificate SHA-256:
`11:9E:F3:C6:B5:ED:68:92:2B:E2:9F:EE:BF:F4:70:1C:BD:88:05:04:BA:C7:3D:F3:47:B9:85:48:C4:24:CD:90`
(the workflow verifies it). An update signed with any other key cannot be
installed over an existing install, so the key must never change.

Bump `expo.version` and `expo.android.versionCode` in `app.json` for each
release you hand out.

Local development (needs JDK 17 and the Android SDK):

```sh
cd mobile
npm ci
npx expo prebuild --platform android --clean   # android/ is generated, never edited
cd android && ./gradlew assembleRelease
```

`android/` and `ios/` are generated by `expo prebuild` (Continuous Native
Generation) and are git-ignored. Native configuration lives in `app.json`,
`plugins/withReleaseSigning.js`, and the module's own `AndroidManifest.xml` /
podspec.
