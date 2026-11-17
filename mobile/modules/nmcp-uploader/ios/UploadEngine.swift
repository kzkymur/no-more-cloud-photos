import Foundation
import Photos
import os

private let engineLog = Logger(subsystem: "io.nmcp.uploader", category: "engine")

/// Snapshot of in-memory progress for `getStats`.
struct EngineSnapshot {
  let running: Bool
  let currentName: String?
  let currentSent: Int64
  let currentTotal: Int64
}

/// Owns the upload pipeline and the background URLSession.
///
/// Threads / queues:
/// - `pipelineQueue` (serial): scanning, picking the next item, exporting and
///   staging it, creating the upload task. Only this queue creates tasks, and
///   at most one task is in flight, so runs can never overlap.
/// - `delegateDispatchQueue` (serial): URLSession delegate callbacks.
/// - `lock`: guards every mutable property below.
///
/// Uploads run out of process (nsurlsessiond). When a task finishes while the
/// app is suspended or terminated, iOS relaunches it in the background,
/// `NmcpAppDelegateSubscriber` hands over the events completion handler, the
/// session is recreated, the completion is recorded and the next item is
/// staged and submitted before the handler is called.
///
/// Queue order and interruptions: see `UploadStore.nextCandidate`. Staging
/// counts as an interrupt until it reaches a result, and a transfer cut off
/// by iOS counts as one, so a file that never fits into the background time
/// windows moves behind the rest of the queue instead of blocking it.
final class UploadEngine: NSObject, URLSessionDataDelegate, @unchecked Sendable {
  static let shared = UploadEngine()
  static let sessionIdentifier = "io.nmcp.uploader.bg"
  static let maxAttempts = 10

  private static let maxResponseBytes = 64 * 1024
  /// Free space that must remain while staging (the size of an original is
  /// not known up front, so it is re-checked every `spaceCheckInterval`).
  private static let freeSpaceMargin: Int64 = 256 * 1024 * 1024
  private static let spaceCheckInterval = 64 * 1024 * 1024
  /// An export (iCloud download or local read) without any data or progress
  /// for this long is given up for this run.
  private static let exportStallTimeout: TimeInterval = 120
  private static let reconcileRetryDelay: TimeInterval = 30
  /// iOS allows ~30 s after a background session wake-up.
  private static let backgroundEventsTimeout: TimeInterval = 25

  private let store = UploadStore.shared
  private let scanner = LibraryScanner()
  private let pipelineQueue = DispatchQueue(label: "io.nmcp.uploader.pipeline", qos: .utility)
  private let delegateDispatchQueue = DispatchQueue(label: "io.nmcp.uploader.session-delegate", qos: .utility)
  private let delegateQueue: OperationQueue

  private let lock = NSLock()
  // MARK: State guarded by `lock`
  private var session: URLSession?
  private var sessionInvalidating = false
  /// Set once the tasks surviving from a previous process were reconciled
  /// with the DB; no new task is created before that.
  private var reconciled = false
  private var didInitialCleanup = false
  private var runActive = false
  private var runStartedAt: Double = 0
  /// Bumped by cancel / background expiration; long operations started under
  /// an older epoch abort.
  private var epoch = 0
  private var preparingItemId: String?
  private var inFlightTaskId: Int?
  private var inFlightItemId: String?
  private var currentName: String?
  private var currentSent: Int64 = 0
  private var currentTotal: Int64 = 0
  private var responseBodies: [Int: Data] = [:]
  private var eventsHandler: (token: UUID, handler: () -> Void)?
  private var lastEventsFinishedAt: Date?

  private override init() {
    let queue = OperationQueue()
    queue.name = "io.nmcp.uploader.session-delegate"
    queue.maxConcurrentOperationCount = 1
    delegateQueue = queue
    super.init()
    queue.underlyingQueue = delegateDispatchQueue
  }

  // MARK: - Public API

  /// Idempotent. Creates the background session (which also re-attaches to
  /// tasks from a previous process) as early as possible.
  func activate() {
    _ = obtainSession()
  }

