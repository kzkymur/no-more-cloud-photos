package metadata

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"strings"

	"github.com/kzkymur/no-more-cloud-photos/internal/mediaformat"
)

const (
	maxJPEGSegments = 64
	maxBMFFBoxes    = 32
	maxTIFFIFDs     = 32
	maxTIFFTags     = 2048
	maxIFDEntries   = 512
	maxInspectBytes = int64(1 << 20)
)

// Detect identifies a registered media format using only bounded reads from
// the supplied complete file.
func Detect(reader io.ReaderAt, size int64) (Detection, error) {
	if reader == nil || size < 0 {
		return Detection{}, invalid("invalid reader or size")
	}
	reader = &inspectionReader{reader: reader, remaining: maxInspectBytes}
	headerLen := size
	if headerLen > 128 {
		headerLen = 128
	}
	header, err := readAt(reader, size, 0, headerLen)
	if err != nil {
		return Detection{}, err
	}

	switch {
	case hasPrefix(header, []byte{0xff, 0xd8}):
		return detectJPEG(reader, size)
	case hasPrefix(header, []byte("\x89PNG\r\n\x1a\n")):
		return detectPNG(reader, size)
	case hasPrefix(header, []byte("GIF87a")) || hasPrefix(header, []byte("GIF89a")):
		return detectGIF(header, size)
	case hasPrefix(header, []byte("RIFF")):
		return detectWebP(reader, header, size)
	case hasPrefix(header, []byte("BM")):
		return detectBMP(header, size)
	case hasPrefix(header, []byte("FUJIFILMCCD-RAW ")):
		return detectRAF(header, size)
	case isTIFFPrefix(header):
		return detectTIFF(reader, size, header)
	case looksLikeBMFF(header):
		return detectBMFF(reader, size)
	case len(header) >= 2 && (bytes.Equal(header[:2], []byte("II")) || bytes.Equal(header[:2], []byte("MM"))):
		return Detection{}, invalid("truncated TIFF header")
	case partialPrefix(header, []byte("\x89PNG\r\n\x1a\n")) ||
		partialPrefix(header, []byte("GIF87a")) ||
		partialPrefix(header, []byte("GIF89a")) ||
		partialPrefix(header, []byte("RIFF")) ||
		partialPrefix(header, []byte("BM")) ||
		partialPrefix(header, []byte("FUJIFILMCCD-RAW ")):
		return Detection{}, invalid("truncated format signature")
	default:
		return Detection{}, ErrUnsupportedMediaType
	}
}

type inspectionReader struct {
	reader    io.ReaderAt
	remaining int64
}

func (r *inspectionReader) ReadAt(p []byte, offset int64) (int, error) {
	if int64(len(p)) > r.remaining {
		return 0, invalid("inspection byte limit exceeded")
	}
	r.remaining -= int64(len(p))
	return r.reader.ReadAt(p, offset)
}

func detection(mimeType, evidence string) (Detection, error) {
	format, ok := mediaformat.Lookup(mimeType)
	if !ok {
		return Detection{}, fmt.Errorf("%w: detector format %q is not registered", ErrInvalidMedia, mimeType)
	}
	return Detection{Format: format, Evidence: evidence}, nil
}

func invalid(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidMedia, reason)
}

func readAt(reader io.ReaderAt, size, offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 || offset > size || length > size-offset || uint64(length) > uint64(^uint(0)>>1) {
		return nil, invalid("offset or length outside file")
	}
	buf := make([]byte, int(length))
	if length == 0 {
		return buf, nil
	}
	n, err := reader.ReadAt(buf, offset)
	if n != len(buf) || (err != nil && err != io.EOF) {
		return nil, invalid("short content read")
	}
	return buf, nil
}

func hasPrefix(data, prefix []byte) bool {
	return len(data) >= len(prefix) && bytes.Equal(data[:len(prefix)], prefix)
}

func partialPrefix(data, signature []byte) bool {
	return len(data) < len(signature) && len(data) > 0 && bytes.Equal(data, signature[:len(data)])
}

