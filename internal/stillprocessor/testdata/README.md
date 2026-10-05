# Still processor fixture provenance

Real external codec fixtures are downloaded or extracted in CI, verified before
use, and never substituted by MIME detection alone. Generated raster fixtures
are separate and are not evidence for camera or third-party codec coverage.

## External fixtures

| Evidence | Source | SHA-256 | License / attribution | Proven envelope |
| --- | --- | --- | --- | --- |
| HEIC | `examples/example.heic` from the verified libheif 1.23.5 source archive, https://github.com/strukturag/libheif/releases/download/v1.23.5/libheif-1.23.5.tar.gz | `7f8b363e4936c0666a25f64f3a92fda10bd8e5453be4592530b65a55dd98f3f2` | libheif project example, LGPL-3.0-or-later; source archive and notices retained by the pinned build | one primary HEIC accepted by libheif, transformed to decodable AV1 AVIF; not proof of every HEIC/HEIF variant |
| static WebP | https://www.gstatic.com/webp/gallery/4.webp | `0858d0afcb2921ded36b05586204f2459d965feb7db54cb083e3cfa059589dd9` | “A Wild Cherry (Prunus avium) in flower”, Benjamin Gimmel, CC BY-SA 3.0; Google WebP Gallery credits: https://developers.google.com/speed/webp/gallery1 | static lossy WebP decoded by the exact libvips/libwebp build and transformed to decodable AV1 AVIF; animation remains issue #12 |
| Exif orientation 6 JPEG | https://raw.githubusercontent.com/recurser/exif-orientation-examples/219294e144531b0c01247913cb58b6f5531b5081/Landscape_6.jpg | `9b344e9f0c869d8637ea22e672df9451d8d3cc1d2d0b291af3b284e538e5f124` | MIT, Ian Arellano / `recurser/exif-orientation-examples`, commit `219294e144531b0c01247913cb58b6f5531b5081` | orientation is applied to pixels before resize: 1200x1800 stored pixels become 1800x1200, with independently rotated JPEG reference pixels compared to decoded AVIF within per-channel tolerance 60 |

## Project-generated raster fixtures

`processor_integration_linux_test.go` generates odd-sized JPEG, alpha PNG,
1-pixel-axis PNG, and BI_RGB 32-bit BMP bytes from project-owned test patterns.
The BMP reserved byte is deliberately zero and must remain opaque. These prove
real decoder/encoder behavior and boundary geometry, but not external-file
provenance or camera support.
