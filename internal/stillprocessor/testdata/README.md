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
| Canon PowerShot A410 DNG | https://raw.pixls.us/data/Canon/Canon%20PowerShot%20A410/CANON_A410_CHDK_CRW_0741.DNG | `f0d2fe47507fadf50008bddbc6bd2c5e39fddbe90cca6bca72dd360fa0e0eb38` | CC0 public-domain dedication declared by raw.pixls.us contributors; repository: https://raw.pixls.us/ | real camera RAW decoded by pinned LibRaw with camera WB, camera matrix, 16-bit processing, and auto-bright disabled; decoded AVIF grid pixels are compared with a separately compiled reference path spelling out those settings, so removing a production setting fails evidence. The other seven advertised RAW MIMEs carry the same evidence from the camera fixtures below. |
| raw-canon-cr2 (`image/x-canon-cr2`) | https://raw.pixls.us/download/data/Canon/EOS%20350D/IMG_1707.CR2 | `8cbb84e04d93b005fe082da9c954122a612b5281af00aa088d767850f343fd38` | CC0-1.0 (raw.pixls.us per-file license 'Creative Commons 0 - Public Domain', confirmed in https://raw.pixls.us/json/getrepository.php?set=all); "Canon - EOS 350D - RAW (3:2)" (IMG_1707.CR2), by raw.pixls.us contributors (CC0). Source: https://raw.pixls.us/data/Canon/EOS%20350D/. License: CC0 1.0 Universal Public Domain Dedication (https://creativecommons.org/publicdomain/zero/1.0/); no attribution legally required. | same LibRaw settings and reference-pixel comparison as the DNG row, from the pinned real camera file; source larger than 1920 px proves bounded downscale |
| raw-canon-cr3 (`image/x-canon-cr3`) | https://raw.pixls.us/download/data/Canon/EOS%20M50/Canon_EOS_M50_CRAW_ISO_100_nocrop_nodual.CR3 | `27a88fdae82516ba136e12c2cc9b934f25dbaaac24d64ab4e5517f8524463594` | CC0-1.0 (raw.pixls.us per-file license 'Creative Commons 0 - Public Domain', confirmed in https://raw.pixls.us/json/getrepository.php?set=all); "Canon - EOS M50 - 3:2" (Canon_EOS_M50_CRAW_ISO_100_nocrop_nodual.CR3), by raw.pixls.us contributors (CC0). Source: https://raw.pixls.us/data/Canon/EOS%20M50/. License: CC0 1.0 Universal Public Domain Dedication (https://creativecommons.org/publicdomain/zero/1.0/); no attribution legally required. | same LibRaw settings and reference-pixel comparison as the DNG row, from the pinned real camera file; source larger than 1920 px proves bounded downscale |
| raw-fuji-raf (`image/x-fuji-raf`) | https://raw.pixls.us/download/data/Fujifilm/FinePix%20X100/DSCF1676.RAF | `1f4aea4fa43dd314f8b90954d7823655d4906d3e468a9ea014652b872a68db16` | CC0-1.0 (raw.pixls.us per-file license 'Creative Commons 0 - Public Domain', confirmed in https://raw.pixls.us/json/getrepository.php?set=all); "Fujifilm - FinePix X100 - 12bit 12bit uncompressed (3:2)" (DSCF1676.RAF), by raw.pixls.us contributors (CC0). Source: https://raw.pixls.us/data/Fujifilm/FinePix%20X100/. License: CC0 1.0 Universal Public Domain Dedication (https://creativecommons.org/publicdomain/zero/1.0/); no attribution legally required. | same LibRaw settings and reference-pixel comparison as the DNG row, from the pinned real camera file; source larger than 1920 px proves bounded downscale |
| raw-nikon-nef (`image/x-nikon-nef`) | https://raw.pixls.us/download/data/Nikon/D70/20170902_0047.NEF | `dd6405aeb33b0cd5bf66c98ba98ccbb478a765450cfd130810e470dab8d1f4b4` | CC0-1.0 (raw.pixls.us per-file license 'Creative Commons 0 - Public Domain', confirmed in https://raw.pixls.us/json/getrepository.php?set=all); "Nikon - D70 - 12bit 12bit compressed (Lossy (type 1)) (3:2)" (20170902_0047.NEF), by raw.pixls.us contributors (CC0). Source: https://raw.pixls.us/data/Nikon/D70/. License: CC0 1.0 Universal Public Domain Dedication (https://creativecommons.org/publicdomain/zero/1.0/); no attribution legally required. | same LibRaw settings and reference-pixel comparison as the DNG row, from the pinned real camera file; source larger than 1920 px proves bounded downscale |
| raw-olympus-orf (`image/x-olympus-orf`) | https://raw.pixls.us/download/data/Olympus/E-420/P4232993.ORF | `4b981b6e82357514b00183f3c676202625a61684373cc32decb361639ed2eec4` | CC0-1.0 (raw.pixls.us per-file license 'Creative Commons 0 - Public Domain', confirmed in https://raw.pixls.us/json/getrepository.php?set=all); "Olympus - E-420 - 16bit (4:3)" (P4232993.ORF), by raw.pixls.us contributors (CC0). Source: https://raw.pixls.us/data/Olympus/E-420/. License: CC0 1.0 Universal Public Domain Dedication (https://creativecommons.org/publicdomain/zero/1.0/); no attribution legally required. | same LibRaw settings and reference-pixel comparison as the DNG row, from the pinned real camera file; source larger than 1920 px proves bounded downscale |
| raw-panasonic-rw2 (`image/x-panasonic-rw2`) | https://raw.pixls.us/download/data/Panasonic/DMC-GF1/panasonic_4-3.RW2 | `96c0e6f0330515ad6b4a8d8ad6a3753bc035ee21c6467155d551266f10759473` | CC0-1.0 (raw.pixls.us per-file license 'Creative Commons 0 - Public Domain', confirmed in https://raw.pixls.us/json/getrepository.php?set=all); "Panasonic - DMC-GF1 - 4:3" (panasonic_4-3.RW2), by raw.pixls.us contributors (CC0). Source: https://raw.pixls.us/data/Panasonic/DMC-GF1/. License: CC0 1.0 Universal Public Domain Dedication (https://creativecommons.org/publicdomain/zero/1.0/); no attribution legally required. | same LibRaw settings and reference-pixel comparison as the DNG row, from the pinned real camera file; source larger than 1920 px proves bounded downscale |
| raw-sony-arw (`image/x-sony-arw`) | https://raw.pixls.us/download/data/Sony/DSLR-A200/DSC00108.ARW | `04eacd403d92e0c90da9d91b87615af8eda1afc963350d3ab774557c36763eef` | CC0-1.0 (raw.pixls.us per-file license 'Creative Commons 0 - Public Domain', confirmed in https://raw.pixls.us/json/getrepository.php?set=all); "Sony - DSLR-A200 - 12bit 12bit compressed (3:2)" (DSC00108.ARW), by raw.pixls.us contributors (CC0). Source: https://raw.pixls.us/data/Sony/DSLR-A200/. License: CC0 1.0 Universal Public Domain Dedication (https://creativecommons.org/publicdomain/zero/1.0/); no attribution legally required. | same LibRaw settings and reference-pixel comparison as the DNG row, from the pinned real camera file; source larger than 1920 px proves bounded downscale |

