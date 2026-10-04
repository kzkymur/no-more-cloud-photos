package metadata

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

func TestReferenceFixtureHashes(t *testing.T) {
	hashes := map[string]string{
		"BMP.bmp":        "fab182ec28064483847443e29982d592b64d7019fc4f1db85e02501a40e1dcf8",
		"CanonRaw.cr2":   "b5d3d26f3c85bcb35a52515eac060e2e362161893504013589ffd9ad2e9b004b",
		"CanonRaw.cr3":   "dc02aa55e277935b690879584e97c2d013f54d854afaec6f9d3274c99a918fd6",
		"DNG.dng":        "daa9ce7a2c6923815390d8566254ef4d4a75d68d1531afdb264bd4b39a8dfd89",
		"ExifTool.jpg":   "fdca3287a21453b51e250e67aba836d560e6043559a6e37acb429a03e720e412",
		"FujiFilm.raf":   "e12e30bd0cf5f160b82b93f043696c04d1d5f4628f1fdd19abdab9f8328d8bf0",
		"GIF.gif":        "55f8d30ea6fac980f35d5af11a90b10ddc0186d961b0273e66df2f8b7c5aa6be",
		"Nikon.nef":      "6fae30a2809b52ece100316f33a3fade4b65cf5690af2d62bb00f44ae052bdd6",
		"Panasonic.rw2":  "431a1239713ce1bca8f0b422b9a094372246669432060e2a0a21d1fd2f761678",
		"PNG.png":        "45f0bd6bd3c85cf8c79028dee8f0de5cd470eb3b0124d2c7559f0dd3de24a288",
		"QuickTime.heic": "4e1785e9924600d0274176f52609a2d514481877103b91c714bd2088ea803ae7",
		"QuickTime.mov":  "eea529609b6026e0cd7b3d9188b997889f905cd89a93421ad7a9063c670449ec",
		"RIFF.webp":      "054fb882674304992a0af031a77df061c3f802c30891dbb8bc2d16e41213767d",
	}
	for name, want := range hashes {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
			t.Errorf("%s SHA-256 = %s; want %s", name, got, want)
		}
	}
}

func TestDetectExifToolReferenceFixtures(t *testing.T) {
	tests := map[string]string{
		"BMP.bmp":       "image/bmp",
		"CanonRaw.cr2":  "image/x-canon-cr2",
		"CanonRaw.cr3":  "image/x-canon-cr3",
		"DNG.dng":       "image/dng",
		"ExifTool.jpg":  "image/jpeg",
		"FujiFilm.raf":  "image/x-fuji-raf",
		"GIF.gif":       "image/gif",
		"Nikon.nef":     "image/x-nikon-nef",
		"Panasonic.rw2": "image/x-panasonic-rw2",
		"PNG.png":       "image/png",
		"RIFF.webp":     "image/webp",
	}
	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("testdata", name)
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			got, err := Detect(file, info.Size())
			if err != nil {
				t.Fatalf("Detect() error = %v", err)
			}
			if got.Format.MIMEType != want {
				t.Fatalf("MIME = %q, want %q", got.Format.MIMEType, want)
			}
		})
	}
}

