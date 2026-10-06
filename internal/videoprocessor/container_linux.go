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
			if !walkBMFF(file, box.payloadOffset, box.end, 1, &boxCount, &facts) {
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
		*count++
		if *count > maxBMFFBoxes {
			return false
		}
		if box.typ == "stsd" {
			if !parseSTSD(file, box, facts) {
				return false
			}
		} else if box.typ == "av1C" {
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

func parseSTSD(file *os.File, box bmffBox, facts *bmffFacts) bool {
	if box.end-box.payloadOffset < 8 {
		return false
	}
	var header [8]byte
	if !readExactAt(file, box.payloadOffset, header[:]) {
		return false
	}
	entries := binary.BigEndian.Uint32(header[4:])
	if entries == 0 || entries > 32 {
		return false
	}
	offset := box.payloadOffset + 8
	for range entries {
		entry, ok := readBMFFBox(file, offset, box.end)
		if !ok || entry.next <= offset {
			return false
		}
		if isVideoEntry(entry.typ) {
			facts.video[entry.typ]++
		} else if isAudioEntry(entry.typ) {
			facts.audio[entry.typ]++
		}
		offset = entry.next
	}
	return offset == box.end
}

func isContainerBox(typ string) bool {
	switch typ {
	case "trak", "mdia", "minf", "stbl", "edts", "dinf", "mvex", "moof", "traf", "meta", "iprp", "ipco":
		return true
	default:
		return false
	}
}

func isVideoEntry(typ string) bool {
	switch typ {
	case "av01", "avc1", "avc3", "hvc1", "hev1", "vp08", "vp09":
		return true
	default:
		return false
	}
}

func isAudioEntry(typ string) bool {
	switch typ {
	case "mp4a", "ac-3", "ec-3", "Opus", "fLaC":
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
