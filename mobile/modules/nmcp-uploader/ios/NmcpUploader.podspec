Pod::Spec.new do |s|
  s.name           = 'NmcpUploader'
  s.version        = '1.0.0'
  s.summary        = 'Background photo library uploader for a self-hosted No More Cloud Photos server'
  s.description    = 'Scans the iOS photo library and uploads original photos, videos and Live Photo videos ' \
                     'to a self-hosted server using a background URLSession and BGTaskScheduler.'
  s.author         = ''
  s.homepage       = 'https://docs.expo.dev/modules/'
  s.platforms      = {
    :ios => '16.4'
  }
  s.swift_version  = '5.9'
  s.source         = { git: '' }
  s.static_framework = true

  s.dependency 'ExpoModulesCore'

  s.frameworks = 'Photos', 'PhotosUI', 'BackgroundTasks', 'UserNotifications', 'CryptoKit', 'UniformTypeIdentifiers'
  s.library = 'sqlite3'

  # Swift/Objective-C compatibility
  s.pod_target_xcconfig = {
    'DEFINES_MODULE' => 'YES',
  }

  s.source_files = "**/*.{h,m,mm,swift,hpp,cpp}"
end