func detectJPEG(reader io.ReaderAt, size int64) (Detection, error) {
	if size < 4 {
		return Detection{}, invalid("truncated JPEG")
	}
	offset := int64(2)
	for segment := 0; segment < maxJPEGSegments && offset < size && offset < maxInspectBytes; segment++ {
		marker, err := readAt(reader, size, offset, 2)
		if err != nil {
			return Detection{}, err
		}
		if marker[0] != 0xff || marker[1] == 0x00 || marker[1] == 0xff || marker[1] == 0xd8 {
			return Detection{}, invalid("invalid JPEG marker")
		}
		offset += 2
		if marker[1] == 0xd9 {
			return detection("image/jpeg", "jpeg-marker-stream")
		}
		if marker[1] >= 0xd0 && marker[1] <= 0xd7 {
			continue
		}
		lengthBytes, err := readAt(reader, size, offset, 2)
		if err != nil {
			return Detection{}, err
		}
		length := int64(binary.BigEndian.Uint16(lengthBytes))
		if length < 2 || length > size-offset {
			return Detection{}, invalid("invalid JPEG segment length")
		}
		offset += length
		if marker[1] == 0xda {
			return detection("image/jpeg", "jpeg-marker-stream")
		}
	}
	return Detection{}, invalid("JPEG marker inspection limit exceeded")
}

func detectPNG(reader io.ReaderAt, size int64) (Detection, error) {
	if size < 33 {
		return Detection{}, invalid("truncated PNG IHDR")
	}
	chunk, err := readAt(reader, size, 8, 25)
	if err != nil {
		return Detection{}, err
	}
	if binary.BigEndian.Uint32(chunk[:4]) != 13 || string(chunk[4:8]) != "IHDR" ||
		binary.BigEndian.Uint32(chunk[8:12]) == 0 || binary.BigEndian.Uint32(chunk[12:16]) == 0 {
		return Detection{}, invalid("invalid PNG IHDR")
	}
	if crc32.ChecksumIEEE(chunk[4:21]) != binary.BigEndian.Uint32(chunk[21:25]) {
		return Detection{}, invalid("invalid PNG IHDR checksum")
	}
	return detection("image/png", "png-signature-ihdr")
}

func detectGIF(header []byte, size int64) (Detection, error) {
	if size < 13 || len(header) < 13 || binary.LittleEndian.Uint16(header[6:8]) == 0 || binary.LittleEndian.Uint16(header[8:10]) == 0 {
		return Detection{}, invalid("invalid GIF logical screen descriptor")
	}
	return detection("image/gif", "gif-header-logical-screen")
}

func detectWebP(reader io.ReaderAt, header []byte, size int64) (Detection, error) {
	if size < 20 || len(header) < 20 || string(header[8:12]) != "WEBP" {
		return Detection{}, invalid("invalid WebP RIFF header")
	}
	declared := uint64(binary.LittleEndian.Uint32(header[4:8])) + 8
	if declared != uint64(size) || declared < 20 {
		return Detection{}, invalid("invalid WebP RIFF size")
	}
	chunkSize := int64(binary.LittleEndian.Uint32(header[16:20]))
	padded := chunkSize + chunkSize%2
	if chunkSize < 0 || padded > size-20 {
		return Detection{}, invalid("invalid WebP chunk size")
	}
	switch string(header[12:16]) {
	case "VP8 ", "VP8L", "VP8X":
	default:
		return Detection{}, invalid("invalid WebP first chunk")
	}
	// Force a bounded read of the declared chunk endpoint through ReaderAt.
	if padded > 0 {
		if _, err := readAt(reader, size, 20+padded-1, 1); err != nil {
			return Detection{}, err
		}
	}
	return detection("image/webp", "riff-webp-chunk")
}