func TestDetectAllRegisteredFormats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mime string
		data []byte
	}{
		{"jpeg", "image/jpeg", []byte{0xff, 0xd8, 0xff, 0xd9}},
		{"png", "image/png", pngFixture()},
		{"gif", "image/gif", gifFixture()},
		{"webp", "image/webp", webPFixture()},
		{"bmp", "image/bmp", bmpFixture()},
		{"raf", "image/x-fuji-raf", rafFixture()},
		{"orf", "image/x-olympus-orf", specialTIFFFixture([]byte{'I', 'I', 'R', 'O'})},
		{"rw2", "image/x-panasonic-rw2", specialTIFFFixture([]byte{'I', 'I', 0x55, 0x00})},
		{"cr2", "image/x-canon-cr2", cr2Fixture()},
		{"dng", "image/dng", tiffFixture("", true, true)},
		{"nef", "image/x-nikon-nef", tiffFixture("NIKON CORPORATION", false, true)},
		{"arw", "image/x-sony-arw", tiffFixture("SONY", false, true)},
		{"heic", "image/heic", ftypFixture("heic", "mif1")},
		{"heif", "image/heif", ftypFixture("mif1")},
		{"cr3", "image/x-canon-cr3", ftypFixture("crx ", "isom")},
		{"mp4", "video/mp4", ftypFixture("mp42", "isom")},
		{"quicktime", "video/quicktime", ftypFixture("qt  ")},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Detect(bytes.NewReader(test.data), int64(len(test.data)))
			if err != nil {
				t.Fatalf("Detect() error = %v", err)
			}
			if got.Format.MIMEType != test.mime {
				t.Fatalf("Detect() MIME = %q, want %q", got.Format.MIMEType, test.mime)
			}
			if got.Format.Extension == "" || got.Evidence == "" {
				t.Fatalf("Detect() returned incomplete detection: %+v", got)
			}
		})
	}
}

func TestDetectRejectsUnknownAndSpoofedContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data []byte
		err  error
	}{
		{"empty", nil, ErrUnsupportedMediaType},
		{"unknown", []byte("not a media file.jpg"), ErrUnsupportedMediaType},
		{"jpeg prefix only", []byte{0xff, 0xd8}, ErrInvalidMedia},
		{"png signature only", []byte("\x89PNG\r\n\x1a\n"), ErrInvalidMedia},
		{"gif header only", []byte("GIF89a"), ErrInvalidMedia},
		{"riff but not webp", append([]byte("RIFF\x0c\x00\x00\x00WAVEfmt "), make([]byte, 4)...), ErrInvalidMedia},
		{"bmp magic only", []byte("BM"), ErrInvalidMedia},
		{"raf magic only", []byte("FUJIFILMCCD-RAW "), ErrInvalidMedia},
		{"generic tiff", genericTIFFFixture(), ErrUnsupportedMediaType},
		{"unknown ftyp brand", ftypFixture("zzzz"), ErrUnsupportedMediaType},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := Detect(bytes.NewReader(test.data), int64(len(test.data)))
			if !errors.Is(err, test.err) {
				t.Fatalf("Detect() error = %v, want %v", err, test.err)
			}
		})
	}
}

func TestDetectRejectsVendorNameWithoutRawTagConjunction(t *testing.T) {
	t.Parallel()
	for _, makeName := range []string{"NIKON CORPORATION", "SONY"} {
		data := tiffFixture(makeName, false, false)
		if _, err := Detect(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrUnsupportedMediaType) {
			t.Fatalf("Detect(%q vendor-only TIFF) error = %v, want unsupported", makeName, err)
		}
	}
}

func TestDetectRejectsContradictoryEvidence(t *testing.T) {
	t.Parallel()
	tests := [][]byte{
		ftypFixture("heic", "mp42"),
		ftypFixture("heic", "heif"),
		ftypFixture("crx ", "qt  "),
		tiffFixture("NIKON", true, true),
	}
	for i, data := range tests {
		if _, err := Detect(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrInvalidMedia) {
			t.Errorf("case %d: Detect() error = %v, want invalid media", i, err)
		}
	}
}

func TestDetectRejectsTruncatedAndInvalidLengths(t *testing.T) {
	t.Parallel()
	badExtendedBox := make([]byte, 16)
	binary.BigEndian.PutUint32(badExtendedBox[:4], 1)
	copy(badExtendedBox[4:8], "ftyp")
	binary.BigEndian.PutUint64(badExtendedBox[8:16], ^uint64(0))

	badWebP := webPFixture()
	binary.LittleEndian.PutUint32(badWebP[4:8], 1000)
	badBMP := bmpFixture()
	binary.LittleEndian.PutUint32(badBMP[10:14], ^uint32(0))
	badRAF := rafFixture()
	binary.BigEndian.PutUint32(badRAF[84:88], ^uint32(0))
	badFTYPAlignment := append(ftypFixture("isom"), 0)
	binary.BigEndian.PutUint32(badFTYPAlignment[:4], uint32(len(badFTYPAlignment)))

	tests := [][]byte{badExtendedBox, badWebP, badBMP, badRAF, badFTYPAlignment}
	for i, data := range tests {
		if _, err := Detect(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrInvalidMedia) {
			t.Errorf("case %d: Detect() error = %v, want invalid media", i, err)
		}
	}
}

