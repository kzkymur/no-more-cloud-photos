//go:build linux

// Package videoprocessor implements the descriptor-only video transform
// boundary. This file owns the immutable limits agreed for protocol version 1.
package videoprocessor

import (
	"errors"
	"math/bits"
	"time"
)

const (
	ProtocolVersion = 1

	MaxTimeout                 = 2 * time.Hour
	MaxAddressSpaceBytes       = uint64(4 << 30)
	MaxLogBytesPerStream       = int64(1 << 20)
	MaxVideoOutputBytes        = int64(8 << 30)
	MaxThumbnailOutputBytes    = int64(64 << 20)
	MaxGeneratedOutputBytes    = int64(8 << 30)
	MaxStreams                 = 32
	MaxDimension               = 16_384
	MaxPixelsPerFrame          = uint64(268_435_456)
	MaxDuration                = 4 * time.Hour
	MaxEffectiveFPSNumerator   = int64(240)
	MaxFrames                  = 3_456_000
	MaxCumulativeDecodedPixels = uint64(8_000_000_000_000)
	MaxAudioChannels           = 8
	MaxAudioSampleRate         = 192_000
	RequiredThreads            = 1
	OutputVideoMaxLongEdge     = 1920
	OutputThumbnailMaxLongEdge = 640
	OutputAACBitrateKbps       = 128
	OutputAV1CRF               = 32
	OutputAV1BitDepth          = 10
)

var (
	ErrInvalid          = errors.New("invalid video processor input")
	ErrUnsupportedInput = errors.New("unsupported video input")
	ErrDecode           = errors.New("video input decode failed")
	ErrCapability       = errors.New("video helper capability failure")
	ErrResourcePolicy   = errors.New("video processor resource or policy failure")
	ErrProcess          = errors.New("video helper process failure")
	ErrTimeout          = errors.New("video helper timed out")
	ErrLogOutputLimit   = errors.New("video helper log output limit exceeded")
	errOperationTimeout = errors.New("video transform operation deadline exceeded")
)

// Policy can only reduce the protocol's immutable hard ceilings. A zero value
// selects the corresponding protocol maximum.
type Policy struct {
	Timeout                 time.Duration
	AddressSpaceBytes       uint64
	LogBytesPerStream       int64
	VideoOutputMaxBytes     int64
	ThumbnailOutputMaxBytes int64
	GeneratedOutputMaxBytes int64
	Streams                 int
	Dimension               int
	PixelsPerFrame          uint64
	Duration                time.Duration
	EffectiveFPSNumerator   int64
	EffectiveFPSDenominator int64
	Frames                  int
	CumulativeDecodedPixels uint64
	AudioChannels           int
	AudioSampleRate         int
	Threads                 int
}

func DefaultPolicy() Policy {
	return Policy{
		Timeout: MaxTimeout, AddressSpaceBytes: MaxAddressSpaceBytes,
		LogBytesPerStream:   MaxLogBytesPerStream,
		VideoOutputMaxBytes: MaxVideoOutputBytes, ThumbnailOutputMaxBytes: MaxThumbnailOutputBytes,
		GeneratedOutputMaxBytes: MaxGeneratedOutputBytes, Streams: MaxStreams, Dimension: MaxDimension,
		PixelsPerFrame: MaxPixelsPerFrame, Duration: MaxDuration,
		EffectiveFPSNumerator: MaxEffectiveFPSNumerator, EffectiveFPSDenominator: 1,
		Frames: MaxFrames, CumulativeDecodedPixels: MaxCumulativeDecodedPixels,
		AudioChannels: MaxAudioChannels, AudioSampleRate: MaxAudioSampleRate, Threads: RequiredThreads,
	}
}

