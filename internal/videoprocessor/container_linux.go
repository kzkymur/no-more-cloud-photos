//go:build linux

package videoprocessor

import (
	"encoding/binary"
	"os"
)

const (
	maxBMFFBoxes = 100_000
	maxBMFFDepth = 16
)

type bmffFacts struct {
	majorBrand  string
	brands      map[string]bool
	ftyp, moov  int
	meta, mdat  int
	mdatPayload uint64
	video       map[string]int
	audio       map[string]int
}

type bmffBox struct {
	typ                string
	payloadOffset, end int64
	next               int64
}

func readBMFFBox(file *os.File, offset, limit int64) (bmffBox, bool) {
	if offset < 0 || limit-offset < 8 {
		return bmffBox{}, false
	}
	var header [16]byte
	if !readExactAt(file, offset, header[:8]) {
		return bmffBox{}, false
	}
	size := int64(binary.BigEndian.Uint32(header[:4]))
	headerSize := int64(8)
	if size == 1 {
		if limit-offset < 16 || !readExactAt(file, offset+8, header[8:]) {
			return bmffBox{}, false
		}
		large := binary.BigEndian.Uint64(header[8:])
		if large > uint64(^uint64(0)>>1) {
			return bmffBox{}, false
		}
		size, headerSize = int64(large), 16
	} else if size == 0 {
		size = limit - offset
	}
	if size < headerSize || size > limit-offset {
		return bmffBox{}, false
	}
	return bmffBox{typ: string(header[4:8]), payloadOffset: offset + headerSize, end: offset + size, next: offset + size}, true
}

func validateVideoContainer(file *os.File, size int64, kind string, audioExpected bool) bool {
	if file == nil || size < 24 {
		return false
	}
	facts := bmffFacts{brands: map[string]bool{}, video: map[string]int{}, audio: map[string]int{}}
	boxCount := 0
	for offset := int64(0); offset < size; {
		box, ok := readBMFFBox(file, offset, size)
		if !ok || box.next <= offset {
			return false
		}
		boxCount++
		if boxCount > maxBMFFBoxes {
			return false
		}
		switch box.typ {
		case "ftyp":
			facts.ftyp++
			if !parseFTYP(file, box, &facts) {
				return false
			}
		case "moov":
			facts.moov++
			valid := false
			if kind == "mp4-av1" {
				valid = parseMovie(file, box, &boxCount, &facts)
			} else {
				valid = walkBMFF(file, box.payloadOffset, box.end, 1, &boxCount, &facts)
			}
			if !valid {
				return false
			}
		case "meta":
			facts.meta++
			if box.end-box.payloadOffset < 4 || !walkBMFF(file, box.payloadOffset+4, box.end, 1, &boxCount, &facts) {
				return false
			}
		case "mdat":
			facts.mdat++
			payload := uint64(box.end - box.payloadOffset)
			if payload == 0 || facts.mdatPayload > ^uint64(0)-payload {
				return false
			}
			facts.mdatPayload += payload
		}
		offset = box.next
	}
	if facts.ftyp != 1 || facts.mdat == 0 || facts.mdatPayload == 0 {
		return false
	}
	if kind == "mp4-av1" {
		return facts.moov == 1 && facts.brands["av01"] && isMP4Brand(facts.majorBrand) &&
			facts.video["av01"] == 1 && totalEntries(facts.video) == 1 &&
			((audioExpected && facts.audio["mp4a"] == 1 && totalEntries(facts.audio) == 1) ||
				(!audioExpected && totalEntries(facts.audio) == 0))
	}
	return kind == "first-frame-avif" && facts.majorBrand == "avif" && !facts.brands["avis"] && facts.meta == 1 && facts.video["av01"] >= 1
}

