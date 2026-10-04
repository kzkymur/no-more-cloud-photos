# Metadata fixture provenance

The binary files in this directory come from ExifTool `13.36` test fixtures at
`https://github.com/exiftool/exiftool/tree/13.36/t/images`. ExifTool is by Phil
Harvey and is distributed under the same terms as Perl (Artistic License or
GNU GPL). These small metadata-oriented fixtures are evidence for content
detection and real ExifTool probing; they are not evidence that Core can decode
or transform every camera/codec variant.

| File | Canonical MIME | SHA-256 |
|---|---|---|
| `BMP.bmp` | `image/bmp` | `fab182ec28064483847443e29982d592b64d7019fc4f1db85e02501a40e1dcf8` |
| `CanonRaw.cr2` | `image/x-canon-cr2` | `b5d3d26f3c85bcb35a52515eac060e2e362161893504013589ffd9ad2e9b004b` |
| `CanonRaw.cr3` | `image/x-canon-cr3` | `dc02aa55e277935b690879584e97c2d013f54d854afaec6f9d3274c99a918fd6` |
| `DNG.dng` | `image/dng` | `daa9ce7a2c6923815390d8566254ef4d4a75d68d1531afdb264bd4b39a8dfd89` |
| `ExifTool.jpg` | `image/jpeg` | `fdca3287a21453b51e250e67aba836d560e6043559a6e37acb429a03e720e412` |
| `FujiFilm.raf` | `image/x-fuji-raf` | `e12e30bd0cf5f160b82b93f043696c04d1d5f4628f1fdd19abdab9f8328d8bf0` |
| `GIF.gif` | `image/gif` | `55f8d30ea6fac980f35d5af11a90b10ddc0186d961b0273e66df2f8b7c5aa6be` |
| `Nikon.nef` | `image/x-nikon-nef` | `6fae30a2809b52ece100316f33a3fade4b65cf5690af2d62bb00f44ae052bdd6` |
| `Panasonic.rw2` | `image/x-panasonic-rw2` | `431a1239713ce1bca8f0b422b9a094372246669432060e2a0a21d1fd2f761678` |
| `PNG.png` | `image/png` | `45f0bd6bd3c85cf8c79028dee8f0de5cd470eb3b0124d2c7559f0dd3de24a288` |
| `QuickTime.heic` | rejection evidence (`mif1` major brand mixes still `heic` and sequence `hevc` compatibility) | `4e1785e9924600d0274176f52609a2d514481877103b91c714bd2088ea803ae7` |
| `QuickTime.mov` | `video/quicktime` | `eea529609b6026e0cd7b3d9188b997889f905cd89a93421ad7a9063c670449ec` |
| `RIFF.webp` | `image/webp` | `054fb882674304992a0af031a77df061c3f802c30891dbb8bc2d16e41213767d` |

The complete 17-format detector matrix is generated synthetically in
`detector_test.go` so malformed lengths, loops, offsets, and conflicting brands
can be asserted deterministically. Those synthetic cases are reported
separately from the real external-probe fixtures above.