  /// Schedules work on the pipeline.
  /// - scan: rescan the library first.
  /// - upload: start (or continue) an upload run.
  /// - onSettled: called once the scan is done and the next upload has been
  ///   submitted, or the run ended.
  func requestRun(scan: Bool, upload: Bool, onSettled: (() -> Void)? = nil) {
    activate()
    let requestEpoch: Int = lock.locked {
      if upload, !runActive {
        runActive = true
        runStartedAt = Date().timeIntervalSince1970
        RuntimeState.lastRunError = nil
      }
      return epoch
    }
    pipelineQueue.async { [self] in
      if scan {
        performScan(epoch: requestEpoch)
      }
      if upload {
        startNextIfIdle()
      }
      onSettled?()
    }
  }

  /// Cancels the in-flight upload (the item goes back to pending) and stops
  /// the run. Staging in progress is abandoned.
  func cancel() {
    let current: URLSession? = lock.locked {
      runActive = false
      epoch += 1
      return session
    }
    RuntimeState.lastRunAt = RuntimeState.nowMillis
    current?.getAllTasks { tasks in
      tasks.forEach { $0.cancel() }
    }
  }

  /// BGTask expiration or background time (session wake-up, app moved to
  /// the background) running out: abort scanning/staging immediately. The
  /// item being staged keeps its interrupt (attempts unchanged) and the run
  /// resumes with the next BGTask, session event or foreground. An upload
  /// that is already in nsurlsessiond keeps going.
  func backgroundTimeExpired() {
    lock.locked {
      epoch += 1
      if inFlightTaskId == nil {
        runActive = false
      }
    }
  }

  func configDidChange(from old: UploaderConfig, to new: UploaderConfig) {
    if old.autoUpload, !new.autoUpload {
      cancel()
    }
    if old.wifiOnly != new.wifiOnly {
      // Network policy lives in the session configuration, which is fixed at
      // creation: invalidate (cancelling the current task, which is re-queued)
      // and lazily recreate with the new policy.
      recreateSession()
    } else if UploaderConfig.normalize(serverUrl: old.serverUrl) != UploaderConfig.normalize(serverUrl: new.serverUrl) {
      // The in-flight request targets the old server.
      let current = lock.locked { session }
      current?.getAllTasks { tasks in
        tasks.forEach { $0.cancel() }
      }
    }
  }

  func snapshot() -> EngineSnapshot {
    lock.locked {
      EngineSnapshot(
        running: runActive || inFlightTaskId != nil,
        currentName: currentName,
        currentSent: currentSent,
        currentTotal: currentTotal
      )
    }
  }

  /// From `application(_:handleEventsForBackgroundURLSession:completionHandler:)`.
  func handleBackgroundEvents(_ handler: @escaping () -> Void) {
    let token = UUID()
    let (previous, eventsAlreadyFinished): ((() -> Void)?, Bool) = lock.locked {
      let prior = eventsHandler?.handler
      eventsHandler = (token, handler)
      let recent = lastEventsFinishedAt.map { Date().timeIntervalSince($0) < 10 } ?? false
      return (prior, recent)
    }
    if let previous {
      // A newer wake-up supersedes the old one; never leave a handler uncalled.
      DispatchQueue.main.async { previous() }
    }

    // Recreating the session is what makes iOS deliver the pending events.
    activate()
    let config = ConfigStore.load()
    if config.isAutoUploadReady {
      // Woken (not relaunched) processes still need an active run so the
      // completion chains into the next upload.
      requestRun(scan: false, upload: true)
    }
    if eventsAlreadyFinished {
      // urlSessionDidFinishEvents raced ahead of this call.
      pipelineQueue.async { [self] in
        flushEventsHandler(token: nil)
      }
    }
    DispatchQueue.main.asyncAfter(deadline: .now() + Self.backgroundEventsTimeout) { [weak self] in
      self?.flushEventsHandler(token: token)
    }
  }

  // MARK: - Session

