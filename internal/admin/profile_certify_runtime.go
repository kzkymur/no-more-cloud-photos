package admin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/animationprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/mediaformat"
	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/stillprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/videoprocessor"
)

// productionCertifyRuntime drives the SAME processors, policies, and prlimit
// resource envelope the core Worker configures, plus an independent ffprobe.
type productionCertifyRuntime struct {
	still     *stillprocessor.Processor
	animation *animationprocessor.Processor
	video     *videoprocessor.Processor
	probe     *processrunner.Runner
	ffprobe   string
}

const (
	certifyProbeTimeout      = 2 * time.Minute
	certifyProbeAddressSpace = uint64(2 << 30)
	certifyProbeOutputBytes  = int64(1 << 20)
)

func newProductionCertifyRuntime(ws *certifyWorkspace) (certifyRuntime, error) {
	prlimit, icc, digest := ws.helperPath("prlimit"), ws.helperPath("srgb_icc"), ws.helpers["srgb_icc"].sha256
	still, err := stillprocessor.New(stillprocessor.Config{Helper: ws.helperPath("still_helper"), Prlimit: prlimit,
		SRGBICC: icc, SRGBICCSHA256: digest, Policy: stillprocessor.DefaultPolicy()})
	if err != nil {
		return nil, errors.Join(ErrValidation, err)
	}
	animation, err := animationprocessor.New(animationprocessor.Config{Helper: ws.helperPath("animation_helper"), Prlimit: prlimit,
		SRGBICC: icc, SRGBICCSHA256: digest, Policy: animationprocessor.DefaultPolicy()})
	if err != nil {
		return nil, errors.Join(ErrValidation, err)
	}
	video, err := videoprocessor.New(videoprocessor.Config{Helper: ws.helperPath("video_helper"), Prlimit: prlimit,
		SRGBICC: icc, SRGBICCSHA256: digest, Policy: videoprocessor.DefaultPolicy()})
	if err != nil {
		return nil, errors.Join(ErrValidation, err)
	}
	probe, err := processrunner.New(prlimit, processrunner.Limits{Timeout: certifyProbeTimeout, AddressSpaceBytes: certifyProbeAddressSpace,
		FileSizeBytes: 1 << 20, OutputBytesPerStream: certifyProbeOutputBytes})
	if err != nil {
		return nil, errors.Join(ErrValidation, err)
	}
	return &productionCertifyRuntime{still: still, animation: animation, video: video, probe: probe, ffprobe: ws.helperPath("ffprobe")}, nil
}

// certifyProcessorError keeps helper/process text out of every result. Input
// and capability refusals are validation failures; timeouts, resource and
// process failures remain operational failures.
func certifyProcessorError(err error) error {
	if err == nil {
		return nil
	}
	for _, target := range []error{
		stillprocessor.ErrInvalid, stillprocessor.ErrUnsupportedInput, stillprocessor.ErrAnimatedInput, stillprocessor.ErrDecode, stillprocessor.ErrCapability,
		animationprocessor.ErrInvalid, animationprocessor.ErrStaticInput, animationprocessor.ErrUnsupportedInput, animationprocessor.ErrDecode, animationprocessor.ErrCapability,
		videoprocessor.ErrInvalid, videoprocessor.ErrUnsupportedInput, videoprocessor.ErrDecode, videoprocessor.ErrCapability,
	} {
		if errors.Is(err, target) {
			return errors.Join(ErrValidation, err)
		}
	}
	return err
}

func (r *productionCertifyRuntime) Identity(ctx context.Context) (map[string]certifyProcessorIdentity, error) {
	still, err := r.still.Capabilities(ctx)
	if err != nil {
		return nil, certifyProcessorError(err)
	}
	animation, err := r.animation.Capabilities(ctx)
	if err != nil {
		return nil, certifyProcessorError(err)
	}
	video, err := r.video.Capabilities(ctx)
	if err != nil {
		return nil, certifyProcessorError(err)
	}
	return map[string]certifyProcessorIdentity{
		"still": {HelperVersion: still.HelperVersion, LibraryVersions: still.LibraryVersions,
			Encoders: []string{still.AVIFEncoder}, DecoderMIMETypes: still.DecoderMIMETypes},
		"animation": {HelperVersion: animation.HelperVersion, LibraryVersions: animation.LibraryVersions, BuildManifest: animation.BuildManifest,
			Encoders: animation.Encoders, DecoderMIMETypes: animation.DecoderMIMETypes},
		"video": {HelperVersion: video.HelperVersion, LibraryVersions: video.LibraryVersions, BuildManifest: video.BuildManifest,
			Encoders: []string{video.VideoEncoder, video.AudioEncoder}, DecoderMIMETypes: video.DecoderMIMETypes},
	}, nil
}