func normalizePolicy(input Policy) (Policy, error) {
	defaults := DefaultPolicy()
	if input.Timeout == 0 {
		input.Timeout = defaults.Timeout
	}
	if input.AddressSpaceBytes == 0 {
		input.AddressSpaceBytes = defaults.AddressSpaceBytes
	}
	if input.LogBytesPerStream == 0 {
		input.LogBytesPerStream = defaults.LogBytesPerStream
	}
	if input.VideoOutputMaxBytes == 0 {
		input.VideoOutputMaxBytes = defaults.VideoOutputMaxBytes
	}
	if input.ThumbnailOutputMaxBytes == 0 {
		input.ThumbnailOutputMaxBytes = defaults.ThumbnailOutputMaxBytes
	}
	if input.GeneratedOutputMaxBytes == 0 {
		input.GeneratedOutputMaxBytes = defaults.GeneratedOutputMaxBytes
	}
	if input.Streams == 0 {
		input.Streams = defaults.Streams
	}
	if input.Dimension == 0 {
		input.Dimension = defaults.Dimension
	}
	if input.PixelsPerFrame == 0 {
		input.PixelsPerFrame = defaults.PixelsPerFrame
	}
	if input.Duration == 0 {
		input.Duration = defaults.Duration
	}
	if input.EffectiveFPSNumerator == 0 && input.EffectiveFPSDenominator == 0 {
		input.EffectiveFPSNumerator, input.EffectiveFPSDenominator = defaults.EffectiveFPSNumerator, 1
	}
	if input.Frames == 0 {
		input.Frames = defaults.Frames
	}
	if input.CumulativeDecodedPixels == 0 {
		input.CumulativeDecodedPixels = defaults.CumulativeDecodedPixels
	}
	if input.AudioChannels == 0 {
		input.AudioChannels = defaults.AudioChannels
	}
	if input.AudioSampleRate == 0 {
		input.AudioSampleRate = defaults.AudioSampleRate
	}
	if input.Threads == 0 {
		input.Threads = RequiredThreads
	}
	if input.Timeout <= 0 || input.Timeout > MaxTimeout || input.AddressSpaceBytes == 0 || input.AddressSpaceBytes > MaxAddressSpaceBytes ||
		input.LogBytesPerStream <= 0 || input.LogBytesPerStream > MaxLogBytesPerStream ||
		input.VideoOutputMaxBytes <= 0 || input.VideoOutputMaxBytes > MaxVideoOutputBytes ||
		input.ThumbnailOutputMaxBytes <= 0 || input.ThumbnailOutputMaxBytes > MaxThumbnailOutputBytes ||
		input.GeneratedOutputMaxBytes <= 0 || input.GeneratedOutputMaxBytes > MaxGeneratedOutputBytes ||
		input.VideoOutputMaxBytes > input.GeneratedOutputMaxBytes || input.ThumbnailOutputMaxBytes > input.GeneratedOutputMaxBytes ||
		input.Streams <= 0 || input.Streams > MaxStreams || input.Dimension <= 0 || input.Dimension > MaxDimension ||
		input.PixelsPerFrame == 0 || input.PixelsPerFrame > MaxPixelsPerFrame || input.Duration <= 0 || input.Duration > MaxDuration ||
		input.EffectiveFPSNumerator <= 0 || input.EffectiveFPSDenominator <= 0 ||
		rationalGreater(input.EffectiveFPSNumerator, input.EffectiveFPSDenominator, MaxEffectiveFPSNumerator, 1) ||
		input.Frames <= 0 || input.Frames > MaxFrames || input.CumulativeDecodedPixels == 0 || input.CumulativeDecodedPixels > MaxCumulativeDecodedPixels ||
		input.AudioChannels <= 0 || input.AudioChannels > MaxAudioChannels || input.AudioSampleRate <= 0 || input.AudioSampleRate > MaxAudioSampleRate ||
		input.Threads != RequiredThreads {
		return Policy{}, ErrInvalid
	}
	return input, nil
}

func rationalGreater(an, ad, bn, bd int64) bool {
	if an < 0 || ad <= 0 || bn < 0 || bd <= 0 {
		return true
	}
	aHi, aLo := bits.Mul64(uint64(an), uint64(bd))
	bHi, bLo := bits.Mul64(uint64(bn), uint64(ad))
	return aHi > bHi || aHi == bHi && aLo > bLo
}

type streamCandidate struct {
	Index           int
	Default         bool
	AttachedPicture bool
}

// selectPrimary implements the fixed default-disposition-then-index rule.
func selectPrimary(candidates []streamCandidate, video bool) (int, bool) {
	selected, found := 0, false
	for _, candidate := range candidates {
		if candidate.Index < 0 || video && candidate.AttachedPicture {
			continue
		}
		if !found || candidate.Default && !candidatesDefault(candidates, selected) ||
			candidate.Default == candidatesDefault(candidates, selected) && candidate.Index < selected {
			selected, found = candidate.Index, true
		}
	}
	return selected, found
}

func candidatesDefault(candidates []streamCandidate, index int) bool {
	for _, candidate := range candidates {
		if candidate.Index == index {
			return candidate.Default
		}
	}
	return false
}

func resolveRotation(matrixPresent bool, matrix int, tagPresent bool, tag int) (int, error) {
	valid := func(value int) bool { return value == 0 || value == 90 || value == 180 || value == 270 }
	if matrixPresent && !valid(matrix) || tagPresent && !valid(tag) {
		return 0, ErrUnsupportedInput
	}
	if matrixPresent && tagPresent && matrix != tag {
		return 0, ErrUnsupportedInput
	}
	if matrixPresent {
		return matrix, nil
	}
	if tagPresent {
		return tag, nil
	}
	return 0, nil
}

// fitDimensions uses display-oriented square-pixel dimensions. Video floors
// both axes to positive even values; thumbnails retain odd dimensions.
func fitDimensions(width, height, edge int, even bool) (int, int, error) {
	if width <= 0 || height <= 0 || edge <= 0 || width > MaxDimension || height > MaxDimension {
		return 0, 0, ErrUnsupportedInput
	}
	outputWidth, outputHeight := width, height
	long := max(width, height)
	if long > edge {
		outputWidth = int(uint64(width) * uint64(edge) / uint64(long))
		outputHeight = int(uint64(height) * uint64(edge) / uint64(long))
	}
	if even {
		outputWidth -= outputWidth % 2
		outputHeight -= outputHeight % 2
		if outputWidth < 2 || outputHeight < 2 {
			return 0, 0, ErrUnsupportedInput
		}
	} else if outputWidth < 1 || outputHeight < 1 {
		return 0, 0, ErrUnsupportedInput
	}
	return outputWidth, outputHeight, nil
}

func checkedProductWithin(a, b, limit uint64) bool {
	return a != 0 && b != 0 && a <= limit/b
}

func checkedAddWithin(a, b, limit uint64) bool {
	return a <= limit && b <= limit-a
}
