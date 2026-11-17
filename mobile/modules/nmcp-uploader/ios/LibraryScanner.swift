import Foundation
import Photos
import UniformTypeIdentifiers
import os

private let scanLog = Logger(subsystem: "io.nmcp.uploader", category: "scan")

enum LibraryAccess {
  static var status: PHAuthorizationStatus {
    PHPhotoLibrary.authorizationStatus(for: .readWrite)
  }

  /// True for full and limited ("selected photos") access.
  static var isReadable: Bool {
    switch status {
    case .authorized, .limited:
      return true
    default:
      return false
    }
  }

  /// Maps to `LibraryAccess` in the JS contract.
  static func contractValue(_ status: PHAuthorizationStatus) -> String {
    switch status {
    case .authorized:
      return "full"
    case .limited:
      return "partial"
    case .denied, .restricted:
      return "denied"
    case .notDetermined:
      return "undetermined"
    @unknown default:
      return "denied"
    }
  }
}

/// Walks the photo library and records every ORIGINAL resource that should be
/// backed up. Edited renditions (`.fullSizePhoto`, `.fullSizeVideo`,
/// `.adjustmentData`, ...) are never recorded.
final class LibraryScanner {
  enum Outcome {
    case completed(assets: Int, newOrChanged: Int)
    case interrupted
    case notAuthorized
  }

  private static let batchSize = 500

  /// Synchronous; call from a background queue. `shouldContinue` is polled
  /// between assets. Rows of vanished assets are only purged after a
  /// complete pass, so an interrupted scan never deletes anything.
  func scan(into store: UploadStore, shouldContinue: () -> Bool) -> Outcome {
    guard LibraryAccess.isReadable else {
      return .notAuthorized
    }

    let options = PHFetchOptions()
    options.includeHiddenAssets = false
    // A backup must contain every burst frame, not just the representative.
    options.includeAllBurstAssets = true
    options.includeAssetSourceTypes = [.typeUserLibrary]
    options.predicate = NSPredicate(
      format: "mediaType == %d OR mediaType == %d",
      PHAssetMediaType.image.rawValue,
      PHAssetMediaType.video.rawValue
    )
    let assets = PHAsset.fetchAssets(with: options)

    let known = store.knownAssetModifications()
    var seen = Set<String>()
    seen.reserveCapacity(assets.count)
    var batch: [ScannedResource] = []
    batch.reserveCapacity(Self.batchSize)
    var changed = 0

    for index in 0..<assets.count {
      if !shouldContinue() {
        store.upsert(batch)
        scanLog.info("Scan interrupted after \(index) of \(assets.count) assets")
        return .interrupted
      }
      autoreleasepool {
        let asset = assets.object(at: index)
        let assetId = asset.localIdentifier
        seen.insert(assetId)
        let modified = (asset.modificationDate ?? asset.creationDate)?.timeIntervalSince1970 ?? 0
        // Fast path: unchanged since the last scan. Enumerating resources is
        // the expensive part of a scan, so skip it when nothing changed.
        if let previous = known[assetId], abs(previous - modified) < 0.001 {
          return
        }
        let resources = Self.originalResources(of: asset, modified: modified)
        if !resources.isEmpty {
          changed += 1
          batch.append(contentsOf: resources)
        }
      }
      if batch.count >= Self.batchSize {
        store.upsert(batch)
        batch.removeAll(keepingCapacity: true)
      }
    }
    store.upsert(batch)

    let removed = store.removeVanished(keeping: seen)
    RuntimeState.lastScanAt = RuntimeState.nowMillis
    scanLog.info("Scan complete: \(assets.count) assets, \(changed) new/changed, \(removed) vanished rows removed")
    return .completed(assets: assets.count, newOrChanged: changed)
  }

  /// Original resources to upload for one asset:
  /// - image: `.photo`; plus `.alternatePhoto` (RAW+JPEG pair); plus
  ///   `.pairedVideo` for Live Photos.
  /// - video: `.video`.
  static func originalResources(of asset: PHAsset, modified: Double) -> [ScannedResource] {
    let resources = PHAssetResource.assetResources(for: asset)
    var wanted: [PHAssetResourceType]
    switch asset.mediaType {
    case .image:
      wanted = [.photo, .alternatePhoto, .pairedVideo]
    case .video:
      wanted = [.video]
    default:
      return []
    }
    if asset.mediaType == .image, !resources.contains(where: { $0.type == .photo }) {
      // No original still: nothing meaningful to back up for this asset.
      wanted = []
    }

    var result: [ScannedResource] = []
    for type in wanted {
      guard let resource = resources.first(where: { $0.type == type }) else {
        continue
      }
      let name = resource.originalFilename.isEmpty ? fallbackName(asset: asset, type: type) : resource.originalFilename
      result.append(ScannedResource(
        id: "\(asset.localIdentifier)#\(type.rawValue)",
        assetId: asset.localIdentifier,
        resourceType: type.rawValue,
        name: name,
        mime: mimeType(forUTI: resource.uniformTypeIdentifier),
        modified: modified
      ))
    }
    return result
  }

  static func mimeType(forUTI uti: String) -> String {
    UTType(uti)?.preferredMIMEType ?? "application/octet-stream"
  }

  private static func fallbackName(asset: PHAsset, type: PHAssetResourceType) -> String {
    let base = asset.localIdentifier.components(separatedBy: "/").first ?? "asset"
    return type == .photo || type == .alternatePhoto ? "\(base).jpg" : "\(base).mov"
  }
}

/// Triggers a (debounced) scan when the photo library changes while the app
/// process is alive.
final class LibraryObserver: NSObject, PHPhotoLibraryChangeObserver, @unchecked Sendable {
  static let shared = LibraryObserver()

  private let lock = NSLock()
  private var registered = false
  private var pendingWork: DispatchWorkItem?
  private let queue = DispatchQueue(label: "io.nmcp.uploader.library-observer")
  private static let debounce: TimeInterval = 3

  /// Registering before authorization is pointless (no change events), so
  /// this is a no-op until access is granted; call again after granting.
  func registerIfAuthorized() {
    guard LibraryAccess.isReadable else {
      return
    }
    let shouldRegister = lock.locked { () -> Bool in
      if registered {
        return false
      }
      registered = true
      return true
    }
    if shouldRegister {
      PHPhotoLibrary.shared().register(self)
    }
  }

  func photoLibraryDidChange(_ changeInstance: PHChange) {
    queue.async { [weak self] in
      guard let self else { return }
      self.pendingWork?.cancel()
      let work = DispatchWorkItem {
        let config = ConfigStore.load()
        UploadEngine.shared.requestRun(scan: true, upload: config.isAutoUploadReady)
      }
      self.pendingWork = work
      self.queue.asyncAfter(deadline: .now() + Self.debounce, execute: work)
    }
  }
}