  private func obtainSession() -> URLSession? {
    let (result, created): (URLSession?, Bool) = lock.locked {
      if sessionInvalidating {
        return (nil, false)
      }
      if let session {
        return (session, false)
      }
      let fresh = makeSession()
      session = fresh
      reconciled = false
      return (fresh, true)
    }
    if created, let result {
      reconcile(result)
    }
    return result
  }

  /// Call with `lock` held.
  private func makeSession() -> URLSession {
    let config = ConfigStore.load()
    let configuration = URLSessionConfiguration.background(withIdentifier: Self.sessionIdentifier)
    configuration.sessionSendsLaunchEvents = true
    configuration.isDiscretionary = false
    configuration.allowsCellularAccess = !config.wifiOnly
    configuration.allowsExpensiveNetworkAccess = !config.wifiOnly
    configuration.allowsConstrainedNetworkAccess = !config.wifiOnly
    configuration.httpMaximumConnectionsPerHost = 1
    return URLSession(configuration: configuration, delegate: self, delegateQueue: delegateQueue)
  }

  private func recreateSession() {
    let current: URLSession? = lock.locked {
      guard let session, !sessionInvalidating else {
        return nil
      }
      sessionInvalidating = true
      return session
    }
    current?.invalidateAndCancel()
  }

  /// Re-attaches to tasks that survived a relaunch and repairs DB rows whose
  /// task is gone (e.g. the app was force-quit, which cancels tasks).
  private func reconcile(_ session: URLSession) {
    session.getAllTasks { [weak self] tasks in
      guard let self else { return }
      self.delegateDispatchQueue.async {
        self.applyReconcile(session, tasks)
      }
    }
  }

  private func applyReconcile(_ session: URLSession, _ tasks: [URLSessionTask]) {
    guard lock.locked({ self.session === session }) else {
      return
    }
    // Without the ledger (e.g. data protection before the first unlock after
    // a reboot) every surviving task would look like a stray and be
    // cancelled. Leave all tasks alone and try again later; nothing is
    // created before `reconciled` is set.
    guard store.isAvailable() else {
      engineLog.error("Upload database unavailable; reconcile retried in \(Self.reconcileRetryDelay)s")
      delegateDispatchQueue.asyncAfter(deadline: .now() + Self.reconcileRetryDelay) { [weak self] in
        guard let self, self.lock.locked({ self.session === session && !self.reconciled }) else { return }
        self.reconcile(session)
      }
      return
    }
    let live = tasks.filter { $0.state == .running || $0.state == .suspended }
    // Tasks that finished (or are being cancelled) while we were not running
    // may still have their completion queued for delivery: keep their rows
    // in `uploading` so the completion is applied to them (the bytes may
    // already be on the server). If it never arrives, the next launch's
    // reconcile no longer sees the task and resets the row.
    let awaitingDelivery = Set(tasks.filter { $0.state == .completed || $0.state == .canceling }
      .compactMap { $0.taskDescription })
    var kept: (task: URLSessionTask, item: UploadItem)?
    for task in live {
      if kept == nil,
         let itemId = task.taskDescription,
         let item = store.item(id: itemId),
         item.state == .uploading,
         item.taskId == nil || item.taskId == task.taskIdentifier {
        kept = (task, item)
      } else {
        // Stray or duplicate task. Its row (if any) is reset below and counted
        // as interrupted; the late cancellation event is then ignored.
        task.cancel()
      }
    }
    var keepIds = awaitingDelivery
    if let kept {
      keepIds.insert(kept.item.id)
    }
    let reset = store.resetUploading(exceptItemIds: keepIds)

    let cleanup: Bool = lock.locked {
      let first = !didInitialCleanup
      didInitialCleanup = true
      return first
    }
    if cleanup {
      removeStaleStagingFiles(keeping: kept.map { NmcpPaths.bodyFile(forItemId: $0.item.id) })
    }

    lock.locked {
      reconciled = true
      if let kept {
        inFlightTaskId = kept.task.taskIdentifier
        inFlightItemId = kept.item.id
        currentName = kept.item.name
        currentSent = kept.task.countOfBytesSent
        currentTotal = max(kept.task.countOfBytesExpectedToSend, 0)
      }
    }
    if let kept, kept.task.state == .suspended {
      kept.task.resume()
    }
    engineLog.info("Reconciled session: \(live.count) live task(s), \(reset) lost upload(s) re-queued")
    kickIfActive()
  }

