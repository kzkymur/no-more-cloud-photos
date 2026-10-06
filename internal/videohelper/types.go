//go:build linux

package videohelper

import "time"

const (
	protocolVersion         = 1
	helperVersion           = "nmcp-video-helper/1"
	buildManifest           = "ffmpeg=9.0.2;svt-av1=4.2.0;zimg=3.0.6;libaom=v3.8.2;threads=1"
	toolchainManifestPrefix = "manifest_version=1\nffmpeg_version=9.0.2\nffmpeg_source_sha256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e\nsvt_av1_version=4.2.0\nsvt_av1_source_sha256=c7b13c4a84bd3751aa35fcc72be13e6875467e7c2216879251a486e5b1e4e740\nzimg_version=3.0.6\nzimg_source_sha256=be89390f13a5c9b2388ce0f44a5e89364a20c1c57ce46d382b1fcc3967057577\nlibaom_version=3.8.2\nlibaom_commit=615b5f541e4434aebd993036bc97ebc1a77ebc25\nlibaom_source_sha256=eb0bfa625cd17849be2e17ffd38bf8e1dc67b7c7787e7152250a4075d05f245f\n"
	toolchainManifestSuffix = "configure=shared,no-autodetect,no-network,no-gpl,no-nonfree,libsvtav1,libzimg,libaom,rpath-pinned\n"

	maxStreams       = 32
	maxDimension     = 16_384
	maxFrames        = 3_456_000
	maxAudioChannels = 8
	maxAudioRate     = 192_000
	maxDurationUS    = int64((4 * time.Hour) / time.Microsecond)
	maxPixels        = uint64(268_435_456)
	maxDecodedPixels = uint64(8_000_000_000_000)
	maxVideoBytes    = int64(8 << 30)
	maxThumbBytes    = int64(64 << 20)
	maxGenerated     = int64(8 << 30)
)

var versions = map[string]string{
	"ffmpeg": "9.0.2", "libsvtav1": "4.2.0", "zimg": "3.0.6", "libaom": "v3.8.2",
}

type rational struct {
	Numerator   int64 `json:"numerator"`
	Denominator int64 `json:"denominator"`
}

type inspection struct {
	TotalStreams            int      `json:"total_streams"`
	VideoStreamIndex        int      `json:"video_stream_index"`
	AudioPresent            bool     `json:"audio_present"`
	AudioStreamIndex        int      `json:"audio_stream_index"`
	VideoCodec              string   `json:"video_codec"`
	CodedWidth              int      `json:"coded_width"`
	CodedHeight             int      `json:"coded_height"`
	DisplayWidth            int      `json:"display_width"`
	DisplayHeight           int      `json:"display_height"`
	SampleAspectRatio       rational `json:"sample_aspect_ratio"`
	RotationDegrees         int      `json:"rotation_degrees"`
	FrameCount              int      `json:"frame_count"`
	DurationUS              int64    `json:"duration_us"`
	EffectiveFPS            rational `json:"effective_fps"`
	TimeBase                rational `json:"time_base"`
	FirstPTS                int64    `json:"first_pts"`
	LastPTS                 int64    `json:"last_pts"`
	PTSDeltaSHA256          string   `json:"pts_delta_sha256"`
	ColorPrimaries          string   `json:"color_primaries"`
	ColorTransfer           string   `json:"color_transfer"`
	ColorMatrix             string   `json:"color_matrix"`
	ColorRange              string   `json:"color_range"`
	HDR                     bool     `json:"hdr"`
	SourcePeakNits          int      `json:"source_peak_nits"`
	MasteringMaxNits        int      `json:"mastering_max_nits"`
	MaxCLLNits              int      `json:"max_cll_nits"`
	AudioCodec              string   `json:"audio_codec"`
	AudioChannels           int      `json:"audio_channels"`
	AudioChannelLayout      string   `json:"audio_channel_layout"`
	AudioSampleRate         int      `json:"audio_sample_rate"`
	CumulativeDecodedPixels uint64   `json:"cumulative_decoded_pixels"`
}

type audit struct {
	ToolVersion            string            `json:"tool_version"`
	BuildManifest          string            `json:"build_manifest"`
	LibraryVersions        map[string]string `json:"library_versions"`
	ICCSHA256              string            `json:"icc_sha256"`
	Demuxer                string            `json:"demuxer"`
	VideoDecoder           string            `json:"video_decoder"`
	AudioDecoder           string            `json:"audio_decoder"`
	VideoEncoder           string            `json:"video_encoder"`
	AudioEncoder           string            `json:"audio_encoder"`
	Muxer                  string            `json:"muxer"`
	SelectedVideoStream    int               `json:"selected_video_stream"`
	SelectedAudioStream    int               `json:"selected_audio_stream"`
	StreamSelection        string            `json:"stream_selection"`
	RotationSource         string            `json:"rotation_source"`
	RotationDegreesApplied int               `json:"rotation_degrees_applied"`
	Orientation            string            `json:"orientation"`
	Geometry               string            `json:"geometry"`
	Timing                 string            `json:"timing"`
	VideoFilterGraph       string            `json:"video_filter_graph"`
	AudioFilterGraph       string            `json:"audio_filter_graph"`
	InputColor             string            `json:"input_color"`
	OutputColor            string            `json:"output_color"`
	OutputPrimaries        string            `json:"output_primaries"`
	OutputTransfer         string            `json:"output_transfer"`
	OutputMatrix           string            `json:"output_matrix"`
	OutputRange            string            `json:"output_range"`
	HDRDisposition         string            `json:"hdr_disposition"`
	ToneMap                string            `json:"tone_map"`
	TargetNits             int               `json:"target_nits"`
	SourcePeakNits         int               `json:"source_peak_nits"`
	InputAudioLayout       string            `json:"input_audio_layout"`
	OutputAudioLayout      string            `json:"output_audio_layout"`
	InputAudioSampleRate   int               `json:"input_audio_sample_rate"`
	OutputAudioSampleRate  int               `json:"output_audio_sample_rate"`
	Metadata               string            `json:"metadata"`
}

type limits struct {
	Streams, Dimension, Frames, AudioChannels, AudioRate int
	Pixels, DecodedPixels                                uint64
	DurationUS, FPSNum, FPSDen                           int64
	VideoBytes, ThumbBytes, GeneratedBytes               int64
}

type request struct {
	command, input, output, source, mime, kind, icc string
	maxLongEdge, quality, bitDepth, threads         int
	generatedBytesBefore                            int64
	limits                                          limits
}

type helperError struct{ code string }

func (e helperError) Error() string { return e.code }

func fail(code string) error { return helperError{code: code} }
