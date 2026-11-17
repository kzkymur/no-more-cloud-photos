// Values that differ per Apple developer account / build come from the
// environment so the same source builds for any team:
//   NMCP_IOS_BUNDLE_ID   bundle identifier registered in the customer's Apple team
//                        (default io.nmcp.photos; must be unique across Apple)
//   NMCP_ANDROID_PACKAGE Android application id (default io.nmcp.photos; changing it
//                        makes a different app — keep it once distributed)
//   NMCP_BUILD_NUMBER    CFBundleVersion / versionCode (CI passes github.run_number)
module.exports = ({ config }) => {
  const bundleId = process.env.NMCP_IOS_BUNDLE_ID || config.ios.bundleIdentifier;
  const build = process.env.NMCP_BUILD_NUMBER;
  return {
    ...config,
    ios: {
      ...config.ios,
      bundleIdentifier: bundleId,
      ...(build ? { buildNumber: String(build) } : {}),
    },
    android: {
      ...config.android,
      package: process.env.NMCP_ANDROID_PACKAGE || config.android.package,
      ...(build ? { versionCode: Number(build) } : {}),
    },
  };
};