func TestDetectRejectsInvalidTIFFGraphsAndValues(t *testing.T) {
	t.Parallel()
	outOfRangeIFD := genericTIFFFixture()
	binary.LittleEndian.PutUint32(outOfRangeIFD[4:8], ^uint32(0))

	loop := genericTIFFFixture()
	binary.LittleEndian.PutUint32(loop[10:14], 8)

	outOfRangeValue := make([]byte, 26)
	copy(outOfRangeValue, []byte{'I', 'I', 42, 0, 8, 0, 0, 0})
	binary.LittleEndian.PutUint16(outOfRangeValue[8:10], 1)
	binary.LittleEndian.PutUint16(outOfRangeValue[10:12], 0x010f)
	binary.LittleEndian.PutUint16(outOfRangeValue[12:14], 2)
	binary.LittleEndian.PutUint32(outOfRangeValue[14:18], 20)
	binary.LittleEndian.PutUint32(outOfRangeValue[18:22], 1000)

	tooManyTags := make([]byte, 14)
	copy(tooManyTags, []byte{'I', 'I', 42, 0, 8, 0, 0, 0})
	binary.LittleEndian.PutUint16(tooManyTags[8:10], maxIFDEntries+1)

	for i, data := range [][]byte{outOfRangeIFD, loop, outOfRangeValue, tooManyTags} {
		if _, err := Detect(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrInvalidMedia) {
			t.Errorf("case %d: Detect() error = %v, want invalid media", i, err)
		}
	}
}

func TestDetectValidatesTIFFPointerTags(t *testing.T) {
	t.Parallel()
	for _, tag := range []uint16{0x014a, 0x8769, 0x8825, 0xa005} {
		data := tiffPointerFixture(tag, []uint32{^uint32(0)})
		if _, err := Detect(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrInvalidMedia) {
			t.Errorf("tag %#04x: Detect() error = %v, want invalid media", tag, err)
		}
	}
}

func TestDetectRequiresParsedNonzeroSubIFDForRawEvidence(t *testing.T) {
	t.Parallel()
	data := tiffFixture("NIKON", false, true)
	setTIFFTagValue(t, data, 0x014a, 0)
	if _, err := Detect(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("zero SubIFD pointer error = %v, want unsupported", err)
	}
}

func TestDetectRejectsOversizedSubIFDPointerArray(t *testing.T) {
	t.Parallel()
	pointers := make([]uint32, maxTIFFIFDs+1)
	data := tiffPointerFixture(0x014a, pointers)
	if _, err := Detect(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("oversized SubIFD array error = %v, want invalid media", err)
	}
}

func TestDetectValidatesCR2RawIFD(t *testing.T) {
	t.Parallel()
	tests := map[string]func([]byte){
		"zero": func(data []byte) { binary.LittleEndian.PutUint32(data[12:16], 0) },
		"outside file": func(data []byte) {
			binary.LittleEndian.PutUint32(data[12:16], ^uint32(0))
		},
		"malformed graph": func(data []byte) {
			rawOffset := binary.LittleEndian.Uint32(data[12:16])
			binary.LittleEndian.PutUint16(data[rawOffset:rawOffset+2], maxIFDEntries+1)
		},
	}
	for name, mutate := range tests {
		data := cr2Fixture()
		mutate(data)
		if _, err := Detect(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrInvalidMedia) {
			t.Errorf("%s raw IFD error = %v, want invalid media", name, err)
		}
	}
}

