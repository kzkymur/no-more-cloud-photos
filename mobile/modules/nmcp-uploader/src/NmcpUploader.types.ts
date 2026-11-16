/**
 * Contract between the React Native UI and the native uploader core
 * (Kotlin on Android, Swift on iOS). Both platforms implement exactly
 * these functions with exactly these shapes.
 */

export type UploaderConfig = {
  /** Base URL of the Core API, e.g. https://photos.example.ts.net (no trailing slash). Empty = not configured. */
  serverUrl: string;
  /** Automatic background upload enabled. */
  autoUpload: boolean;
  /** Only upload on unmetered (Wi-Fi) networks. */
  wifiOnly: boolean;
  /** Only upload while charging. */
  chargingOnly: boolean;
};

export type ServerCheck = {
  ok: boolean;
  /** HTTP status, 0 when the request did not complete. */
  status: number;
  /** Short human readable detail (response body excerpt or error message). */
  message: string;
};

/**
 * full        = whole library readable (and on Android ACCESS_MEDIA_LOCATION granted or not needed)
 * partial     = user granted only selected photos (Android 14+ / iOS limited)
 * denied      = refused; must be fixed in system settings
 * undetermined = never asked
 */
export type LibraryAccess = 'full' | 'partial' | 'denied' | 'undetermined';

export type PermissionState = {
  library: LibraryAccess;
  /** Android: ACCESS_MEDIA_LOCATION granted (GPS EXIF kept). iOS: always true. */
  mediaLocation: boolean;
  /** Notifications allowed (progress notification on Android). */
  notifications: boolean;
};

export type UploadStats = {
  /** Items known in the local DB (photos, videos, Live Photo paired videos count separately). */
  total: number;
  uploaded: number;
  pending: number;
  failed: number;
  /** True while an upload run is active. */
  running: boolean;
  /** Display name of the item currently being sent, or null. */
  currentName: string | null;
  /** Bytes sent / total bytes for the current item (0/0 when idle). */
  currentSent: number;
  currentTotal: number;
  /** Epoch millis of the last completed library scan, 0 if never. */
  lastScanAt: number;
  /** Epoch millis of the last finished run, 0 if never. */
  lastRunAt: number;
  /** Error that stopped the last run (e.g. server unreachable), or null. */
  lastRunError: string | null;
};

export type FailedItem = {
  id: string;
  name: string;
  error: string;
  attempts: number;
  /** true = server rejected the content (4xx); automatic retries are stopped. */
  permanent: boolean;
};

export type NmcpUploaderApi = {
  getConfig(): Promise<UploaderConfig>;
  /** Persists natively and (re)schedules background work. */
  setConfig(config: UploaderConfig): Promise<void>;
  /** GET {url}/readyz with Accept: application/json. */
  checkServer(url: string): Promise<ServerCheck>;
  getPermissions(): Promise<PermissionState>;
  /** Shows the system dialogs. Resolves with the resulting state. */
  requestPermissions(): Promise<PermissionState>;
  /** Opens the app's page in system settings. */
  openSettings(): Promise<void>;
  /** Android 14+: re-open the photo picker to extend a partial selection. iOS: presentLimitedLibraryPicker. */
  extendPartialAccess(): Promise<void>;
  /** Scan the library and start (or continue) uploading in the background-capable executor. */
  syncNow(): Promise<void>;
  /** Re-queue all failed items (including permanent ones) and start a run. */
  retryFailed(): Promise<void>;
  /** Cancel the current run (items stay pending). */
  cancel(): Promise<void>;
  getStats(): Promise<UploadStats>;
  listFailed(limit: number): Promise<FailedItem[]>;
};