func detectBMP(header []byte, size int64) (Detection, error) {
	if size < 18 || len(header) < 18 {
		return Detection{}, invalid("truncated BMP header")
	}
	declared := int64(binary.LittleEndian.Uint32(header[2:6]))
	pixelOffset := int64(binary.LittleEndian.Uint32(header[10:14]))
	dibSize := int64(binary.LittleEndian.Uint32(header[14:18]))
	if declared != size || dibSize < 12 || dibSize > size-14 || pixelOffset < 14+dibSize || pixelOffset > size {
		return Detection{}, invalid("invalid BMP sizes")
	}
	return detection("image/bmp", "bmp-file-dib-header")
}

func detectRAF(header []byte, size int64) (Detection, error) {
	if size < 100 || len(header) < 100 {
		return Detection{}, invalid("truncated RAF header")
	}
	jpegOffset := int64(binary.BigEndian.Uint32(header[84:88]))
	jpegLength := int64(binary.BigEndian.Uint32(header[88:92]))
	rawOffset := int64(binary.BigEndian.Uint32(header[92:96]))
	rawLength := int64(binary.BigEndian.Uint32(header[96:100]))
	if !validRange(jpegOffset, jpegLength, size) || !validRange(rawOffset, rawLength, size) || jpegLength == 0 || rawLength == 0 {
		return Detection{}, invalid("invalid RAF data ranges")
	}
	return detection("image/x-fuji-raf", "raf-header-data-ranges")
}

func validRange(offset, length, size int64) bool {
	return offset >= 0 && length >= 0 && offset <= size && length <= size-offset
}

func isTIFFPrefix(header []byte) bool {
	return len(header) >= 4 && ((header[0] == 'I' && header[1] == 'I') || (header[0] == 'M' && header[1] == 'M'))
}

type tiffEvidence struct {
	dng, subIFD, cfa bool
	make             string
}

func detectTIFF(reader io.ReaderAt, size int64, header []byte) (Detection, error) {
	if size < 8 || len(header) < 8 {
		return Detection{}, invalid("truncated TIFF header")
	}
	var order binary.ByteOrder
	if header[0] == 'I' && header[1] == 'I' {
		order = binary.LittleEndian
	} else if header[0] == 'M' && header[1] == 'M' {
		order = binary.BigEndian
	} else {
		return Detection{}, invalid("invalid TIFF byte order")
	}
	magic := order.Uint16(header[2:4])
	if magic == 0x4f52 || magic == 0x5352 {
		if err := validateTIFFIFDs(reader, size, order, nil, uint64(order.Uint32(header[4:8]))); err != nil {
			return Detection{}, err
		}
		return detection("image/x-olympus-orf", "orf-tiff-magic")
	}
	if magic == 0x55 {
		if err := validateTIFFIFDs(reader, size, order, nil, uint64(order.Uint32(header[4:8]))); err != nil {
			return Detection{}, err
		}
		return detection("image/x-panasonic-rw2", "rw2-tiff-magic")
	}
	if magic != 42 {
		return Detection{}, invalid("invalid TIFF magic")
	}
	if size >= 16 && len(header) >= 16 && bytes.Equal(header[8:12], []byte{'C', 'R', 2, 0}) {
		rawIFD := uint64(order.Uint32(header[12:16]))
		if rawIFD == 0 || rawIFD > uint64(size) || uint64(size)-rawIFD < 2 {
			return Detection{}, invalid("CR2 raw IFD offset outside file")
		}
		if err := validateTIFFIFDs(reader, size, order, nil, uint64(order.Uint32(header[4:8])), rawIFD); err != nil {
			return Detection{}, err
		}
		return detection("image/x-canon-cr2", "cr2-tiff-signature")
	}

	evidence := &tiffEvidence{}
	if err := validateTIFFIFDs(reader, size, order, evidence, uint64(order.Uint32(header[4:8]))); err != nil {
		return Detection{}, err
	}
	rawConjunction := evidence.subIFD && evidence.cfa
	matches := 0
	mimeType := ""
	reason := ""
	if evidence.dng {
		matches++
		mimeType, reason = "image/dng", "tiff-dng-version-tag"
	}
	if strings.HasPrefix(evidence.make, "NIKON") && rawConjunction {
		matches++
		mimeType, reason = "image/x-nikon-nef", "tiff-nikon-raw-tag-conjunction"
	}
	if strings.HasPrefix(evidence.make, "SONY") && rawConjunction {
		matches++
		mimeType, reason = "image/x-sony-arw", "tiff-sony-raw-tag-conjunction"
	}
	if matches > 1 {
		return Detection{}, invalid("conflicting TIFF format evidence")
	}
	if matches == 0 {
		return Detection{}, ErrUnsupportedMediaType
	}
	return detection(mimeType, reason)
}