func TestDetectEnforcesCumulativeReadBudget(t *testing.T) {
	t.Parallel()
	const entries = 300
	ifdLength := 2 + entries*12 + 4
	data := make([]byte, 8+ifdLength+4096)
	copy(data, []byte{'I', 'I', 42, 0, 8, 0, 0, 0})
	binary.LittleEndian.PutUint16(data[8:10], entries)
	valueOffset := uint32(8 + ifdLength)
	for i := 0; i < entries; i++ {
		entry := data[10+i*12 : 22+i*12]
		binary.LittleEndian.PutUint16(entry[0:2], 0x0200)
		binary.LittleEndian.PutUint16(entry[2:4], 7)
		binary.LittleEndian.PutUint32(entry[4:8], 4096)
		binary.LittleEndian.PutUint32(entry[8:12], valueOffset)
	}
	reader := &countingReaderAt{reader: bytes.NewReader(data)}
	if _, err := Detect(reader, int64(len(data))); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("amplified reads error = %v, want invalid media", err)
	}
	if reader.bytesRead > maxInspectBytes {
		t.Fatalf("underlying reader read %d bytes, limit is %d", reader.bytesRead, maxInspectBytes)
	}
}

func TestDetectBMFFValidatesCompleteTopLevelGraph(t *testing.T) {
	t.Parallel()
	valid := append(ftypFixture("mp42", "isom"), bmffBox("free", nil)...)
	if got, err := Detect(bytes.NewReader(valid), int64(len(valid))); err != nil || got.Format.MIMEType != "video/mp4" {
		t.Fatalf("valid multi-box BMFF = (%+v, %v), want video/mp4", got, err)
	}

	for name, suffix := range map[string][]byte{
		"trailing garbage": {0xff},
		"malformed box":    {0, 0, 0, 32, 'f', 'r', 'e', 'e'},
	} {
		data := append(ftypFixture("mp42"), suffix...)
		if _, err := Detect(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrInvalidMedia) {
			t.Errorf("%s error = %v, want invalid media", name, err)
		}
	}
	moovBeforeFTYP := append(bmffBox("moov", nil), ftypFixture("mp42")...)
	if _, err := Detect(bytes.NewReader(moovBeforeFTYP), int64(len(moovBeforeFTYP))); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("moov before ftyp error = %v, want invalid media", err)
	}
}

func TestDetectBMFFRequiresSpecificRegisteredEvidence(t *testing.T) {
	t.Parallel()
	if _, err := Detect(bytes.NewReader(ftypFixture("isom")), int64(len(ftypFixture("isom")))); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("generic isom error = %v, want unsupported", err)
	}

	moov := bmffBox("moov", nil)
	if _, err := Detect(bytes.NewReader(moov), int64(len(moov))); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("ftyp-less moov error = %v, want unsupported", err)
	}

	for _, brand := range []string{"hevc", "hevx", "hevm", "hevs", "msf1"} {
		for _, data := range [][]byte{ftypFixture(brand, "mif1"), ftypFixture("mif1", brand)} {
			if _, err := Detect(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrUnsupportedMediaType) {
				t.Errorf("sequence brand %q error = %v, want unsupported", brand, err)
			}
		}
	}
	ambiguousFixture, err := os.ReadFile(filepath.Join("testdata", "QuickTime.heic"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Detect(bytes.NewReader(ambiguousFixture), int64(len(ambiguousFixture))); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("still/sequence-compatible HEIF fixture error = %v, want unsupported", err)
	}

	legacySpoofPayload := append(bmffBox("mvhd", make([]byte, 20)), bmffBox("trak", nil)...)
	legacySpoofPayload = append(legacySpoofPayload, bmffBox("free", []byte("hdlr\x00\x00\x00\x00mhlr"))...)
	legacySpoof := bmffBox("moov", legacySpoofPayload)
	if _, err := Detect(bytes.NewReader(legacySpoof), int64(len(legacySpoof))); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("legacy handler byte spoof error = %v, want unsupported", err)
	}
}

