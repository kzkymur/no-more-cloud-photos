// Release signing for the generated android/ project.
// The keystore never lives in the repo: CI decodes it from a GitHub secret and
// passes its location and passwords through environment variables:
//   NMCP_UPLOAD_STORE_FILE, NMCP_UPLOAD_STORE_PASSWORD,
//   NMCP_UPLOAD_KEY_ALIAS,  NMCP_UPLOAD_KEY_PASSWORD
// Without them the release build falls back to the debug key (local testing only).
const { withAppBuildGradle } = require('expo/config-plugins');

const MARKER = '// nmcp-release-signing';

module.exports = function withReleaseSigning(config) {
  return withAppBuildGradle(config, (cfg) => {
    let gradle = cfg.modResults.contents;
    if (gradle.includes(MARKER)) return cfg;

    gradle = gradle.replace(
      /signingConfigs\s*\{/,
      `signingConfigs {
        ${MARKER}
        release {
            if (System.getenv('NMCP_UPLOAD_STORE_FILE')) {
                storeFile file(System.getenv('NMCP_UPLOAD_STORE_FILE'))
                storePassword System.getenv('NMCP_UPLOAD_STORE_PASSWORD')
                keyAlias System.getenv('NMCP_UPLOAD_KEY_ALIAS')
                keyPassword System.getenv('NMCP_UPLOAD_KEY_PASSWORD')
            }
        }`
    );
    // In the release buildType, use the release key when it is configured.
    gradle = gradle.replace(
      /(release\s*\{[^{}]*?)signingConfig\s+signingConfigs\.debug/,
      `$1signingConfig System.getenv('NMCP_UPLOAD_STORE_FILE') ? signingConfigs.release : signingConfigs.debug`
    );
    if (!gradle.includes("signingConfigs.release : signingConfigs.debug")) {
      throw new Error('withReleaseSigning: could not find release buildType signingConfig in app/build.gradle');
    }
    cfg.modResults.contents = gradle;
    return cfg;
  });
};