Camera RAW pins (URL, size, SHA-256, license and attribution) live once in
`test/e2e/testdata/fixtures.json`; CI downloads each into
`TEST_STILL_RAW_FIXTURES_DIR` and the native test re-verifies size and digest
before use. `TestRealStillHelper` fails if the helper advertises a RAW MIME
without one of these reference cases, or if a proven one is not advertised.

## Project-generated raster fixtures

`processor_integration_linux_test.go` generates odd-sized JPEG, alpha PNG,
1-pixel-axis PNG, an ICC-tagged 16-bit PNG gradient, and BI_RGB BMP bytes from
project-owned test patterns. The 16-bit reference pixels prove full-range
scaling and ICC-before-quantization. A 2000-pixel alpha pattern is independently
decoded as RGBA after resize to prove alpha 4:4:4 and the premultiply/resize/
unpremultiply path. BMP evidence covers 32-bit reserved-byte opacity plus
24-bit padded bottom-up and top-down rows; truncated and overflow-dimension
inputs fail closed. These prove real decoder/encoder behavior and boundary
geometry, but not external-file provenance or camera support.

`nmcp-heif-fixture-generator` is project-owned test code linked to the same
pinned libheif and AOM build as the helper. It produces deterministic 32x32
AVIF-container HEIF fixtures for:

- BT.2020/PQ code value 512/1023, whose independently calculated BT.2446A
  reference output is sRGB `(127,127,127)`;
- BT.2020/HLG code value 512/1023, whose independently calculated BT.2100 OOTF
  plus BT.2446A reference output is sRGB `(100,100,100)`;
- project-generated Display P3 ICC plus Display P3 NCLX `(200,100,50)`, whose
  independent matrix reference is sRGB `(215,93,31)`, proving NCLX passthrough
  followed by exactly one ICC normalization rather than two gamut conversions;
- deliberately malformed embedded ICC bytes, which must fail closed as a
  decode error rather than being ignored.
- a two-item top-level HEIF collection whose second, green image is explicitly
  designated primary; decoded output pixels prove intentional primary selection.

The AVIF output is decoded independently with pinned FFmpeg and compared at a
center reference pixel with per-channel tolerance 24 to allow the required
quality-60 output encoding. These generated files prove controlled color-path
behavior, not provenance of a camera HDR capture.

## Failure and resource evidence

The native integration test truncates the hash-verified real DNG to 512 bytes
and requires a stable decode failure. Unit tests separately prove a bounded
process timeout, cancellation, stderr overflow, generated-file size limit,
4 GiB address-space ceiling configuration, and secret/path redaction. The real
A410 RAW exceeds the 1920 standard edge and proves bounded downscale without
upscale or crop. A generated 1000x777 JPEG separately proves the 640-edge,
quality-50 thumbnail path and odd rounded height 497. Physical exhaustion of
the full 4 GiB ceiling is intentionally not performed in CI.