func parseFTYP(file *os.File, box bmffBox, facts *bmffFacts) bool {
	payload := box.end - box.payloadOffset
	if payload < 8 || payload%4 != 0 || payload > 4096 {
		return false
	}
	data := make([]byte, payload)
	if !readExactAt(file, box.payloadOffset, data) {
		return false
	}
	facts.majorBrand = string(data[:4])
	facts.brands[facts.majorBrand] = true
	for offset := 8; offset < len(data); offset += 4 {
		facts.brands[string(data[offset:offset+4])] = true
	}
	return true
}

func walkBMFF(file *os.File, offset, limit int64, depth int, count *int, facts *bmffFacts) bool {
	if depth > maxBMFFDepth {
		return false
	}
	for offset < limit {
		box, ok := readBMFFBox(file, offset, limit)
		if !ok || box.next <= offset {
			return false
		}
		(*count)++
		if *count > maxBMFFBoxes {
			return false
		}
		if box.typ == "av1C" {
			facts.video["av01"]++
		} else if isContainerBox(box.typ) {
			childOffset := box.payloadOffset
			if box.typ == "meta" {
				if box.end-childOffset < 4 {
					return false
				}
				childOffset += 4
			}
			if !walkBMFF(file, childOffset, box.end, depth+1, count, facts) {
				return false
			}
		}
		offset = box.next
	}
	return offset == limit
}

func parseMovie(file *os.File, box bmffBox, count *int, facts *bmffFacts) bool {
	offset := box.payloadOffset
	for offset < box.end {
		child, ok := readCountedBMFFBox(file, offset, box.end, count)
		if !ok {
			return false
		}
		if child.typ == "trak" && !parseTrack(file, child, count, facts) {
			return false
		}
		offset = child.next
	}
	return offset == box.end
}

func parseTrack(file *os.File, box bmffBox, count *int, facts *bmffFacts) bool {
	var media bmffBox
	mediaCount := 0
	for offset := box.payloadOffset; offset < box.end; {
		child, ok := readCountedBMFFBox(file, offset, box.end, count)
		if !ok {
			return false
		}
		if child.typ == "mdia" {
			media, mediaCount = child, mediaCount+1
		}
		offset = child.next
	}
	return mediaCount == 1 && parseMedia(file, media, count, facts)
}

func parseMedia(file *os.File, box bmffBox, count *int, facts *bmffFacts) bool {
	var handler, mediaInfo bmffBox
	handlerCount, mediaInfoCount := 0, 0
	for offset := box.payloadOffset; offset < box.end; {
		child, ok := readCountedBMFFBox(file, offset, box.end, count)
		if !ok {
			return false
		}
		switch child.typ {
		case "hdlr":
			handler, handlerCount = child, handlerCount+1
		case "minf":
			mediaInfo, mediaInfoCount = child, mediaInfoCount+1
		}
		offset = child.next
	}
	if handlerCount != 1 || mediaInfoCount != 1 {
		return false
	}
	handlerType, ok := parseHandler(file, handler)
	if !ok || handlerType != "vide" && handlerType != "soun" || !parseMediaInfo(file, mediaInfo, handlerType, count) {
		return false
	}
	if handlerType == "vide" {
		facts.video["av01"]++
	} else {
		facts.audio["mp4a"]++
	}
	return true
}

func parseHandler(file *os.File, box bmffBox) (string, bool) {
	// FullBox header, pre_defined, handler_type, and three reserved words.
	if box.end-box.payloadOffset < 24 {
		return "", false
	}
	var data [12]byte
	if !readExactAt(file, box.payloadOffset, data[:]) || data[0] != 0 {
		return "", false
	}
	return string(data[8:12]), true
}