type tiffIFDPointer struct {
	offset uint64
}

func validateTIFFIFDs(reader io.ReaderAt, size int64, order binary.ByteOrder, evidence *tiffEvidence, roots ...uint64) error {
	if len(roots) == 0 || len(roots) > maxTIFFIFDs {
		return invalid("invalid TIFF IFD roots")
	}
	queue := make([]tiffIFDPointer, 0, len(roots))
	scheduled := make(map[uint64]struct{}, len(roots))
	for _, root := range roots {
		if root == 0 {
			return invalid("zero TIFF first IFD offset")
		}
		if _, ok := scheduled[root]; ok {
			continue
		}
		queue = append(queue, tiffIFDPointer{offset: root})
		scheduled[root] = struct{}{}
	}
	seen := make(map[uint64]struct{})
	edges := make(map[uint64][]uint64)
	subIFDTargets := make(map[uint64]struct{})
	tagCount := 0
	for len(queue) > 0 {
		if len(seen) >= maxTIFFIFDs {
			return invalid("TIFF IFD limit exceeded")
		}
		pointer := queue[0]
		queue = queue[1:]
		offset := pointer.offset
		seen[offset] = struct{}{}
		if offset > uint64(size) || uint64(size)-offset < 2 {
			return invalid("TIFF IFD offset outside file")
		}
		countBytes, err := readAt(reader, size, int64(offset), 2)
		if err != nil {
			return err
		}
		count := uint64(order.Uint16(countBytes))
		if count > maxIFDEntries || tagCount+int(count) > maxTIFFTags {
			return invalid("TIFF tag limit exceeded")
		}
		if count > (^uint64(0)-6)/12 {
			return invalid("TIFF IFD length overflow")
		}
		ifdLength := uint64(2) + count*12 + 4
		if offset > uint64(size) || ifdLength > uint64(size)-offset || ifdLength > uint64(maxInspectBytes) {
			return invalid("TIFF IFD outside inspection bounds")
		}
		ifd, err := readAt(reader, size, int64(offset), int64(ifdLength))
		if err != nil {
			return err
		}
		tagCount += int(count)
		for i := uint64(0); i < count; i++ {
			entry := ifd[2+i*12 : 2+(i+1)*12]
			tag := order.Uint16(entry[0:2])
			typ := order.Uint16(entry[2:4])
			valueCount := uint64(order.Uint32(entry[4:8]))
			typeSize, ok := tiffTypeSize(typ)
			if !ok {
				return invalid("unknown TIFF field type")
			}
			if valueCount != 0 && typeSize > ^uint64(0)/valueCount {
				return invalid("TIFF value length overflow")
			}
			valueLength := typeSize * valueCount
			value := entry[8:12]
			pointerTag := tag == 0x014a || tag == 0x8769 || tag == 0x8825 || tag == 0xa005
			if pointerTag {
				if typ != 4 || valueCount == 0 || (tag != 0x014a && valueCount != 1) || valueCount > maxTIFFIFDs {
					return invalid("invalid TIFF IFD pointer tag")
				}
			}
			if valueLength > 4 {
				valueOffset := uint64(order.Uint32(entry[8:12]))
				if valueOffset > uint64(size) || valueLength > uint64(size)-valueOffset {
					return invalid("TIFF value outside file")
				}
				if valueLength <= 4096 || pointerTag {
					value, err = readAt(reader, size, int64(valueOffset), int64(valueLength))
					if err != nil {
						return err
					}
				} else {
					value = nil
				}
			} else {
				value = value[:valueLength]
			}
			if pointerTag {
				for pos := uint64(0); pos+4 <= uint64(len(value)); pos += 4 {
					pointedOffset := uint64(order.Uint32(value[pos : pos+4]))
					if pointedOffset == 0 {
						continue
					}
					if pointedOffset > uint64(size) || uint64(size)-pointedOffset < 2 {
						return invalid("TIFF IFD pointer outside file")
					}
					edges[offset] = append(edges[offset], pointedOffset)
					if tag == 0x014a {
						subIFDTargets[pointedOffset] = struct{}{}
					}
					if _, ok := scheduled[pointedOffset]; ok {
						continue
					}
					if len(scheduled) >= maxTIFFIFDs {
						return invalid("TIFF IFD limit exceeded")
					}
					queue = append(queue, tiffIFDPointer{offset: pointedOffset})
					scheduled[pointedOffset] = struct{}{}
				}
			}
			if evidence == nil {
				continue
			}
			switch tag {
			case 0x010f: // Make
				if typ == 2 && value != nil {
					evidence.make = strings.ToUpper(strings.TrimSpace(strings.TrimRight(string(value), "\x00")))
				}
			case 0x828d, 0x828e: // CFARepeatPatternDim, CFAPattern
				evidence.cfa = (typ == 1 || typ == 3 || typ == 7) && valueCount > 0
			case 0xc612: // DNGVersion
				evidence.dng = typ == 1 && valueCount == 4 && len(value) == 4 && value[0] != 0
			}
		}
		next := uint64(order.Uint32(ifd[len(ifd)-4:]))
		if next != 0 {
			if next > uint64(size) || uint64(size)-next < 2 {
				return invalid("TIFF next IFD outside file")
			}
			edges[offset] = append(edges[offset], next)
			if _, ok := scheduled[next]; ok {
				continue
			}
			if len(scheduled) >= maxTIFFIFDs {
				return invalid("TIFF IFD limit exceeded")
			}
			queue = append(queue, tiffIFDPointer{offset: next})
			scheduled[next] = struct{}{}
		}
	}
	if hasTIFFIFDCycle(edges) {
		return invalid("TIFF IFD loop")
	}
	if evidence != nil {
		for target := range subIFDTargets {
			if _, ok := seen[target]; ok {
				evidence.subIFD = true
				break
			}
		}
	}
	return nil
}

