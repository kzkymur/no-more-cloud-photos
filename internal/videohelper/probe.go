//go:build linux

package videohelper

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/bits"
	"strconv"
	"strings"
)

type probeDocument struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		FormatName string `json:"format_name"`
	} `json:"format"`
}

type probeStream struct {
	Index          int    `json:"index"`
	CodecName      string `json:"codec_name"`
	CodecType      string `json:"codec_type"`
	Profile        string `json:"profile"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	PixFmt         string `json:"pix_fmt"`
	BitsPerRaw     string `json:"bits_per_raw_sample"`
	SampleAspect   string `json:"sample_aspect_ratio"`
	TimeBase       string `json:"time_base"`
	DurationTicks  int64  `json:"duration_ts"`
	ColorPrimaries string `json:"color_primaries"`
	ColorTransfer  string `json:"color_transfer"`
	ColorSpace     string `json:"color_space"`
	ColorRange     string `json:"color_range"`
	Channels       int    `json:"channels"`
	ChannelLayout  string `json:"channel_layout"`
	SampleRate     string `json:"sample_rate"`
	Disposition    struct {
		Default         int `json:"default"`
		AttachedPic     int `json:"attached_pic"`
		TimedThumbnails int `json:"timed_thumbnails"`
	} `json:"disposition"`
	Tags         map[string]string        `json:"tags"`
	SideDataList []map[string]interface{} `json:"side_data_list"`
}

type mediaProbe struct {
	inspection    inspection
	document      probeDocument
	video         probeStream
	audio         *probeStream
	rotation      string
	pts           []int64
	durationTicks int64
	displayWidth  *big.Rat
	displayHeight *big.Rat
}

func (e *engine) inspect(path, mime string, expectedVideoStreamIndex *int, limit limits) (inspection, *mediaProbe, error) {
	if !oneOf(mime, "video/mp4", "video/quicktime") {
		return inspection{}, nil, fail("unsupported_input")
	}
	stdout, _, err := e.run.run(e.ffprobe, []string{"-v", "error", "-threads", "1", "-show_format", "-show_streams", "-show_entries", "stream:format", "-of", "json", path}, 1<<20)
	if err != nil {
		return inspection{}, nil, fail("decode_failed")
	}
	var document probeDocument
	// ffprobe adds version-dependent stream fields, so decode permissively after
	// bounding the complete metadata document.
	if json.Unmarshal(stdout, &document) != nil || len(document.Streams) == 0 {
		return inspection{}, nil, fail("decode_failed")
	}
	if !strings.Contains(document.Format.FormatName, "mov") && !strings.Contains(document.Format.FormatName, "mp4") {
		return inspection{}, nil, fail("unsupported_input")
	}
	if len(document.Streams) > limit.Streams {
		return inspection{}, nil, fail("resource_limit")
	}
	var videoIndex int
	var ok bool
	if expectedVideoStreamIndex == nil {
		videoIndex, ok = selectStream(document.Streams, "video")
	} else {
		videoIndex, ok = *expectedVideoStreamIndex, validVideoStream(document.Streams, *expectedVideoStreamIndex)
	}
	if !ok {
		return inspection{}, nil, fail("unsupported_input")
	}
	audioIndex, audioPresent := selectStream(document.Streams, "audio")
	video, ok := streamByIndex(document.Streams, videoIndex)
	if !ok || video.CodecName == "" || video.Width <= 0 || video.Height <= 0 {
		return inspection{}, nil, fail("unsupported_input")
	}
	if video.Width > limit.Dimension || video.Height > limit.Dimension || !productWithin(uint64(video.Width), uint64(video.Height), limit.Pixels) {
		return inspection{}, nil, fail("resource_limit")
	}
	sar, err := parseRationalDefault(video.SampleAspect, rational{1, 1})
	if err != nil || sar.Numerator <= 0 {
		return inspection{}, nil, fail("unsupported_input")
	}
	if !rationalAxisWithin(video.Width, sar.Numerator, sar.Denominator, limit.Dimension) {
		return inspection{}, nil, fail("resource_limit")
	}
	rotation, rotationSource, err := streamRotation(video)
	if err != nil {
		return inspection{}, nil, err
	}
	displayWidthExact := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(video.Width)), new(big.Rat).SetFrac(big.NewInt(sar.Numerator), big.NewInt(sar.Denominator)))
	displayHeightExact := new(big.Rat).SetInt64(int64(video.Height))
	if rotation == 90 || rotation == 270 {
		displayWidthExact, displayHeightExact = displayHeightExact, displayWidthExact
	}
	displayWidth, err := floorRationalAxis(displayWidthExact)
	if err != nil {
		return inspection{}, nil, fail("unsupported_input")
	}
	displayHeight, err := floorRationalAxis(displayHeightExact)
	if err != nil {
		return inspection{}, nil, fail("unsupported_input")
	}
	if displayWidth > limit.Dimension || displayHeight > limit.Dimension {
		return inspection{}, nil, fail("resource_limit")
	}
	timeBase, err := parseRational(video.TimeBase)
	if err != nil || timeBase.Numerator <= 0 {
		return inspection{}, nil, fail("unsupported_input")
	}
	facts, err := e.frameFacts(path, videoIndex, video.Width, video.Height, timeBase, video.DurationTicks, limit)
	if err != nil {
		return inspection{}, nil, err
	}
	primaries, transfer, matrix, colorRange, hdr, peak, mastering, maxCLL, err := classifyColor(video, facts.frameHDR)
	if err != nil {
		return inspection{}, nil, err
	}
	i := inspection{
		TotalStreams: len(document.Streams), VideoStreamIndex: videoIndex, AudioPresent: audioPresent, AudioStreamIndex: -1,
		VideoCodec: video.CodecName, CodedWidth: video.Width, CodedHeight: video.Height, DisplayWidth: displayWidth, DisplayHeight: displayHeight,
		SampleAspectRatio: sar, RotationDegrees: rotation, FrameCount: len(facts.pts), DurationUS: facts.durationUS,
		EffectiveFPS: facts.fps, TimeBase: timeBase, FirstPTS: facts.pts[0], LastPTS: facts.pts[len(facts.pts)-1], PTSDeltaSHA256: hashDeltas(facts.pts),
		ColorPrimaries: primaries, ColorTransfer: transfer, ColorMatrix: matrix, ColorRange: colorRange, HDR: hdr, SourcePeakNits: peak, MasteringMaxNits: mastering, MaxCLLNits: maxCLL,
		AudioCodec: "none", AudioChannelLayout: "none", CumulativeDecodedPixels: facts.decodedPixels,
	}
	var selectedAudio *probeStream
	if audioPresent {
		audio, _ := streamByIndex(document.Streams, audioIndex)
		rate, parseErr := decimalInt(audio.SampleRate)
		if parseErr != nil || audio.CodecName == "" || audio.Channels <= 0 || audio.Channels > limit.AudioChannels || rate <= 0 || rate > limit.AudioRate || !recognizedLayout(audio.ChannelLayout, audio.Channels) {
			return inspection{}, nil, fail("unsupported_input")
		}
		i.AudioStreamIndex, i.AudioCodec, i.AudioChannels, i.AudioChannelLayout, i.AudioSampleRate = audioIndex, audio.CodecName, audio.Channels, audio.ChannelLayout, rate
		if _, _, decodeErr := e.run.run(e.ffmpeg, []string{"-v", "error", "-xerror", "-nostdin", "-threads", "1", "-i", path, "-map", "0:" + strconv.Itoa(audioIndex), "-vn", "-threads:a", "1", "-f", "null", "-"}, 1<<20); decodeErr != nil {
			return inspection{}, nil, fail("unsupported_input")
		}
		selectedAudio = &audio
	}
	return i, &mediaProbe{inspection: i, document: document, video: video, audio: selectedAudio, rotation: rotationSource, pts: facts.pts, durationTicks: facts.durationTicks, displayWidth: displayWidthExact, displayHeight: displayHeightExact}, nil
}

type hdrEvidence struct {
	mastering int
	cll       int
	present   bool
	invalid   bool
}

type frameAccounting struct {
	pts           []int64
	durationUS    int64
	durationTicks int64
	fps           rational
	decodedPixels uint64
	frameHDR      hdrEvidence
}

func (e *engine) frameFacts(path string, streamIndex, width, height int, timeBase rational, streamDurationTicks int64, limit limits) (frameAccounting, error) {
	args := []string{"-v", "error", "-err_detect", "explode", "-threads", "1", "-select_streams", strconv.Itoa(streamIndex), "-show_frames", "-show_entries", "frame=media_type,stream_index,pts,duration,pkt_duration,width,height:frame_side_data=side_data_type,max_luminance,max_content", "-of", "json", path}
	var result frameAccounting
	var lastDuration int64
	err := e.run.stream(e.ffprobe, args, func(reader io.Reader) error {
		decoder := json.NewDecoder(bufio.NewReader(reader))
		return decodeFrameDocument(decoder, func(frame probeFrame) error {
			if frame.MediaType != "video" {
				return nil
			}
			frameStream, err := parseJSONInt(frame.StreamIndex)
			if err != nil || frameStream != int64(streamIndex) {
				return fail("unsupported_input")
			}
			pts, err := parseJSONInt(frame.PTS)
			if err != nil || len(result.pts) > 0 && pts <= result.pts[len(result.pts)-1] {
				return fail("unsupported_input")
			}
			if len(result.pts) > 0 {
				delta, overflow := subtractInt64(pts, result.pts[len(result.pts)-1])
				if overflow {
					return fail("resource_limit")
				}
				if delta != lastDuration {
					return fail("unsupported_input")
				}
			}
			frameWidth, errW := parseJSONInt(frame.Width)
			frameHeight, errH := parseJSONInt(frame.Height)
			if errW != nil || errH != nil || frameWidth != int64(width) || frameHeight != int64(height) {
				return fail("unsupported_input")
			}
			durationValue := frame.Duration
			if len(durationValue) == 0 {
				durationValue = frame.PacketDuration
			}
			duration, durationErr := parseJSONInt(durationValue)
			if durationErr != nil || duration <= 0 {
				return fail("unsupported_input")
			}
			frameHDR, hdrErr := parseHDREvidence(frame.SideDataList)
			if hdrErr != nil {
				result.frameHDR.invalid = true
			} else if len(result.pts) == 0 {
				result.frameHDR = frameHDR
			} else {
				if frameHDR.present != result.frameHDR.present || frameHDR.present && (frameHDR.mastering != result.frameHDR.mastering || frameHDR.cll != result.frameHDR.cll) {
					result.frameHDR.invalid = true
				}
			}
			if len(result.pts) == limit.Frames {
				return fail("resource_limit")
			}
			pixels := uint64(width) * uint64(height)
			if !addWithin(result.decodedPixels, pixels, limit.DecodedPixels) {
				return fail("resource_limit")
			}
			result.decodedPixels += pixels
			result.pts = append(result.pts, pts)
			lastDuration = duration
			return nil
		})
	})
	if err != nil {
		var protocolErr helperError
		if errors.As(err, &protocolErr) {
			return frameAccounting{}, err
		}
		return frameAccounting{}, fail("unsupported_input")
	}
	// ffprobe reports frame facts but does not implement ffmpeg's -xerror and
	// can exit successfully after concealed/recoverable decoder errors even with
	// err_detect=explode. Require a separate full selected-stream decode whose
	// process status is fatal on every logged decode error before trusting facts.
	if _, _, err := e.run.run(e.ffmpeg, []string{"-v", "error", "-xerror", "-nostdin", "-threads", "1", "-i", path,
		"-map", "0:" + strconv.Itoa(streamIndex), "-an", "-sn", "-dn", "-threads:v", "1", "-f", "null", "-"}, 1<<20); err != nil {
		return frameAccounting{}, fail("unsupported_input")
	}
	if len(result.pts) == 0 || streamDurationTicks < 0 {
		return frameAccounting{}, fail("unsupported_input")
	}
	span, overflow := subtractInt64(result.pts[len(result.pts)-1], result.pts[0])
	if overflow {
		return frameAccounting{}, fail("resource_limit")
	}
	durationTicks, overflow := addInt64(span, lastDuration)
	if overflow {
		return frameAccounting{}, fail("resource_limit")
	}
	if durationTicks <= span {
		return frameAccounting{}, fail("unsupported_input")
	}
	if streamDurationTicks > 0 && streamDurationTicks != durationTicks {
		return frameAccounting{}, fail("unsupported_input")
	}
	durationUS, within, ok := durationMicroseconds(durationTicks, timeBase, limit.DurationUS)
	if !ok {
		return frameAccounting{}, fail("resource_limit")
	}
	if !within {
		return frameAccounting{}, fail("resource_limit")
	}
	result.durationUS = durationUS
	result.durationTicks = durationTicks
	result.fps, ok = effectiveFrameRate(len(result.pts), durationTicks, timeBase)
	if !ok {
		return frameAccounting{}, fail("resource_limit")
	}
	if fractionGreater(result.fps.Numerator, result.fps.Denominator, limit.FPSNum, limit.FPSDen) {
		return frameAccounting{}, fail("resource_limit")
	}
	return result, nil
}

type probeFrame struct {
	MediaType      string                   `json:"media_type"`
	StreamIndex    json.RawMessage          `json:"stream_index"`
	PTS            json.RawMessage          `json:"pts"`
	Duration       json.RawMessage          `json:"duration"`
	PacketDuration json.RawMessage          `json:"pkt_duration"`
	Width          json.RawMessage          `json:"width"`
	Height         json.RawMessage          `json:"height"`
	SideDataList   []map[string]interface{} `json:"side_data_list"`
}

func decodeFrameDocument(decoder *json.Decoder, consume func(probeFrame) error) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fail("unsupported_input")
	}
	foundFrames := false
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			return fail("unsupported_input")
		}
		if name != "frames" {
			var ignored json.RawMessage
			if decoder.Decode(&ignored) != nil {
				return fail("unsupported_input")
			}
			continue
		}
		if foundFrames {
			return fail("unsupported_input")
		}
		foundFrames = true
		token, err = decoder.Token()
		if err != nil || token != json.Delim('[') {
			return fail("unsupported_input")
		}
		for decoder.More() {
			var frame probeFrame
			if decoder.Decode(&frame) != nil {
				return fail("unsupported_input")
			}
			if err := consume(frame); err != nil {
				return err
			}
		}
		if token, err = decoder.Token(); err != nil || token != json.Delim(']') {
			return fail("unsupported_input")
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || !foundFrames {
		return fail("unsupported_input")
	}
	if _, err = decoder.Token(); err != io.EOF {
		return fail("unsupported_input")
	}
	return nil
}

func parseJSONInt(value json.RawMessage) (int64, error) {
	if len(value) == 0 {
		return 0, errors.New("missing integer")
	}
	text := string(value)
	if value[0] == '"' {
		decoded, err := strconv.Unquote(text)
		if err != nil {
			return 0, err
		}
		text = decoded
	}
	return strconv.ParseInt(text, 10, 64)
}

func selectStream(streams []probeStream, kind string) (int, bool) {
	selected, found, selectedDefault := 0, false, false
	for _, stream := range streams {
		if stream.Index < 0 || stream.CodecType != kind || kind == "video" && auxiliaryVideoStream(stream) {
			continue
		}
		isDefault := stream.Disposition.Default != 0
		if !found || isDefault && !selectedDefault || isDefault == selectedDefault && stream.Index < selected {
			selected, found, selectedDefault = stream.Index, true, isDefault
		}
	}
	return selected, found
}

func validVideoStream(streams []probeStream, index int) bool {
	stream, found := streamByIndex(streams, index)
	return found && stream.Index >= 0 && stream.CodecType == "video" && !auxiliaryVideoStream(stream)
}

func auxiliaryVideoStream(stream probeStream) bool {
	return stream.Disposition.AttachedPic != 0 || stream.Disposition.TimedThumbnails != 0
}

func streamByIndex(streams []probeStream, index int) (probeStream, bool) {
	for _, stream := range streams {
		if stream.Index == index {
			return stream, true
		}
	}
	return probeStream{}, false
}

func streamRotation(stream probeStream) (int, string, error) {
	matrix, matrixPresent := 0, false
	for _, side := range stream.SideDataList {
		if value, ok := side["rotation"]; ok {
			if matrixPresent {
				return 0, "", fail("unsupported_input")
			}
			n, err := interfaceInt(value)
			if err != nil {
				return 0, "", fail("unsupported_input")
			}
			var valid bool
			matrix, valid = normalizeMatrixRotation(n)
			if !valid {
				return 0, "", fail("unsupported_input")
			}
			matrixPresent = true
		}
	}
	tag, tagPresent := 0, false
	if value, ok := stream.Tags["rotate"]; ok {
		n, err := decimalInt(value)
		if err != nil {
			return 0, "", fail("unsupported_input")
		}
		var valid bool
		tag, valid = exactRotation(n)
		if !valid {
			return 0, "", fail("unsupported_input")
		}
		tagPresent = true
	}
	if matrixPresent && tagPresent && matrix != tag {
		return 0, "", fail("unsupported_input")
	}
	if matrixPresent {
		return matrix, map[bool]string{true: "display-matrix+rotate", false: "display-matrix"}[tagPresent], nil
	}
	if tagPresent {
		return tag, "rotate-tag", nil
	}
	return 0, "none", nil
}

func normalizeMatrixRotation(value int) (int, bool) {
	if value < -270 || value > 270 {
		return 0, false
	}
	value %= 360
	if value < 0 {
		value += 360
	}
	return value, value == 0 || value == 90 || value == 180 || value == 270
}

func exactRotation(value int) (int, bool) {
	return value, value == 0 || value == 90 || value == 180 || value == 270
}

func classifyColor(stream probeStream, frameHDR hdrEvidence) (string, string, string, string, bool, int, int, int, error) {
	primaries := stream.ColorPrimaries
	transfer := stream.ColorTransfer
	matrix := stream.ColorSpace
	rangeName := map[string]string{"tv": "limited", "mpeg": "limited", "pc": "full", "jpeg": "full"}[stream.ColorRange]
	if rangeName == "" {
		return "", "", "", "", false, 0, 0, 0, fail("unsupported_input")
	}
	if transfer == "smpte2084" || transfer == "arib-std-b67" {
		if primaries != "bt2020" || !oneOf(matrix, "bt2020nc", "bt2020c") {
			return "", "", "", "", false, 0, 0, 0, fail("unsupported_input")
		}
		if transfer == "arib-std-b67" {
			if frameHDR.invalid {
				return "", "", "", "", false, 0, 0, 0, fail("unsupported_input")
			}
			return primaries, transfer, matrix, rangeName, true, 1000, 0, 0, nil
		}
		streamHDR, evidenceErr := parseHDREvidence(stream.SideDataList)
		if evidenceErr != nil || frameHDR.invalid || !frameHDR.present || streamHDR.present && (streamHDR.mastering != frameHDR.mastering || streamHDR.cll != frameHDR.cll) {
			return "", "", "", "", false, 0, 0, 0, fail("unsupported_input")
		}
		evidence := streamHDR
		if !evidence.present {
			evidence = frameHDR
		}
		return primaries, transfer, matrix, rangeName, true, evidence.mastering, evidence.mastering, evidence.cll, nil
	}
	bt709 := primaries == "bt709" && matrix == "bt709"
	bt2020 := primaries == "bt2020" && oneOf(matrix, "bt2020nc", "bt2020c")
	if transfer != "bt709" || !bt709 && !bt2020 {
		return "", "", "", "", false, 0, 0, 0, fail("unsupported_input")
	}
	return primaries, transfer, matrix, rangeName, false, 0, 0, 0, nil
}

func parseHDREvidence(sideData []map[string]interface{}) (hdrEvidence, error) {
	mastering, cll, masteringCount, cllCount := hdrFacts(sideData)
	if masteringCount == 0 && cllCount == 0 {
		return hdrEvidence{}, nil
	}
	if masteringCount != 1 || cllCount != 1 || mastering <= 0 || cll <= 0 || cll > mastering {
		return hdrEvidence{}, errors.New("invalid HDR evidence")
	}
	return hdrEvidence{mastering: mastering, cll: cll, present: true}, nil
}

func hdrFacts(sideData []map[string]interface{}) (mastering, cll, masteringCount, cllCount int) {
	for _, side := range sideData {
		if value, ok := side["max_luminance"]; ok {
			masteringCount++
			parsed, parseErr := parseNits(value)
			if parseErr != nil || masteringCount != 1 {
				return 0, 0, masteringCount, cllCount
			}
			mastering = parsed
		}
		if value, ok := side["max_content"]; ok {
			cllCount++
			parsed, parseErr := interfaceInt(value)
			if parseErr != nil || cllCount != 1 {
				return 0, 0, masteringCount, cllCount
			}
			cll = parsed
		}
	}
	return mastering, cll, masteringCount, cllCount
}

func parseNits(value interface{}) (int, error) {
	s := fmt.Sprint(value)
	r, err := parseRational(s)
	if err == nil && r.Numerator > 0 {
		return int(r.Numerator / r.Denominator), nil
	}
	n, intErr := decimalInt(s)
	if intErr != nil || n <= 0 {
		return 0, errors.New("invalid luminance")
	}
	return n, nil
}

func interfaceInt(value interface{}) (int, error) {
	switch n := value.(type) {
	case float64:
		if n != math.Trunc(n) || n < math.MinInt32 || n > math.MaxInt32 {
			return 0, errors.New("not integer")
		}
		return int(n), nil
	case string:
		return decimalInt(n)
	default:
		return 0, errors.New("not integer")
	}
}

func recognizedLayout(layout string, channels int) bool {
	allowed := map[string]int{"mono": 1, "stereo": 2, "2.1": 3, "3.0": 3, "4.0": 4, "quad": 4, "5.0": 5, "5.1": 6, "6.1": 7, "7.1": 8}
	return allowed[layout] == channels
}

func parseRational(value string) (rational, error) {
	left, right, ok := strings.Cut(value, "/")
	if !ok {
		left, right, ok = strings.Cut(value, ":")
	}
	if !ok {
		return rational{}, errors.New("invalid rational")
	}
	n, errN := strconv.ParseInt(left, 10, 64)
	d, errD := strconv.ParseInt(right, 10, 64)
	if errN != nil || errD != nil || d <= 0 || n < 0 {
		return rational{}, errors.New("invalid rational")
	}
	return reduce(n, d), nil
}

func parseRationalDefault(value string, fallback rational) (rational, error) {
	if value == "" || value == "N/A" {
		return fallback, nil
	}
	return parseRational(value)
}

func floorRationalAxis(value *big.Rat) (int, error) {
	if value == nil || value.Sign() <= 0 {
		return 0, errors.New("invalid display axis")
	}
	floor := new(big.Int).Quo(value.Num(), value.Denom())
	if !floor.IsInt64() || floor.Sign() <= 0 || floor.Int64() > maxDimension {
		return 0, errors.New("invalid display axis")
	}
	return int(floor.Int64()), nil
}

func rationalAxisWithin(axis int, n, d int64, limit int) bool {
	if axis <= 0 || n <= 0 || d <= 0 || limit <= 0 {
		return false
	}
	left := new(big.Int).Mul(big.NewInt(int64(axis)), big.NewInt(n))
	right := new(big.Int).Mul(big.NewInt(int64(limit)), big.NewInt(d))
	return left.Cmp(right) <= 0
}

func hashDeltas(pts []int64) string {
	h := sha256.New()
	var encoded [8]byte
	for i := 1; i < len(pts); i++ {
		binary.BigEndian.PutUint64(encoded[:], uint64(pts[i]-pts[i-1]))
		_, _ = h.Write(encoded[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func reduce(n, d int64) rational {
	if n == 0 {
		return rational{0, 1}
	}
	g := gcd(n, d)
	return rational{n / g, d / g}
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

func rationalScale(value, n, d, scale int64) (int64, bool) {
	if value < 0 || n <= 0 || d <= 0 || scale <= 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(uint64(value), uint64(n))
	if hi != 0 {
		return 0, false
	}
	hi, lo = bits.Mul64(lo, uint64(scale))
	if hi != 0 || lo > math.MaxInt64 {
		return 0, false
	}
	return int64(lo / uint64(d)), true
}

func durationMicroseconds(ticks int64, timeBase rational, limitUS int64) (durationUS int64, within, ok bool) {
	if ticks <= 0 || timeBase.Numerator <= 0 || timeBase.Denominator <= 0 || limitUS <= 0 {
		return 0, false, false
	}
	numerator := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(timeBase.Numerator))
	numerator.Mul(numerator, big.NewInt(1_000_000))
	limit := new(big.Int).Mul(big.NewInt(limitUS), big.NewInt(timeBase.Denominator))
	within = numerator.Cmp(limit) <= 0
	quotient, remainder := new(big.Int).QuoRem(numerator, big.NewInt(timeBase.Denominator), new(big.Int))
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() || quotient.Sign() <= 0 {
		return 0, within, false
	}
	return quotient.Int64(), within, true
}

func effectiveFrameRate(frames int, durationTicks int64, timeBase rational) (rational, bool) {
	if frames <= 0 || durationTicks <= 0 || timeBase.Numerator <= 0 || timeBase.Denominator <= 0 {
		return rational{}, false
	}
	numerator := new(big.Int).Mul(big.NewInt(int64(frames)), big.NewInt(timeBase.Denominator))
	denominator := new(big.Int).Mul(big.NewInt(durationTicks), big.NewInt(timeBase.Numerator))
	common := new(big.Int).GCD(nil, nil, numerator, denominator)
	numerator.Quo(numerator, common)
	denominator.Quo(denominator, common)
	if !numerator.IsInt64() || !denominator.IsInt64() {
		return rational{}, false
	}
	return rational{numerator.Int64(), denominator.Int64()}, true
}

func fractionGreater(an, ad, bn, bd int64) bool {
	if an < 0 || ad <= 0 || bn < 0 || bd <= 0 {
		return true
	}
	aHi, aLo := bits.Mul64(uint64(an), uint64(bd))
	bHi, bLo := bits.Mul64(uint64(bn), uint64(ad))
	return aHi > bHi || aHi == bHi && aLo > bLo
}

func productWithin(a, b, limit uint64) bool { return a != 0 && b != 0 && a <= limit/b }
func addWithin(a, b, limit uint64) bool     { return a <= limit && b <= limit-a }

func addInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, true
	}
	return a + b, false
}

func subtractInt64(a, b int64) (int64, bool) {
	if b > 0 && a < math.MinInt64+b || b < 0 && a > math.MaxInt64+b {
		return 0, true
	}
	return a - b, false
}