func parseMediaInfo(file *os.File, box bmffBox, handler string, count *int) bool {
	var sampleTable bmffBox
	sampleTableCount := 0
	for offset := box.payloadOffset; offset < box.end; {
		child, ok := readCountedBMFFBox(file, offset, box.end, count)
		if !ok {
			return false
		}
		if child.typ == "stbl" {
			sampleTable, sampleTableCount = child, sampleTableCount+1
		}
		offset = child.next
	}
	if sampleTableCount != 1 {
		return false
	}
	var sampleDescription bmffBox
	sampleDescriptionCount := 0
	for offset := sampleTable.payloadOffset; offset < sampleTable.end; {
		child, ok := readCountedBMFFBox(file, offset, sampleTable.end, count)
		if !ok {
			return false
		}
		if child.typ == "stsd" {
			sampleDescription, sampleDescriptionCount = child, sampleDescriptionCount+1
		}
		offset = child.next
	}
	return sampleDescriptionCount == 1 && parseSTSD(file, sampleDescription, handler, count)
}

func parseSTSD(file *os.File, box bmffBox, handler string, count *int) bool {
	if box.end-box.payloadOffset < 8 {
		return false
	}
	var header [8]byte
	if !readExactAt(file, box.payloadOffset, header[:]) {
		return false
	}
	entries := binary.BigEndian.Uint32(header[4:])
	if header[0] != 0 || entries != 1 {
		return false
	}
	offset := box.payloadOffset + 8
	for range entries {
		entry, ok := readCountedBMFFBox(file, offset, box.end, count)
		if !ok {
			return false
		}
		if handler == "vide" {
			if entry.typ != "av01" || !parseVisualSampleEntry(file, entry, count) {
				return false
			}
		} else if entry.typ != "mp4a" || !parseAudioSampleEntry(file, entry, count) {
			return false
		}
		offset = entry.next
	}
	return offset == box.end
}

func parseVisualSampleEntry(file *os.File, entry bmffBox, count *int) bool {
	// SampleEntry (8 bytes) followed by the 70-byte VisualSampleEntry fields.
	if entry.end-entry.payloadOffset < 78 {
		return false
	}
	var data [78]byte
	if !readExactAt(file, entry.payloadOffset, data[:]) || binary.BigEndian.Uint16(data[6:8]) == 0 ||
		binary.BigEndian.Uint16(data[24:26]) == 0 || binary.BigEndian.Uint16(data[26:28]) == 0 ||
		binary.BigEndian.Uint32(data[28:32]) == 0 || binary.BigEndian.Uint32(data[32:36]) == 0 ||
		binary.BigEndian.Uint16(data[40:42]) == 0 || data[42] > 31 ||
		binary.BigEndian.Uint16(data[74:76]) == 0 {
		return false
	}
	av1C := 0
	for offset := entry.payloadOffset + int64(len(data)); offset < entry.end; {
		child, ok := readCountedBMFFBox(file, offset, entry.end, count)
		if !ok {
			return false
		}
		if child.typ == "av1C" {
			av1C++
			var config [4]byte
			if child.end-child.payloadOffset < int64(len(config)) || !readExactAt(file, child.payloadOffset, config[:]) || config[0] != 0x81 {
				return false
			}
		}
		offset = child.next
	}
	return av1C == 1
}

func parseAudioSampleEntry(file *os.File, entry bmffBox, count *int) bool {
	// This output contract uses version-0 AudioSampleEntry with an ES descriptor.
	if entry.end-entry.payloadOffset < 28 {
		return false
	}
	var data [28]byte
	if !readExactAt(file, entry.payloadOffset, data[:]) || binary.BigEndian.Uint16(data[6:8]) == 0 ||
		binary.BigEndian.Uint16(data[8:10]) != 0 || binary.BigEndian.Uint16(data[16:18]) == 0 ||
		binary.BigEndian.Uint16(data[18:20]) == 0 || binary.BigEndian.Uint32(data[24:28]) == 0 {
		return false
	}
	esds := 0
	for offset := entry.payloadOffset + int64(len(data)); offset < entry.end; {
		child, ok := readCountedBMFFBox(file, offset, entry.end, count)
		if !ok {
			return false
		}
		if child.typ == "esds" {
			esds++
			if !parseESDS(file, child) {
				return false
			}
		}
		offset = child.next
	}
	return esds == 1
}