  /// Only runs before the first task of this process is created, so it can
  /// never delete a file that is being staged.
  private func removeStaleStagingFiles(keeping keep: URL?) {
    let fm = FileManager.default
    let dir = NmcpPaths.stagingDirectory
    guard let names = try? fm.contentsOfDirectory(atPath: dir.path) else {
      return
    }
    for name in names where name.hasPrefix("export-") || name.hasPrefix("body-") {
      let url = dir.appendingPathComponent(name)
      if let keep, url.lastPathComponent == keep.lastPathComponent {
        continue
      }
      try? fm.removeItem(at: url)
    }
  }

  // MARK: - Pipeline (pipelineQueue)

  private func kickIfActive() {
    if lock.locked({ runActive }) {
      pipelineQueue.async { [self] in
        startNextIfIdle()
      }
    }
  }

  private func isCurrent(epoch: Int) -> Bool {
    lock.locked { runActive && self.epoch == epoch }
  }

  private func performScan(epoch scanEpoch: Int) {
    let outcome = scanner.scan(into: store) {
      self.lock.locked { self.epoch == scanEpoch }
    }
    switch outcome {
    case .completed(let assets, let changed):
      engineLog.info("Scan: \(assets) assets, \(changed) new or changed")
    case .interrupted:
      engineLog.info("Scan interrupted")
    case .notAuthorized:
      engineLog.info("Scan skipped: no photo library access")
    }
  }

  private enum StageResult {
    case staged(StagedBody)
    /// The item reached a result (failed or deleted); move on to the next one.
    /// The kind feeds `ResourceFailureStreak` (nil = not a resource problem).
    case resolved(ExportFailureKind?, message: String?)
    /// Stop the whole run with this user-facing message; the item stays queued.
    case stop(String)
    /// Cut off (cancel, BGTask expiry, background time running out): the item
    /// stays queued and keeps the interrupt recorded by `markStagingStarted`.
    case aborted
  }

  private func startNextIfIdle() {
    var streak = ResourceFailureStreak()
    var resolvedIds = Set<String>()
    while true {
      let state: (runEpoch: Int, runStart: Double)? = lock.locked {
        guard runActive, inFlightTaskId == nil, preparingItemId == nil else {
          return nil
        }
        return (epoch, runStartedAt)
      }
      guard let state else {
        return
      }
      // A nil session means it is being invalidated; didBecomeInvalid kicks again.
      guard obtainSession() != nil, lock.locked({ reconciled }) else {
        return
      }

      let config = ConfigStore.load()
      guard let mediaURL = config.mediaEndpoint else {
        finishRun(error: "No server is configured. Enter the server URL in the app settings.")
        return
      }
      if config.chargingOnly, !DeviceState.shared.isCharging {
        finishRun(error: "Paused: waiting for the device to charge (\"Only while charging\" is on).")
        return
      }
      guard LibraryAccess.isReadable else {
        finishRun(error: "Photo library access is not granted. Allow access to Photos in Settings.")
        return
      }
      guard store.isAvailable() else {
        finishRun(error: "The local upload database is unavailable. Unlock the device and open the app.")
        return
      }
      guard let item = store.nextCandidate(failedBefore: state.runStart, maxAttempts: Self.maxAttempts) else {
        finishRun(error: nil)
        return
      }
      if resolvedIds.contains(item.id) {
        // Its result could not be written (the row would be picked forever).
        finishRun(error: "The local upload database could not be updated. Open the app to retry.")
        return
      }

      lock.locked {
        preparingItemId = item.id
        currentName = item.name
        currentSent = 0
        currentTotal = 0
      }
      // Pessimistic: stays counted if staging is cut off or the app is
      // suspended/killed before it finishes (see markStagingStarted).
      store.markStagingStarted(id: item.id)
      // Background time running out (session wake, app moved to background)
      // aborts staging like a BGTask expiry; the interrupt stays counted.
      let activity = BackgroundActivity.begin("nmcp.stage") { [weak self] in
        self?.backgroundTimeExpired()
      }
      let result = stage(item, epoch: state.runEpoch)
      if case .aborted = result {
        engineLog.info("Staging of \(item.id, privacy: .public) was cut off; it moves back in the queue")
      } else {
        store.markStagingEnded(id: item.id)
      }
      lock.locked {
        preparingItemId = nil
      }

      switch result {
      case .staged(let body):
        let submitted = submit(item: item, body: body, to: mediaURL, config: config, epoch: state.runEpoch)
        activity.end()
        if submitted {
          return
        }
        // The run, session or server changed while staging; the item is
        // still pending. Re-evaluate from the top (which exits if the run
        // was cancelled or the session is being replaced).
        clearProgress()
        continue
      case .resolved(let kind, let message):
        activity.end()
        resolvedIds.insert(item.id)
        clearProgress()
        if streak.record(kind) {
          finishRun(error: "Paused after several items could not be read: \(message ?? "unknown error"). "
                    + "Uploads will resume later.")
          return
        }
        continue
      case .stop(let message):
        activity.end()
        finishRun(error: message)
        return
      case .aborted:
        activity.end()
        clearProgress()
        return
      }
    }
  }