func TestDetectBMFFStillHEVCBrandsAndBaseCompatibility(t *testing.T) {
	t.Parallel()
	for _, brand := range []string{"heim", "heis"} {
		data := ftypFixture(brand, "mif1")
		got, err := Detect(bytes.NewReader(data), int64(len(data)))
		if err != nil || got.Format.MIMEType != "image/heic" {
			t.Errorf("still brand %q = (%+v, %v), want image/heic", brand, got, err)
		}
	}

	data := ftypFixture("isom", "mp42")
	got, err := Detect(bytes.NewReader(data), int64(len(data)))
	if err != nil || got.Format.MIMEType != "video/mp4" {
		t.Fatalf("strong compatible MP4 brand = (%+v, %v), want video/mp4", got, err)
	}
}

func TestDetectHonorsDeclaredSizeAndReaderFailures(t *testing.T) {
	t.Parallel()
	data := pngFixture()
	if _, err := Detect(bytes.NewReader(data), int64(len(data)+1)); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("oversized declaration error = %v, want invalid media", err)
	}
	if _, err := Detect(bytes.NewReader(data), -1); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("negative size error = %v, want invalid media", err)
	}
	if _, err := Detect(nil, 0); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("nil reader error = %v, want invalid media", err)
	}
}

func TestDetectInspectionCaps(t *testing.T) {
	t.Parallel()
	jpeg := []byte{0xff, 0xd8}
	for i := 0; i < maxJPEGSegments; i++ {
		jpeg = append(jpeg, 0xff, 0xd0)
	}
	if _, err := Detect(bytes.NewReader(jpeg), int64(len(jpeg))); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("JPEG over segment cap error = %v, want invalid media", err)
	}

	boxes := make([]byte, 0, (maxBMFFBoxes+1)*8)
	for i := 0; i < maxBMFFBoxes+1; i++ {
		box := make([]byte, 8)
		binary.BigEndian.PutUint32(box[:4], 8)
		copy(box[4:8], "free")
		boxes = append(boxes, box...)
	}
	if _, err := Detect(bytes.NewReader(boxes), int64(len(boxes))); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("BMFF over box cap error = %v, want invalid media", err)
	}
}

func pngFixture() []byte {
	data := make([]byte, 33)
	copy(data, "\x89PNG\r\n\x1a\n")
	binary.BigEndian.PutUint32(data[8:12], 13)
	copy(data[12:16], "IHDR")
	binary.BigEndian.PutUint32(data[16:20], 1)
	binary.BigEndian.PutUint32(data[20:24], 1)
	data[24], data[25] = 8, 2
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}

func gifFixture() []byte {
	data := make([]byte, 13)
	copy(data, "GIF89a")
	binary.LittleEndian.PutUint16(data[6:8], 1)
	binary.LittleEndian.PutUint16(data[8:10], 1)
	return data
}

func webPFixture() []byte {
	data := make([]byte, 20)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], 12)
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8L")
	return data
}

func bmpFixture() []byte {
	data := make([]byte, 26)
	copy(data, "BM")
	binary.LittleEndian.PutUint32(data[2:6], uint32(len(data)))
	binary.LittleEndian.PutUint32(data[10:14], uint32(len(data)))
	binary.LittleEndian.PutUint32(data[14:18], 12)
	return data
}

func rafFixture() []byte {
	data := make([]byte, 120)
	copy(data, "FUJIFILMCCD-RAW ")
	copy(data[16:20], "0201")
	binary.BigEndian.PutUint32(data[84:88], 100)
	binary.BigEndian.PutUint32(data[88:92], 10)
	binary.BigEndian.PutUint32(data[92:96], 110)
	binary.BigEndian.PutUint32(data[96:100], 10)
	return data
}

func genericTIFFFixture() []byte {
	data := make([]byte, 14)
	copy(data, []byte{'I', 'I', 42, 0, 8, 0, 0, 0})
	return data
}

func specialTIFFFixture(magic []byte) []byte {
	data := make([]byte, 14)
	copy(data[:4], magic)
	binary.LittleEndian.PutUint32(data[4:8], 8)
	return data
}

func cr2Fixture() []byte {
	data := make([]byte, 28)
	copy(data, []byte{'I', 'I', 42, 0, 16, 0, 0, 0, 'C', 'R', 2, 0, 22, 0, 0, 0})
	return data
}