func hasTIFFIFDCycle(edges map[uint64][]uint64) bool {
	state := make(map[uint64]uint8, len(edges))
	var visit func(uint64) bool
	visit = func(offset uint64) bool {
		if state[offset] == 1 {
			return true
		}
		if state[offset] == 2 {
			return false
		}
		state[offset] = 1
		for _, next := range edges[offset] {
			if visit(next) {
				return true
			}
		}
		state[offset] = 2
		return false
	}
	for offset := range edges {
		if visit(offset) {
			return true
		}
	}
	return false
}

func tiffTypeSize(typ uint16) (uint64, bool) {
	switch typ {
	case 1, 2, 6, 7:
		return 1, true
	case 3, 8:
		return 2, true
	case 4, 9, 11:
		return 4, true
	case 5, 10, 12:
		return 8, true
	case 13:
		return 4, true
	default:
		return 0, false
	}
}

func looksLikeBMFF(header []byte) bool {
	if len(header) < 8 {
		return false
	}
	typ := string(header[4:8])
	return typ == "ftyp" || typ == "free" || typ == "skip" || typ == "wide" || typ == "moov"
}

func detectBMFF(reader io.ReaderAt, size int64) (Detection, error) {
	offset := int64(0)
	var result Detection
	var classificationErr error
	hasFTYP := false
	ftypNotFirst := false
	var legacyMoovOffset int64
	var legacyMoovSize, legacyMoovHeader uint64
	for boxes := 0; offset < size; boxes++ {
		if boxes >= maxBMFFBoxes {
			return Detection{}, invalid("BMFF box inspection limit exceeded")
		}
		if size-offset < 8 {
			return Detection{}, invalid("trailing data after BMFF boxes")
		}
		header, err := readAt(reader, size, offset, 8)
		if err != nil {
			return Detection{}, err
		}
		boxSize := uint64(binary.BigEndian.Uint32(header[:4]))
		headerSize := uint64(8)
		if boxSize == 1 {
			extended, err := readAt(reader, size, offset+8, 8)
			if err != nil {
				return Detection{}, err
			}
			boxSize = binary.BigEndian.Uint64(extended)
			headerSize = 16
		} else if boxSize == 0 {
			boxSize = uint64(size - offset)
		}
		if boxSize < headerSize || boxSize > uint64(size-offset) {
			return Detection{}, invalid("invalid BMFF box size")
		}
		boxType := string(header[4:8])
		if boxType == "ftyp" {
			if hasFTYP {
				return Detection{}, invalid("multiple BMFF ftyp boxes")
			}
			hasFTYP = true
			if legacyMoovSize != 0 {
				ftypNotFirst = true
			}
			result, classificationErr = classifyFTYP(reader, size, offset, boxSize, headerSize)
		} else if !hasFTYP && boxType != "free" && boxType != "skip" && boxType != "wide" {
			if boxType == "moov" && legacyMoovSize == 0 {
				legacyMoovOffset, legacyMoovSize, legacyMoovHeader = offset, boxSize, headerSize
			} else if legacyMoovSize == 0 {
				ftypNotFirst = true
			}
		}
		offset += int64(boxSize)
	}
	if !hasFTYP {
		if !ftypNotFirst && legacyMoovSize != 0 {
			return detectLegacyQuickTime(reader, size, legacyMoovOffset, legacyMoovSize, legacyMoovHeader)
		}
		return Detection{}, ErrUnsupportedMediaType
	}
	if ftypNotFirst {
		return Detection{}, invalid("BMFF ftyp is not first meaningful box")
	}
	if classificationErr != nil {
		return Detection{}, classificationErr
	}
	return result, nil
}

