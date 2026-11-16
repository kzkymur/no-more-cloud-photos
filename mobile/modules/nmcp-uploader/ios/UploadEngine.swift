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
final class UploadEngine: NSObject, URLSessionDataDelegate, @unchecked Sendable {
  static let shared = UploadEngine()
  static let sessionIdentifier = "io.nmcp.uploader.bg"
  static let maxAttempts = 10

  private static let maxResponseBytes = 64 * 1024
  /// Extra free space required on top of the staged body size.
  private static let freeSpaceMargin: Int64 = 64 * 1024 * 1024
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

  /// BGTask expiration: abort scanning/staging immediately. An upload that
  /// is already in nsurlsessiond keeps going.
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
    let live = tasks.filter { $0.state == .running || $0.state == .suspended }
    var kept: (task: URLSessionTask, item: UploadItem)?
    for task in live {
      if kept == nil,
         let itemId = task.taskDescription,
         let item = store.item(id: itemId),
         item.state == .uploading {
        kept = (task, item)
      } else {
        // Stray or duplicate task: cancelling re-queues its item.
        task.cancel()
      }
    }
    let keepIds: Set<String> = kept.map { Set([$0.item.id]) } ?? Set<String>()
    store.resetUploading(exceptItemIds: keepIds)

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
    engineLog.info("Reconciled session: \(live.count) live task(s)")
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
    /// Item resolved (failed/deleted); move on to the next one.
    case next
    /// Stop the whole run with this user-facing message.
    case stop(String)
    /// Cancelled / expired; leave the item pending.
    case aborted
  }

  private func startNextIfIdle() {
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

      lock.locked {
        preparingItemId = item.id
        currentName = item.name
        currentSent = 0
        currentTotal = 0
      }
      let activity = BackgroundActivity.begin("nmcp.stage")
      let result = stage(item, epoch: state.runEpoch)
      activity.end()
      lock.locked {
        preparingItemId = nil
      }

      switch result {
      case .staged(let body):
        if submit(item: item, body: body, to: mediaURL, config: config, epoch: state.runEpoch) {
          return
        }
        // The run, session or server changed while staging; the item is
        // still pending. Re-evaluate from the top (which exits if the run
        // was cancelled or the session is being replaced).
        clearProgress()
        continue
      case .next:
        clearProgress()
        continue
      case .stop(let message):
        finishRun(error: message)
        return
      case .aborted:
        clearProgress()
        return
      }
    }
  }

  /// Exports the original from PhotoKit and builds the multipart body file.
  private func stage(_ item: UploadItem, epoch: Int) -> StageResult {
    let shouldContinue = { self.isCurrent(epoch: epoch) }

    guard let asset = PHAsset.fetchAssets(withLocalIdentifiers: [item.assetId], options: nil).firstObject else {
      // Deleted from the library since the scan: nothing to back up.
      store.delete(id: item.id)
      return .next
    }
    guard let resource = PHAssetResource.assetResources(for: asset).first(where: { $0.type.rawValue == item.resourceType }) else {
      store.markFailed(id: item.id, error: "The original file is no longer available in the Photos library.",
                       permanent: true, countAttempt: true)
      return .next
    }

    do {
      try NmcpPaths.ensureDirectory(NmcpPaths.stagingDirectory, excludeFromBackup: true)
    } catch {
      return .stop("Cannot create the upload staging folder: \(error.localizedDescription)")
    }

    let exportURL = NmcpPaths.exportFile()
    defer { try? FileManager.default.removeItem(at: exportURL) }

    switch exportOriginal(resource, to: exportURL, shouldContinue: shouldContinue) {
    case .aborted:
      return .aborted
    case .failed(let error):
      if Self.isOutOfSpace(error) {
        return .stop("Not enough free storage to download \"\(item.name)\" from iCloud. Free up space and try again.")
      }
      store.markFailed(
        id: item.id,
        error: "Could not read the original from Photos (it may need to download from iCloud): \(error.localizedDescription)",
        permanent: false,
        countAttempt: true
      )
      return .next
    case .done:
      break
    }

    // The body is a second full copy of the file; make sure it fits.
    let exportSize = Self.fileSize(exportURL)
    if let free = Self.availableCapacity(), free < exportSize + Self.freeSpaceMargin {
      let needed = ByteCountFormatter.string(fromByteCount: exportSize + Self.freeSpaceMargin, countStyle: .file)
      return .stop("Not enough free storage to prepare \"\(item.name)\" (\(needed) needed). Free up space and try again.")
    }

    let bodyURL = NmcpPaths.bodyFile(forItemId: item.id)
    do {
      let body = try Multipart.stage(
        source: exportURL,
        destination: bodyURL,
        name: item.name,
        mime: item.mime,
        shouldContinue: shouldContinue
      )
      return .staged(body)
    } catch Multipart.StagingError.cancelled {
      try? FileManager.default.removeItem(at: bodyURL)
      return .aborted
    } catch {
      try? FileManager.default.removeItem(at: bodyURL)
      if Self.isOutOfSpace(error) {
        return .stop("Not enough free storage to prepare \"\(item.name)\". Free up space and try again.")
      }
      store.markFailed(id: item.id, error: "Could not prepare the upload: \(error.localizedDescription)",
                       permanent: false, countAttempt: true)
      return .next
    }
  }

  private enum ExportResult {
    case done
    case failed(Error)
    case aborted
  }

  /// Shares the export outcome between the PhotoKit callback and the
  /// (possibly abandoned) waiter.
  private final class ExportBox: @unchecked Sendable {
    private let lock = NSLock()
    private var finished = false
    private var abandoned = false
    private(set) var error: Error?

    /// Returns false when the waiter has already given up.
    func complete(_ error: Error?) -> Bool {
      lock.locked {
        finished = true
        self.error = error
        return !abandoned
      }
    }

    /// Returns true when the export already finished (nothing to abandon).
    func abandon() -> Bool {
      lock.locked {
        abandoned = true
        return finished
      }
    }
  }

  /// Writes the original bytes to `url`. Network access is allowed so
  /// iCloud-only originals are downloaded first.
  private func exportOriginal(_ resource: PHAssetResource, to url: URL, shouldContinue: () -> Bool) -> ExportResult {
    let options = PHAssetResourceRequestOptions()
    options.isNetworkAccessAllowed = true
    let box = ExportBox()
    let semaphore = DispatchSemaphore(value: 0)
    PHAssetResourceManager.default().writeData(for: resource, toFile: url, options: options) { error in
      if !box.complete(error) {
        // The waiter gave up (cancel/expiry); clean up the late file.
        try? FileManager.default.removeItem(at: url)
      }
      semaphore.signal()
    }
    while semaphore.wait(timeout: .now() + 1) == .timedOut {
      if !shouldContinue() {
        if !box.abandon() {
          return .aborted
        }
        break // finished in the meantime
      }
    }
    if !shouldContinue() {
      return .aborted
    }
    if let error = box.error {
      return .failed(error)
    }
    return .done
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

    var request = URLRequest(url: mediaURL)
    request.httpMethod = "POST"
    request.setValue(body.contentType, forHTTPHeaderField: "Content-Type")
    request.setValue("application/json", forHTTPHeaderField: "Accept")
    request.setValue(Multipart.idempotencyKey(itemId: item.id, contentSha256: body.contentSha256),
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
    let outcome = Self.classify(response: task.response, error: error, body: body)

    // A late event for an older task of an item that has since been
    // re-submitted (possible after a relaunch race) must not clobber the
    // newer task's row or delete its body file. Success is always recorded.
    let item = itemId.flatMap { store.item(id: $0) }
    let superseded = item.map { $0.state == .uploading && $0.taskId != nil && $0.taskId != task.taskIdentifier } ?? false
    let fileBusy: Bool = lock.locked {
      guard let itemId else { return false }
      return preparingItemId == itemId || (inFlightItemId == itemId && inFlightTaskId != task.taskIdentifier)
    }
    if let itemId, !fileBusy {
      try? FileManager.default.removeItem(at: NmcpPaths.bodyFile(forItemId: itemId))
    }

    if let itemId, item != nil {
      switch outcome {
      case .uploaded(let mediaId):
        store.markUploaded(id: itemId, mediaId: mediaId)
        engineLog.info("Uploaded \(itemId, privacy: .public) media=\(mediaId ?? "-", privacy: .public)")
      case .requeue:
        if !superseded {
          store.markPending(id: itemId)
        }
      case .failed(let message, let permanent, _):
        if !superseded {
          store.markFailed(id: itemId, error: message, permanent: permanent, countAttempt: true)
        }
        engineLog.error("Upload failed \(itemId, privacy: .public): \(message, privacy: .public)")
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

    if case .failed(_, _, let stopRun?) = outcome, !superseded {
      finishRun(error: stopRun)
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

  // MARK: - Response classification

  enum Outcome {
    case uploaded(mediaId: String?)
    /// Cancelled (by us or the system); back to pending, no attempt counted.
    case requeue
    /// stopRun != nil ends the run with that message (server-wide problem).
    case failed(message: String, permanent: Bool, stopRun: String?)
  }

  static func classify(response: URLResponse?, error: Error?, body: Data) -> Outcome {
    if let error {
      let nsError = error as NSError
      if nsError.domain == NSURLErrorDomain, nsError.code == NSURLErrorCancelled {
        return .requeue
      }
      let message = describeTransportError(nsError)
      let unreachable = nsError.domain == NSURLErrorDomain && unreachableCodes.contains(nsError.code)
      return .failed(message: message, permanent: false,
                     stopRun: unreachable ? "Server unreachable: \(message)" : nil)
    }
    guard let http = response as? HTTPURLResponse else {
      return .failed(message: "The server did not send a valid HTTP response.", permanent: false, stopRun: nil)
    }

    let status = http.statusCode
    let json = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any]
    let apiError = json?["error"] as? [String: Any]
    let code = apiError?["code"] as? String
    let apiMessage = apiError?["message"] as? String
    let message = httpMessage(status: status, code: code, apiMessage: apiMessage, body: body)

    switch status {
    case 200..<300:
      let media = json?["media"] as? [String: Any]
      return .uploaded(mediaId: stringValue(media?["id"]))
    case 409 where code == "duplicate_media":
      let details = apiError?["details"] as? [String: Any]
      return .uploaded(mediaId: stringValue(details?["existing_media_id"]))
    case 429, 502, 503, 504:
      // Server-wide back-pressure / outage: retry later instead of burning
      // an attempt on every queued item.
      return .failed(message: message, permanent: false,
                     stopRun: "The server is busy or unavailable (\(message)). Uploads will resume later.")
    case 408, 500...599:
      return .failed(message: message, permanent: false, stopRun: nil)
    case 401, 403, 404, 405:
      // These describe the endpoint/credentials, not the file: marking every
      // item permanently failed because of a wrong URL or proxy would be wrong.
      return .failed(message: message, permanent: false,
                     stopRun: "The server rejected the upload endpoint (\(message)). Check the server URL.")
    case 400..<500:
      return .failed(message: message, permanent: true, stopRun: nil)
    default:
      return .failed(message: "Unexpected response: \(message)", permanent: false, stopRun: nil)
    }
  }

  /// "HTTP <status> <code>: <message>" when the server sent a JSON error,
  /// otherwise "HTTP <status>: <body excerpt or reason phrase>".
  private static func httpMessage(status: Int, code: String?, apiMessage: String?, body: Data) -> String {
    if let code {
      return "HTTP \(status) \(code): \(apiMessage ?? HTTPURLResponse.localizedString(forStatusCode: status))"
    }
    let excerpt = String(decoding: body.prefix(200), as: UTF8.self)
      .trimmingCharacters(in: .whitespacesAndNewlines)
    return "HTTP \(status): \(excerpt.isEmpty ? HTTPURLResponse.localizedString(forStatusCode: status) : excerpt)"
  }

  private static func stringValue(_ value: Any?) -> String? {
    switch value {
    case let string as String:
      return string
    case let number as NSNumber:
      return number.stringValue
    default:
      return nil
    }
  }

  private static let unreachableCodes: Set<Int> = [
    NSURLErrorCannotFindHost,
    NSURLErrorCannotConnectToHost,
    NSURLErrorDNSLookupFailed,
    NSURLErrorTimedOut,
    NSURLErrorNotConnectedToInternet,
    NSURLErrorSecureConnectionFailed,
    NSURLErrorServerCertificateUntrusted,
    NSURLErrorServerCertificateHasBadDate,
    NSURLErrorServerCertificateNotYetValid,
    NSURLErrorServerCertificateHasUnknownRoot,
    NSURLErrorAppTransportSecurityRequiresSecureConnection,
  ]

  static func describeTransportError(_ error: NSError) -> String {
    guard error.domain == NSURLErrorDomain else {
      return error.localizedDescription
    }
    switch error.code {
    case NSURLErrorAppTransportSecurityRequiresSecureConnection:
      return "Plain HTTP is blocked by iOS App Transport Security. Use an https:// server URL."
    case NSURLErrorServerCertificateUntrusted, NSURLErrorServerCertificateHasUnknownRoot,
         NSURLErrorServerCertificateHasBadDate, NSURLErrorServerCertificateNotYetValid:
      return "The server's TLS certificate is not trusted (\(error.localizedDescription))"
    case NSURLErrorCannotFindHost, NSURLErrorDNSLookupFailed:
      return "Server name could not be resolved (\(error.localizedDescription))"
    case NSURLErrorCannotConnectToHost:
      return "Could not connect to the server (\(error.localizedDescription))"
    case NSURLErrorTimedOut:
      return "The server did not respond in time (\(error.localizedDescription))"
    case NSURLErrorNotConnectedToInternet:
      return "No network connection (\(error.localizedDescription))"
    default:
      return "Network error: \(error.localizedDescription)"
    }
  }

  // MARK: - File helpers

  private static func fileSize(_ url: URL) -> Int64 {
    let attributes = try? FileManager.default.attributesOfItem(atPath: url.path)
    return (attributes?[.size] as? NSNumber)?.int64Value ?? 0
  }

  private static func availableCapacity() -> Int64? {
    let values = try? NmcpPaths.stagingDirectory.resourceValues(forKeys: [.volumeAvailableCapacityForImportantUsageKey])
    return values?.volumeAvailableCapacityForImportantUsage
  }

  private static func isOutOfSpace(_ error: Error) -> Bool {
    var current: NSError? = error as NSError
    while let nsError = current {
      if nsError.domain == NSCocoaErrorDomain, nsError.code == NSFileWriteOutOfSpaceError {
        return true
      }
      if nsError.domain == NSPOSIXErrorDomain, nsError.code == Int(ENOSPC) {
        return true
      }
      current = nsError.userInfo[NSUnderlyingErrorKey] as? NSError
    }
    return false
  }
}