type countingReaderAt struct {
	reader    *bytes.Reader
	bytesRead int64
}

func (r *countingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	r.bytesRead += int64(len(p))
	return r.reader.ReadAt(p, offset)
}

type fixtureTag struct {
	tag   uint16
	typ   uint16
	count uint32
	data  []byte
}

func tiffFixture(makeName string, dng, rawTags bool) []byte {
	tags := make([]fixtureTag, 0, 4)
	if makeName != "" {
		tags = append(tags, fixtureTag{0x010f, 2, uint32(len(makeName) + 1), append([]byte(makeName), 0)})
	}
	if dng {
		tags = append(tags, fixtureTag{0xc612, 1, 4, []byte{1, 6, 0, 0}})
	}
	if rawTags {
		tags = append(tags,
			fixtureTag{0x014a, 4, 1, nil},
			fixtureTag{0x828d, 3, 2, []byte{2, 0, 2, 0}},
		)
	}
	ifdSize := 2 + len(tags)*12 + 4
	payloadOffset := 8 + ifdSize
	payload := make([]byte, 0)
	data := make([]byte, payloadOffset)
	copy(data, []byte{'I', 'I', 42, 0, 8, 0, 0, 0})
	binary.LittleEndian.PutUint16(data[8:10], uint16(len(tags)))
	for i, tag := range tags {
		entry := data[10+i*12 : 22+i*12]
		binary.LittleEndian.PutUint16(entry[0:2], tag.tag)
		binary.LittleEndian.PutUint16(entry[2:4], tag.typ)
		binary.LittleEndian.PutUint32(entry[4:8], tag.count)
		if tag.tag == 0x014a {
			binary.LittleEndian.PutUint32(entry[8:12], uint32(payloadOffset+len(payload)))
			payload = append(payload, make([]byte, 6)...)
		} else if len(tag.data) <= 4 {
			copy(entry[8:12], tag.data)
		} else {
			binary.LittleEndian.PutUint32(entry[8:12], uint32(payloadOffset+len(payload)))
			payload = append(payload, tag.data...)
		}
	}
	return append(data, payload...)
}

func ftypFixture(major string, compatible ...string) []byte {
	data := make([]byte, 16+4*len(compatible))
	binary.BigEndian.PutUint32(data[:4], uint32(len(data)))
	copy(data[4:8], "ftyp")
	copy(data[8:12], major)
	for i, brand := range compatible {
		copy(data[16+i*4:20+i*4], brand)
	}
	return data
}

func bmffBox(typ string, payload []byte) []byte {
	data := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(data[:4], uint32(len(data)))
	copy(data[4:8], typ)
	copy(data[8:], payload)
	return data
}

func tiffPointerFixture(tag uint16, pointers []uint32) []byte {
	data := make([]byte, 26)
	copy(data, []byte{'I', 'I', 42, 0, 8, 0, 0, 0})
	binary.LittleEndian.PutUint16(data[8:10], 1)
	binary.LittleEndian.PutUint16(data[10:12], tag)
	binary.LittleEndian.PutUint16(data[12:14], 4)
	binary.LittleEndian.PutUint32(data[14:18], uint32(len(pointers)))
	if len(pointers) == 1 {
		binary.LittleEndian.PutUint32(data[18:22], pointers[0])
		return data
	}
	binary.LittleEndian.PutUint32(data[18:22], uint32(len(data)))
	for _, pointer := range pointers {
		value := make([]byte, 4)
		binary.LittleEndian.PutUint32(value, pointer)
		data = append(data, value...)
	}
	return data
}

func setTIFFTagValue(t *testing.T, data []byte, tag uint16, value uint32) {
	t.Helper()
	count := int(binary.LittleEndian.Uint16(data[8:10]))
	for i := 0; i < count; i++ {
		entry := data[10+i*12 : 22+i*12]
		if binary.LittleEndian.Uint16(entry[:2]) == tag {
			binary.LittleEndian.PutUint32(entry[8:12], value)
			return
		}
	}
	t.Fatalf("TIFF fixture lacks tag %#04x", tag)
}