  /// Streams the original from PhotoKit straight into the multipart body
  /// file (single copy): header, each chunk as it arrives (hashed on the
  /// way), trailer. Only the original resource recorded by the scanner is
  /// read; edits/renders are never touched and nothing is re-encoded.
  private func stage(_ item: UploadItem, epoch: Int) -> StageResult {
    let shouldContinue = { self.isCurrent(epoch: epoch) }

    guard let asset = PHAsset.fetchAssets(withLocalIdentifiers: [item.assetId], options: nil).firstObject else {
      // Deleted from the library (or deselected under limited access) since
      // the scan: nothing to back up. Uploaded history is never deleted.
      store.delete(id: item.id)
      return .resolved(nil, message: nil)
    }
    guard let resource = PHAssetResource.assetResources(for: asset).first(where: { $0.type.rawValue == item.resourceType }) else {
      store.markFailed(id: item.id, error: "The original file is no longer available in the Photos library.",
                       permanent: true, countAttempt: true)
      return .resolved(.other, message: nil)
    }

    do {
      try NmcpPaths.ensureDirectory(NmcpPaths.stagingDirectory, excludeFromBackup: true)
    } catch {
      return .stop("Cannot create the upload staging folder: \(error.localizedDescription)")
    }
    // The size of an original is not public API, so require a fixed margin up
    // front; the writer re-checks while streaming and ENOSPC fails cleanly.
    if let free = Self.availableCapacity(), free < Self.freeSpaceMargin {
      let needed = ByteCountFormatter.string(fromByteCount: Self.freeSpaceMargin, countStyle: .file)
      return .stop("Not enough free storage to prepare uploads (at least \(needed) needed). Free up space and try again.")
    }

    let bodyURL = NmcpPaths.bodyFile(forItemId: item.id)
    let writer: MultipartBodyWriter
    do {
      writer = try MultipartBodyWriter(destination: bodyURL, name: item.name, mime: item.mime)
    } catch {
      return resolveStagingFailure(item, error: error)
    }

    switch streamOriginal(resource, into: writer, shouldContinue: shouldContinue) {
    case .aborted:
      return .aborted
    case .stalled:
      let message = "Downloading the original of \"\(item.name)\" from iCloud stalled. It will be retried later."
      store.markFailed(id: item.id, error: message, permanent: false, countAttempt: false)
      return .resolved(.network, message: message)
    case .failed(let error):
      return resolveStagingFailure(item, error: error)
    case .done(let body):
      return .staged(body)
    }
  }