// detectLegacyQuickTime recognizes pre-ftyp QuickTime only when the moov has
// both required movie structure and a legacy QuickTime mhlr/dhlr handler.
// mvhd/trak alone are generic ISO-BMFF evidence and are deliberately rejected.
func detectLegacyQuickTime(reader io.ReaderAt, size, offset int64, boxSize, headerSize uint64) (Detection, error) {
	if boxSize > uint64(maxInspectBytes) {
		return Detection{}, invalid("legacy QuickTime moov exceeds inspection limit")
	}
	payload, err := readAt(reader, size, offset+int64(headerSize), int64(boxSize-headerSize))
	if err != nil {
		return Detection{}, err
	}
	hasMovieHeader, hasTrack := false, false
	for childOffset, children := 0, 0; childOffset < len(payload); children++ {
		if children >= 64 || len(payload)-childOffset < 8 {
			return Detection{}, invalid("invalid legacy QuickTime child boxes")
		}
		childSize := int(binary.BigEndian.Uint32(payload[childOffset : childOffset+4]))
		if childSize < 8 || childSize > len(payload)-childOffset {
			return Detection{}, invalid("invalid legacy QuickTime child box size")
		}
		switch string(payload[childOffset+4 : childOffset+8]) {
		case "mvhd":
			hasMovieHeader = childSize >= 28
		case "trak":
			hasTrack = true
		}
		childOffset += childSize
	}
	legacyHandler, validChildren := findLegacyQuickTimeHandler(payload, 0)
	if !validChildren {
		return Detection{}, invalid("invalid legacy QuickTime nested boxes")
	}
	if !hasMovieHeader || !hasTrack || !legacyHandler {
		return Detection{}, ErrUnsupportedMediaType
	}
	return detection("video/quicktime", "quicktime-legacy-handler-structure")
}