func (r *productionCertifyRuntime) Inspect(ctx context.Context, mimeType string, input *os.File) (certifyInputFacts, error) {
	format, ok := mediaformat.Lookup(mimeType)
	if !ok {
		return certifyInputFacts{}, ErrValidation
	}
	switch format.Family {
	case mediaformat.FamilyStill:
		return certifyInputFacts{Classification: "still", VideoStreamIndex: -1}, nil
	case mediaformat.FamilyProbeAnimation:
		inspection, err := r.animation.Inspect(ctx, animationprocessor.InspectRequest{Input: input, MIMEType: mimeType})
		if err != nil {
			return certifyInputFacts{}, certifyProcessorError(err)
		}
		return certifyInputFacts{Classification: string(inspection.Classification), VideoStreamIndex: -1}, nil
	case mediaformat.FamilyVideo:
		inspection, err := r.video.Inspect(ctx, videoprocessor.InspectRequest{Input: input, MIMEType: mimeType})
		if err != nil {
			return certifyInputFacts{}, certifyProcessorError(err)
		}
		return certifyInputFacts{Classification: "video", VideoStreamIndex: inspection.VideoStreamIndex}, nil
	}
	return certifyInputFacts{}, ErrValidation
}

func (r *productionCertifyRuntime) Transform(ctx context.Context, run certifyRun, input, output *os.File) (certifyTransformFacts, error) {
	mimeType := run.Fixture.mimeType
	switch run.Route {
	case certifyRouteStill:
		result, err := r.still.Transform(ctx, stillprocessor.Request{Input: input, Output: output, MIMEType: mimeType, Recipe: run.Recipe})
		if err != nil {
			return certifyTransformFacts{}, certifyProcessorError(err)
		}
		return certifyTransformFacts{OutputMIME: result.OutputMIME, Width: result.Width, Height: result.Height, Setting: result.Quality,
			BitDepth: result.BitDepth, MaxLongEdge: result.MaxLongEdge, SourceWidth: result.Audit.SourceWidth, SourceHeight: result.Audit.SourceHeight,
			FrameCount: 1, Decoder: result.Audit.Decoder, Encoder: result.Audit.Encoder, ToolVersion: result.Audit.ToolVersion}, nil
	case certifyRouteAnimation:
		result, err := r.animation.Transform(ctx, animationprocessor.Request{Input: input, Output: output, MIMEType: mimeType, Recipe: run.Recipe})
		if err != nil {
			return certifyTransformFacts{}, certifyProcessorError(err)
		}
		frames := 1
		if result.OutputMIME == "image/webp" {
			frames = result.Source.FrameCount
		}
		return certifyTransformFacts{OutputMIME: result.OutputMIME, Width: result.Width, Height: result.Height, Setting: result.Quality,
			BitDepth: result.BitDepth, MaxLongEdge: result.MaxLongEdge, SourceWidth: result.Source.Width, SourceHeight: result.Source.Height,
			FrameCount: frames, Decoder: result.Audit.Decoder, Encoder: result.Audit.Encoder, ToolVersion: result.Audit.ToolVersion}, nil
	case certifyRouteVideo:
		index := run.Facts.VideoStreamIndex
		result, err := r.video.Transform(ctx, videoprocessor.Request{Input: input, Output: output, MIMEType: mimeType, Recipe: run.Recipe,
			GeneratedBytesBefore: 0, ExpectedVideoStreamIndex: &index})
		if err != nil {
			return certifyTransformFacts{}, certifyProcessorError(err)
		}
		setting := result.Quality
		if result.OutputMIME == "video/mp4" {
			setting = result.CRF
		}
		return certifyTransformFacts{OutputMIME: result.OutputMIME, Width: result.Width, Height: result.Height, Setting: setting,
			BitDepth: result.BitDepth, MaxLongEdge: result.MaxLongEdge, SourceWidth: result.Source.DisplayWidth, SourceHeight: result.Source.DisplayHeight,
			FrameCount: 0, AudioPresent: result.AudioPresent, Decoder: result.Audit.VideoDecoder, Encoder: result.Audit.VideoEncoder,
			ToolVersion: result.Audit.ToolVersion}, nil
	}
	return certifyTransformFacts{}, ErrValidation
}