  /// Records a failed export/staging. Device-wide causes (offline iCloud,
  /// full storage) do not use up an attempt; the item is retried on the next
  /// run and never blocks the rest of this one.
  private func resolveStagingFailure(_ item: UploadItem, error: Error) -> StageResult {
    let kind: ExportFailureKind = error is LowStorageError ? .outOfSpace : ExportFailureKind.classify(error)
    let message: String
    switch kind {
    case .outOfSpace:
      message = "Not enough free storage to prepare \"\(item.name)\". Free up space; it will be retried later."
    case .network:
      message = "Could not download the original from iCloud (\(error.localizedDescription)). It will be retried later."
    case .other:
      if case Multipart.StagingError.emptyPayload? = error as? Multipart.StagingError {
        message = "Photos returned an empty file for the original. It will be retried."
      } else {
        message = "Could not read the original from Photos: \(error.localizedDescription)"
      }
    }
    store.markFailed(id: item.id, error: message, permanent: false, countAttempt: kind == .other)
    return .resolved(kind, message: message)
  }

  private struct LowStorageError: Error {}

  private enum StreamResult {
    case done(StagedBody)
    case failed(Error)
    /// No data or download progress for `exportStallTimeout`.
    case stalled
    case aborted
  }

  /// State shared between the PhotoKit callbacks and the waiting pipeline.
  /// `lock` also serializes every use of the writer.
  private final class StreamBox: @unchecked Sendable {
    let lock = NSLock()
    var abandoned = false
    var completed = false
    var completionError: Error?
    var writeError: Error?
    var lastActivity = Date()
    var bytesSinceSpaceCheck = 0
  }

  private func streamOriginal(
    _ resource: PHAssetResource,
    into writer: MultipartBodyWriter,
    shouldContinue: () -> Bool
  ) -> StreamResult {
    let box = StreamBox()
    let semaphore = DispatchSemaphore(value: 0)
    let options = PHAssetResourceRequestOptions()
    // iCloud-only originals ("Optimize iPhone Storage") are downloaded.
    options.isNetworkAccessAllowed = true
    options.progressHandler = { _ in
      box.lock.locked { box.lastActivity = Date() }
    }
    let spaceCheckInterval = Self.spaceCheckInterval
    let margin = Self.freeSpaceMargin

    let manager = PHAssetResourceManager.default()
    let requestId = manager.requestData(for: resource, options: options, dataReceivedHandler: { data in
      box.lock.locked {
        guard !box.abandoned, !box.completed, box.writeError == nil else { return }
        box.lastActivity = Date()
        do {
          try writer.append(data)
          box.bytesSinceSpaceCheck += data.count
          if box.bytesSinceSpaceCheck >= spaceCheckInterval {
            box.bytesSinceSpaceCheck = 0
            if let free = UploadEngine.availableCapacity(), free < margin {
              throw LowStorageError()
            }
          }
        } catch {
          box.writeError = error
          semaphore.signal()
        }
      }
    }, completionHandler: { error in
      box.lock.locked {
        box.completed = true
        box.completionError = error
      }
      semaphore.signal()
    })

    // Stops the request and deletes the partial body.
    func abandon() {
      box.lock.locked {
        box.abandoned = true
        writer.discard()
      }
      manager.cancelDataRequest(requestId)
    }

    while true {
      _ = semaphore.wait(timeout: .now() + 1)
      let snapshot = box.lock.locked { (box.completed, box.writeError, box.lastActivity) }
      if let writeError = snapshot.1 {
        abandon()
        return .failed(writeError)
      }
      if snapshot.0 {
        break
      }
      if !shouldContinue() {
        abandon()
        return .aborted
      }
      if Date().timeIntervalSince(snapshot.2) > Self.exportStallTimeout {
        abandon()
        return .stalled
      }
    }

    if !shouldContinue() {
      abandon()
      return .aborted
    }
    // Only a request that completed without error yields a body: a partial
    // or failed iCloud download is never sent.
    return box.lock.locked { () -> StreamResult in
      box.abandoned = true // late callbacks (none expected) must not touch the writer
      if let error = box.completionError ?? box.writeError {
        writer.discard()
        return .failed(error)
      }
      do {
        return .done(try writer.finish())
      } catch {
        writer.discard()
        return .failed(error)
      }
    }
  }