func findLegacyQuickTimeHandler(data []byte, depth int) (bool, bool) {
	if depth > 8 {
		return false, false
	}
	found := false
	for offset, boxes := 0, 0; offset < len(data); boxes++ {
		if boxes >= 256 || len(data)-offset < 8 {
			return false, false
		}
		size := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
		headerSize := uint64(8)
		if size == 1 {
			if len(data)-offset < 16 {
				return false, false
			}
			size = binary.BigEndian.Uint64(data[offset+8 : offset+16])
			headerSize = 16
		} else if size == 0 {
			size = uint64(len(data) - offset)
		}
		if size < headerSize || size > uint64(len(data)-offset) {
			return false, false
		}
		boxType := string(data[offset+4 : offset+8])
		payloadStart := offset + int(headerSize)
		boxEnd := offset + int(size)
		if boxType == "hdlr" && boxEnd-payloadStart >= 8 {
			componentType := string(data[payloadStart+4 : payloadStart+8])
			if componentType == "mhlr" || componentType == "dhlr" {
				found = true
			}
		}
		if legacyQuickTimeContainer(boxType) {
			childStart := payloadStart
			childFound, valid := findLegacyQuickTimeHandler(data[childStart:boxEnd], depth+1)
			if !valid {
				return false, false
			}
			found = found || childFound
		}
		offset = boxEnd
	}
	return found, true
}

func legacyQuickTimeContainer(boxType string) bool {
	switch boxType {
	case "trak", "mdia", "minf", "dinf", "stbl", "edts", "udta":
		return true
	default:
		return false
	}
}

func classifyFTYP(reader io.ReaderAt, size, offset int64, boxSize, headerSize uint64) (Detection, error) {
	if boxSize < headerSize+8 || (boxSize-headerSize-8)%4 != 0 || boxSize > uint64(maxInspectBytes) {
		return Detection{}, invalid("invalid BMFF ftyp payload")
	}
	payload, err := readAt(reader, size, offset+int64(headerSize), int64(boxSize-headerSize))
	if err != nil {
		return Detection{}, err
	}
	brands := []string{string(payload[:4])}
	for i := 8; i < len(payload); i += 4 {
		brands = append(brands, string(payload[i:i+4]))
	}
	var heic, heif, explicitHEIF, sequence, cr3, quickTime, mp4 bool
	for _, brand := range brands {
		switch brand {
		case "heic", "heix", "heim", "heis":
			heic = true
		case "hevc", "hevx", "hevm", "hevs", "msf1":
			sequence = true
		case "heif":
			heif = true
			explicitHEIF = true
		case "mif1":
			heif = true
		case "crx ":
			cr3 = true
		case "qt  ":
			quickTime = true
		case "mp41", "mp42", "M4V ", "avc1", "dash":
			mp4 = true
		}
	}
	// Generic base brands such as mif1 and isom do not conflict with a more
	// specific compatible brand.
	image := heic || heif
	if (sequence && !image && (cr3 || quickTime || mp4)) || (heic && explicitHEIF) || boolCount(image, cr3, quickTime, mp4) > 1 {
		return Detection{}, invalid("conflicting BMFF brands")
	}
	if sequence {
		return Detection{}, ErrUnsupportedMediaType
	}
	// The major brand is the canonical container declaration. A generic mif1
	// major brand remains HEIF even when compatible item brands
	// advertise how its payload is encoded.
	switch brands[0] {
	case "mif1":
		return detection("image/heif", "bmff-major-brand-heif")
	case "heic", "heix", "heim", "heis":
		return detection("image/heic", "bmff-major-brand-heic")
	}
	switch {
	case cr3:
		return detection("image/x-canon-cr3", "bmff-ftyp-crx")
	case heic:
		return detection("image/heic", "bmff-ftyp-heic")
	case heif:
		return detection("image/heif", "bmff-ftyp-heif")
	case quickTime:
		return detection("video/quicktime", "bmff-ftyp-quicktime")
	case mp4:
		return detection("video/mp4", "bmff-ftyp-mp4")
	default:
		return Detection{}, ErrUnsupportedMediaType
	}
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}