func parseESDS(file *os.File, box bmffBox) bool {
	if box.end-box.payloadOffset < 4 {
		return false
	}
	var fullBox [4]byte
	if !readExactAt(file, box.payloadOffset, fullBox[:]) || fullBox[0] != 0 {
		return false
	}
	tag, payload, end, ok := readDescriptor(file, box.payloadOffset+4, box.end)
	if !ok || tag != 0x03 || end != box.end || end-payload < 3 {
		return false
	}
	var flags [3]byte
	if !readExactAt(file, payload, flags[:]) {
		return false
	}
	offset := payload + 3
	if flags[2]&0x80 != 0 {
		if end-offset < 2 {
			return false
		}
		offset += 2
	}
	if flags[2]&0x40 != 0 {
		var length [1]byte
		if offset >= end || !readExactAt(file, offset, length[:]) {
			return false
		}
		if int64(length[0]) > end-offset-1 {
			return false
		}
		offset += 1 + int64(length[0])
	}
	if flags[2]&0x20 != 0 {
		if end-offset < 2 {
			return false
		}
		offset += 2
	}
	decoderTag, decoderPayload, decoderEnd, ok := readDescriptor(file, offset, end)
	if !ok || decoderTag != 0x04 || decoderEnd-decoderPayload < 13 {
		return false
	}
	var decoder [13]byte
	if !readExactAt(file, decoderPayload, decoder[:]) || decoder[0] != 0x40 || decoder[1]>>2 != 0x05 {
		return false
	}
	configTag, configPayload, configEnd, ok := readDescriptor(file, decoderPayload+13, decoderEnd)
	if !ok || configTag != 0x05 || configEnd-configPayload < 2 {
		return false
	}
	var config [2]byte
	// Audio Object Type 2 is AAC Low Complexity, the required output profile.
	return readExactAt(file, configPayload, config[:]) && config[0]>>3 == 2
}

func readDescriptor(file *os.File, offset, limit int64) (byte, int64, int64, bool) {
	if offset < 0 || offset >= limit {
		return 0, 0, 0, false
	}
	var value [1]byte
	if !readExactAt(file, offset, value[:]) {
		return 0, 0, 0, false
	}
	tag := value[0]
	offset++
	length := uint64(0)
	for i := 0; i < 4; i++ {
		if offset >= limit || !readExactAt(file, offset, value[:]) || length > (^uint64(0)>>7) {
			return 0, 0, 0, false
		}
		offset++
		length = length<<7 | uint64(value[0]&0x7f)
		if value[0]&0x80 == 0 {
			if length > uint64(limit-offset) {
				return 0, 0, 0, false
			}
			return tag, offset, offset + int64(length), true
		}
	}
	return 0, 0, 0, false
}

func readCountedBMFFBox(file *os.File, offset, limit int64, count *int) (bmffBox, bool) {
	box, ok := readBMFFBox(file, offset, limit)
	if !ok || box.next <= offset || *count >= maxBMFFBoxes {
		return bmffBox{}, false
	}
	(*count)++
	return box, true
}

func isContainerBox(typ string) bool {
	switch typ {
	case "trak", "mdia", "minf", "stbl", "edts", "dinf", "mvex", "moof", "traf", "meta", "iprp", "ipco":
		return true
	default:
		return false
	}
}

func isMP4Brand(brand string) bool {
	switch brand {
	case "isom", "iso2", "iso5", "iso6", "mp41", "mp42", "av01":
		return true
	default:
		return false
	}
}

func totalEntries(entries map[string]int) int {
	total := 0
	for _, count := range entries {
		total += count
	}
	return total
}

func readExactAt(file *os.File, offset int64, buffer []byte) bool {
	n, err := file.ReadAt(buffer, offset)
	return n == len(buffer) && err == nil
}