  /// Creates and resumes the upload task. Returns false when the run changed
  /// while staging (the item stays pending).
  private func submit(item: UploadItem, body: StagedBody, to mediaURL: URL, config: UploaderConfig, epoch: Int) -> Bool {
    let target: URLSession? = lock.locked {
      guard runActive, self.epoch == epoch, reconciled, !sessionInvalidating, inFlightTaskId == nil else {
        return nil
      }
      return session
    }
    // The server URL or network policy may have changed while staging.
    let latest = ConfigStore.load()
    guard let target, latest.mediaEndpoint == mediaURL, latest.wifiOnly == config.wifiOnly else {
      try? FileManager.default.removeItem(at: body.url)
      return false
    }
    // A late success of an older task may have landed while staging.
    if let row = store.item(id: item.id), row.state == .uploaded {
      try? FileManager.default.removeItem(at: body.url)
      return false
    }

    var request = URLRequest(url: mediaURL)
    request.httpMethod = "POST"
    request.setValue(body.contentType, forHTTPHeaderField: "Content-Type")
    request.setValue("application/json", forHTTPHeaderField: "Accept")
    request.setValue(MultipartFormat.idempotencyKey(
      itemId: item.id, contentSha256: body.contentSha256, filename: body.name),
                     forHTTPHeaderField: "Idempotency-Key")
    // Mirrors the session policy; request-level flags can only restrict further.
    request.allowsCellularAccess = !config.wifiOnly
    request.allowsExpensiveNetworkAccess = !config.wifiOnly
    request.allowsConstrainedNetworkAccess = !config.wifiOnly

    let task = target.uploadTask(with: request, fromFile: body.url)
    task.taskDescription = item.id
    task.countOfBytesClientExpectsToSend = body.size
    task.countOfBytesClientExpectsToReceive = 4 * 1024
    store.markUploading(id: item.id, taskId: task.taskIdentifier, contentSha: body.contentSha256)
    lock.locked {
      inFlightTaskId = task.taskIdentifier
      inFlightItemId = item.id
      currentName = item.name
      currentSent = 0
      currentTotal = body.size
    }
    task.resume()
    engineLog.info("Uploading \(item.id, privacy: .public) (\(body.size) bytes) as task \(task.taskIdentifier)")
    return true
  }

  private func clearProgress() {
    lock.locked {
      if inFlightTaskId == nil {
        currentName = nil
        currentSent = 0
        currentTotal = 0
      }
    }
  }

  private func finishRun(error: String?) {
    lock.locked {
      runActive = false
      if inFlightTaskId == nil {
        currentName = nil
        currentSent = 0
        currentTotal = 0
      }
    }
    RuntimeState.lastRunAt = RuntimeState.nowMillis
    RuntimeState.lastRunError = error
    if let error {
      engineLog.info("Run stopped: \(error, privacy: .public)")
    } else {
      engineLog.info("Run finished: queue empty")
    }
  }

  /// Calls the background-session completion handler (on main). With a
  /// token, only that specific handler is flushed (timeout path).
  private func flushEventsHandler(token: UUID?) {
    let handler: (() -> Void)? = lock.locked {
      guard let entry = eventsHandler else {
        return nil
      }
      if let token, entry.token != token {
        return nil
      }
      eventsHandler = nil
      return entry.handler
    }
    if let handler {
      DispatchQueue.main.async { handler() }
    }
  }

  // MARK: - URLSessionDelegate

  func urlSession(_ session: URLSession, didBecomeInvalidWithError error: Error?) {
    lock.locked {
      if self.session === session {
        self.session = nil
      }
      sessionInvalidating = false
      reconciled = false
      inFlightTaskId = nil
      inFlightItemId = nil
    }
    engineLog.info("Session invalidated; it will be recreated on demand")
    kickIfActive()
  }

  func urlSessionDidFinishEvents(forBackgroundURLSession session: URLSession) {
    lock.locked {
      lastEventsFinishedAt = Date()
    }
    // Queued behind any completion-triggered pipeline work, so the next
    // upload is already submitted when iOS gets the handler back.
    pipelineQueue.async { [self] in
      flushEventsHandler(token: nil)
    }
  }