type certifyFFProbeStream struct {
	Index       int    `json:"index"`
	CodecType   string `json:"codec_type"`
	CodecName   string `json:"codec_name"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	PixelFormat string `json:"pix_fmt"`
}

type certifyFFProbeDocument struct {
	Streams []certifyFFProbeStream `json:"streams"`
	Format  struct {
		FormatName string `json:"format_name"`
	} `json:"format"`
}

// parseCertifyFFProbe reduces bounded ffprobe JSON to the compared facts.
func parseCertifyFFProbe(stdout []byte) (certifyProbeFacts, error) {
	var document certifyFFProbeDocument
	if len(stdout) == 0 || json.Unmarshal(stdout, &document) != nil {
		return certifyProbeFacts{}, errCertifyEvidence
	}
	facts := certifyProbeFacts{Container: document.Format.FormatName}
	streams := slices.Clone(document.Streams)
	slices.SortFunc(streams, func(a, b certifyFFProbeStream) int { return a.Index - b.Index })
	for _, stream := range streams {
		switch stream.CodecType {
		case "video":
			if facts.VideoStreams == 0 {
				facts.Codec, facts.Width, facts.Height, facts.PixelFormat = stream.CodecName, stream.Width, stream.Height, stream.PixelFormat
			}
			facts.VideoStreams++
		case "audio":
			if facts.AudioStreams == 0 {
				facts.AudioCodec = stream.CodecName
			}
			facts.AudioStreams++
		default:
			return certifyProbeFacts{}, errCertifyEvidence
		}
	}
	return facts, nil
}

func (r *productionCertifyRuntime) Probe(ctx context.Context, kind profile.OutputKind, output *os.File) (certifyProbeFacts, error) {
	result, err := r.probe.Run(ctx, processrunner.Command{Executable: r.ffprobe, Arguments: []string{
		"-v", "error", "-show_entries", "stream=index,codec_type,codec_name,width,height,pix_fmt:format=format_name",
		"-of", "json", "/proc/self/fd/3",
	}, Files: []*os.File{output}})
	if err != nil {
		return certifyProbeFacts{}, err
	}
	facts, err := parseCertifyFFProbe(result.Stdout)
	if err != nil {
		return certifyProbeFacts{}, err
	}
	if kind == profile.OutputAnimationWebP {
		width, height, frames, ok := certifyAnimatedWebPStructure(output)
		if !ok || width != facts.Width || height != facts.Height {
			return certifyProbeFacts{}, errCertifyEvidence
		}
		facts.FrameCount = frames
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return certifyProbeFacts{}, err
	}
	return facts, nil
}

// certifyAnimatedWebPStructure is a bounded, helper-independent RIFF walk of an
// animated WebP: VP8X with the animation flag, one ANIM, and ANMF frames.
func certifyAnimatedWebPStructure(file io.ReaderAt) (width, height, frames int, ok bool) {
	header := make([]byte, 12)
	if _, err := file.ReadAt(header, 0); err != nil || string(header[0:4]) != "RIFF" || string(header[8:12]) != "WEBP" {
		return 0, 0, 0, false
	}
	end := int64(binary.LittleEndian.Uint32(header[4:8])) + 8
	if end > int64(animationprocessor.MaxGeneratedOutputBytes) || end < 12 {
		return 0, 0, 0, false
	}
	// The RIFF length must be exactly the file length: present, no trailer.
	probe := make([]byte, 1)
	if _, err := file.ReadAt(probe, end-1); err != nil {
		return 0, 0, 0, false
	}
	if n, _ := file.ReadAt(probe, end); n != 0 {
		return 0, 0, 0, false
	}
	var sawVP8X, sawANIM bool
	chunk := make([]byte, 8)
	for offset, count := int64(12), 0; offset < end; count++ {
		if count > 2*animationprocessor.MaxFrames+16 || offset+8 > end {
			return 0, 0, 0, false
		}
		if _, err := file.ReadAt(chunk, offset); err != nil {
			return 0, 0, 0, false
		}
		size := int64(binary.LittleEndian.Uint32(chunk[4:8]))
		payload := offset + 8
		if payload+size > end {
			return 0, 0, 0, false
		}
		switch string(chunk[0:4]) {
		case "VP8X":
			if count != 0 || size < 10 {
				return 0, 0, 0, false
			}
			data := make([]byte, 10)
			if _, err := file.ReadAt(data, payload); err != nil || data[0]&0x02 == 0 {
				return 0, 0, 0, false
			}
			width = 1 + (int(data[4]) | int(data[5])<<8 | int(data[6])<<16)
			height = 1 + (int(data[7]) | int(data[8])<<8 | int(data[9])<<16)
			sawVP8X = true
		case "ANIM":
			if !sawVP8X || sawANIM {
				return 0, 0, 0, false
			}
			sawANIM = true
		case "ANMF":
			if !sawANIM {
				return 0, 0, 0, false
			}
			frames++
		}
		offset = payload + size + size%2
	}
	return width, height, frames, sawVP8X && sawANIM && frames > 0
}