  // MARK: - URLSessionTaskDelegate

  func urlSession(
    _ session: URLSession,
    task: URLSessionTask,
    didSendBodyData bytesSent: Int64,
    totalBytesSent: Int64,
    totalBytesExpectedToSend: Int64
  ) {
    lock.locked {
      guard inFlightTaskId == task.taskIdentifier else { return }
      currentSent = totalBytesSent
      if totalBytesExpectedToSend > 0 {
        currentTotal = totalBytesExpectedToSend
      }
    }
  }

  func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
    let body: Data = lock.locked {
      responseBodies.removeValue(forKey: task.taskIdentifier) ?? Data()
    }
    let itemId = task.taskDescription ?? store.item(taskId: task.taskIdentifier)?.id
    let outcome = UploadClassifier.classify(response: task.response, error: error, body: body)

    // A late event for an older task of an item that has since been
    // re-submitted (possible after a relaunch race) must not clobber the
    // newer attempt or delete its body file. The store applies non-success
    // results only while the row is uploading through this very task.
    // Success is always recorded: the body was that item's original.
    let item = itemId.flatMap { store.item(id: $0) }
    let current = item.map { $0.state == .uploading && ($0.taskId == nil || $0.taskId == task.taskIdentifier) } ?? false
    let fileBusy: Bool = lock.locked {
      guard let itemId else { return false }
      return preparingItemId == itemId || (inFlightItemId == itemId && inFlightTaskId != task.taskIdentifier)
    }
    if let itemId, !fileBusy {
      try? FileManager.default.removeItem(at: NmcpPaths.bodyFile(forItemId: itemId))
    }

    var stopMessage: String?
    if let itemId, item != nil {
      let taskId = task.taskIdentifier
      switch outcome {
      case .uploaded(let mediaId):
        store.markUploaded(id: itemId, mediaId: mediaId)
        engineLog.info("Uploaded \(itemId, privacy: .public) media=\(mediaId ?? "-", privacy: .public)")
      case .interrupted:
        store.markInterrupted(id: itemId, taskId: taskId)
        engineLog.info("Upload of \(itemId, privacy: .public) was interrupted; it moves back in the queue")
      case .failed(let message, let permanent):
        store.markFailed(id: itemId, error: message, permanent: permanent, countAttempt: true, taskId: taskId)
        engineLog.error("Upload failed \(itemId, privacy: .public): \(message, privacy: .public)")
      case .stopRun(let message, let itemError, let interrupted):
        // Not the file's fault: no attempt is used up.
        if interrupted {
          store.markInterrupted(id: itemId, taskId: taskId)
        } else {
          store.markPending(id: itemId, taskId: taskId)
        }
        engineLog.error("Upload stopped \(itemId, privacy: .public): \(itemError, privacy: .public)")
        if current {
          stopMessage = message
        }
      }
    }

    lock.locked {
      if inFlightTaskId == task.taskIdentifier {
        inFlightTaskId = nil
        inFlightItemId = nil
        currentName = nil
        currentSent = 0
        currentTotal = 0
      }
    }

    if let stopMessage {
      finishRun(error: stopMessage)
    } else {
      kickIfActive()
    }
  }

  // MARK: - URLSessionDataDelegate

  func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
    lock.locked {
      var buffer = responseBodies[dataTask.taskIdentifier] ?? Data()
      if buffer.count < Self.maxResponseBytes {
        buffer.append(data.prefix(Self.maxResponseBytes - buffer.count))
      }
      responseBodies[dataTask.taskIdentifier] = buffer
    }
  }

  static func describeTransportError(_ error: NSError) -> String {
    UploadClassifier.describeTransportError(error)
  }

  // MARK: - File helpers

  private static func availableCapacity() -> Int64? {
    let values = try? NmcpPaths.stagingDirectory.resourceValues(forKeys: [.volumeAvailableCapacityForImportantUsageKey])
    return values?.volumeAvailableCapacityForImportantUsage
  }
}
