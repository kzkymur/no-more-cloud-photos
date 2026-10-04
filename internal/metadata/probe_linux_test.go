//go:build linux

package metadata

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRealFFProbeReferenceAndGeneratedFixtures(t *testing.T) {
	if os.Getenv("TEST_METADATA_TOOLS") != "1" {
		t.Skip("TEST_METADATA_TOOLS is not enabled")
	}
	ffprobe := os.Getenv("TEST_FFPROBE_PATH")
	if ffprobe == "" {
		ffprobe = "/usr/bin/ffprobe"
	}
	ffmpeg := os.Getenv("TEST_FFMPEG_PATH")
	if ffmpeg == "" {
		ffmpeg = "/usr/bin/ffmpeg"
	}
	prober := newTestProber(t, "/usr/bin/exiftool", ffprobe, DefaultPolicy())

	t.Run("reference QuickTime", func(t *testing.T) {
		path, err := filepath.Abs(filepath.Join("testdata", "QuickTime.mov"))
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result, err := prober.ProbeFile(context.Background(), path, "Asia/Tokyo")
		if err != nil {
			t.Fatal(err)
		}
		if result.MIMEType != "video/quicktime" || result.Width != 320 || result.Height != 240 || result.DurationMS == nil || *result.DurationMS != 435 || result.Derived.Capture.Source != TakenAtEmbeddedOffset {
			t.Fatalf("result = %#v", result)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(before) != sha256.Sum256(after) {
			t.Fatal("ffprobe modified original bytes")
		}
	})

	t.Run("generated MP4", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fixture.mp4")
		command := exec.Command(ffmpeg,
			"-v", "error", "-y", "-f", "lavfi", "-i", "color=c=black:s=32x18:d=0.2",
			"-c:v", "libx264", "-pix_fmt", "yuv420p", "-metadata", "creation_time=2024-01-02T03:04:05Z", path)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("generate MP4: %v: %s", err, output)
		}
		result, err := prober.ProbeFile(context.Background(), path, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		if result.MIMEType != "video/mp4" || result.Width != 32 || result.Height != 18 || result.DurationMS == nil || *result.DurationMS < 100 {
			t.Fatalf("result = %#v", result)
		}
	})
}

func TestRealExifToolReferenceFixtures(t *testing.T) {
	if os.Getenv("TEST_METADATA_TOOLS") != "1" {
		t.Skip("TEST_METADATA_TOOLS is not enabled")
	}
	exiftool := os.Getenv("TEST_EXIFTOOL_PATH")
	if exiftool == "" {
		exiftool = "/usr/bin/exiftool"
	}
	prober := newTestProber(t, exiftool, "/usr/bin/ffprobe", DefaultPolicy())
	tests := map[string]string{
		"BMP.bmp":      "image/bmp",
		"CanonRaw.cr2": "image/x-canon-cr2", "CanonRaw.cr3": "image/x-canon-cr3",
		"DNG.dng": "image/dng", "FujiFilm.raf": "image/x-fuji-raf",
		"ExifTool.jpg": "image/jpeg", "GIF.gif": "image/gif",
		"Nikon.nef": "image/x-nikon-nef", "Panasonic.rw2": "image/x-panasonic-rw2",
		"PNG.png": "image/png", "RIFF.webp": "image/webp",
	}
	for name, wantMIME := range tests {
		t.Run(name, func(t *testing.T) {
			path, err := filepath.Abs(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			result, err := prober.ProbeFile(context.Background(), path, "Asia/Tokyo")
			if err != nil {
				t.Fatalf("ProbeFile() error = %v", err)
			}
			if result.MIMEType != wantMIME || result.Width < 1 || result.Height < 1 || result.Derived.ProbeTool != "exiftool" {
				t.Fatalf("result = %#v", result)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatal("metadata probe modified original bytes")
			}
		})
	}
}

func TestProberStillNormalizesAllowlistAndCapture(t *testing.T) {
	input := writeMedia(t, "input.jpg", []byte{0xff, 0xd8, 0xff, 0xd9})
	exiftool := writeExecutable(t, "exiftool", `#!/bin/sh
if [ "$1" = "-ver" ]; then printf '13.36\n'; exit 0; fi
cat <<'EOF'
[{"SourceFile":"/secret/input.jpg","ImageWidth":20,"ImageHeight":10,"Make":"Camera","Model":"M","LensMake":"Lens","LensModel":"L","Orientation":6,"ExposureTime":"1/125","FNumber":2.8,"ISO":200,"FocalLength":50,"GPSLatitude":35.0,"GPSLongitude":139.0,"GPSAltitude":12.5,"DateTimeOriginal":"2024:01:02 03:04:05","OffsetTimeOriginal":"+09:00","UnrequestedSecret":"do-not-store"}]
EOF
`)
	ffprobe := writeExecutable(t, "ffprobe", "#!/bin/sh\nprintf 'unused'\n")
	prober := newTestProber(t, exiftool, ffprobe, DefaultPolicy())
	result, err := prober.ProbeFile(context.Background(), input, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if result.MIMEType != "image/jpeg" || result.Width != 20 || result.Height != 10 || result.Derived.ProbeVersion != "13.36" {
		t.Fatalf("result = %#v", result)
	}
	if result.EXIF.Make == nil || *result.EXIF.Make != "Camera" || result.EXIF.DateTimeRaw == nil {
		t.Fatalf("EXIF = %#v", result.EXIF)
	}
	metadataJSON, err := result.SourceMetadataJSON()
	if err != nil {
		t.Fatal(err)
	}
	exifJSON, err := result.EXIFJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"/secret", "SourceFile", "UnrequestedSecret", "do-not-store"} {
		if strings.Contains(string(metadataJSON), forbidden) || strings.Contains(string(exifJSON), forbidden) {
			t.Fatalf("serialized metadata leaked %q", forbidden)
		}
	}
	if result.Derived.Capture.Source != TakenAtEmbeddedOffset || result.Derived.Capture.TakenAt == nil {
		t.Fatalf("capture = %#v", result.Derived.Capture)
	}
}

func TestProberSkipsMalformedOptionalEXIF(t *testing.T) {
	input := writeMedia(t, "input.jpg", []byte{0xff, 0xd8, 0xff, 0xd9})
	exiftool := writeExecutable(t, "exiftool", `#!/bin/sh
if [ "$1" = "-ver" ]; then printf '13.36\n'; exit 0; fi
printf '[{"ImageWidth":2,"ImageHeight":3,"Make":123,"ExposureTime":"not-an-exposure","ISO":"bad","GPSLatitude":"nan","DateTimeOriginal":"bad","OffsetTimeOriginal":"+99:00"}]'
`)
	prober := newTestProber(t, exiftool, writeExecutable(t, "ffprobe", "#!/bin/sh\nexit 1\n"), DefaultPolicy())
	result, err := prober.ProbeFile(context.Background(), input, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if result.EXIF.Make != nil || result.EXIF.ExposureTime != nil || result.EXIF.ISO != nil || result.EXIF.GPSLatitude != nil || result.Derived.Capture.Source != TakenAtUnknown {
		t.Fatalf("optional malformed fields were retained: %#v %#v", result.EXIF, result.Derived.Capture)
	}
}

func TestProbeDecodersRejectInvalidUTF8(t *testing.T) {
	invalid := []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}
	if _, err := decodeExifTool(invalid); !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("decodeExifTool error = %v", err)
	}
	if _, err := decodeFFProbe(invalid); !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("decodeFFProbe error = %v", err)
	}
}

func TestProberMalformedPresentOffsetFallsThroughAndNormalizesDomains(t *testing.T) {
	input := writeMedia(t, "input.jpg", []byte{0xff, 0xd8, 0xff, 0xd9})
	exiftool := writeExecutable(t, "exiftool", `#!/bin/sh
if [ "$1" = "-ver" ]; then printf '13.36\n'; exit 0; fi
printf '[{"ImageWidth":2,"ImageHeight":3,"Orientation":9,"ExposureTime":0.008,"FNumber":-1,"ISO":-2,"FocalLength":0,"GPSLatitude":91,"GPSLongitude":-181,"DateTimeOriginal":"2024:01:02 03:04:05","OffsetTimeOriginal":123,"CreateDate":"2024:01:02 04:05:06","OffsetTimeDigitized":"+09:00"}]'
`)
	prober := newTestProber(t, exiftool, writeExecutable(t, "unused", "#!/bin/sh\nexit 1\n"), DefaultPolicy())
	result, err := prober.ProbeFile(context.Background(), input, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if result.Derived.Capture.Selected == nil || result.Derived.Capture.Selected.Name != "DateTimeDigitized" || result.Derived.Capture.Candidates[0].Discarded != discardInvalidOffset {
		t.Fatalf("capture = %#v", result.Derived.Capture)
	}
	if result.EXIF.ExposureTime == nil || *result.EXIF.ExposureTime != "0.008" {
		t.Fatalf("numeric exposure = %#v", result.EXIF.ExposureTime)
	}
	if result.EXIF.Orientation != nil || result.EXIF.Aperture != nil || result.EXIF.ISO != nil || result.EXIF.FocalLengthMM != nil || result.EXIF.GPSLatitude != nil || result.EXIF.GPSLongitude != nil {
		t.Fatalf("out-of-domain EXIF retained: %#v", result.EXIF)
	}
}

func TestProberUsesPinnedDescriptorAndIsolatedEnvironment(t *testing.T) {
	input := writeMedia(t, "secret-input.jpg", []byte{0xff, 0xd8, 0xff, 0xd9})
	t.Setenv("DATABASE_PASSWORD", "must-not-reach-probe")
	t.Setenv("FFREPORT", "file=must-not-exist")
	t.Setenv("PERL5OPT", "-Mnot_a_real_module")
	t.Setenv("HOME", t.TempDir())
	exiftool := writeExecutable(t, "exiftool", `#!/bin/sh
if [ "$1" = "-ver" ]; then printf '13.36\n'; exit 0; fi
if [ "$1" != "-config" ] || [ "$2" != "" ]; then exit 40; fi
if [ -n "$DATABASE_PASSWORD" ] || [ -n "$FFREPORT" ] || [ -n "$PERL5OPT" ] || [ -n "$HOME" ]; then exit 41; fi
last=""
for arg in "$@"; do last="$arg"; done
if [ "$last" != "/proc/self/fd/3" ] || [ ! -r "$last" ]; then exit 42; fi
printf '[{"ImageWidth":2,"ImageHeight":3}]'
`)
	prober := newTestProber(t, exiftool, writeExecutable(t, "unused", "#!/bin/sh\nexit 1\n"), DefaultPolicy())
	if _, err := prober.ProbeFile(context.Background(), input, "UTC"); err != nil {
		t.Fatal(err)
	}

	symlink := filepath.Join(t.TempDir(), "link.jpg")
	if err := os.Symlink(input, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := prober.ProbeFile(context.Background(), symlink, "UTC"); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("symlink error = %v", err)
	}
	fifo := filepath.Join(t.TempDir(), "fifo.jpg")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := prober.ProbeFile(context.Background(), fifo, "UTC"); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("FIFO error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("FIFO open blocked")
	}
}

func TestPrimaryVideoAndDurationRules(t *testing.T) {
	streams := []ffprobeStream{
		{Index: 1, CodecType: "video", Width: 500, Height: 500},
		{Index: 2, CodecType: "video", Width: 100, Height: 100},
		{Index: 3, CodecType: "video", Width: 0, Height: 100},
	}
	streams[0].Disposition.AttachedPic = 1
	streams[1].Disposition.Default = 1
	selected, ok := primaryVideoStream(streams)
	if !ok || selected.Index != 2 {
		t.Fatalf("selected = %#v, %v", selected, ok)
	}
	for raw, want := range map[string]int64{"0.0005": 1, "1.2344": 1234, "1.2345": 1235} {
		got, ok := durationMilliseconds(raw)
		if !ok || got != want {
			t.Errorf("durationMilliseconds(%q) = %d, %v; want %d", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "0", "-1", "NaN", "9223372036854775.808"} {
		if got, ok := durationMilliseconds(raw); ok {
			t.Errorf("durationMilliseconds(%q) = %d, true", raw, got)
		}
	}
}

func TestProberVideoSelectsDefaultStreamAndContainerFallback(t *testing.T) {
	input := writeMedia(t, "input.mp4", ftypFixture("mp42", "isom"))
	ffprobe := writeExecutable(t, "ffprobe", `#!/bin/sh
if [ "$1" = "-version" ]; then printf 'ffprobe version 7.1\n'; exit 0; fi
cat <<'EOF'
{"streams":[{"index":3,"codec_type":"video","codec_name":"h264","width":640,"height":360,"duration":"1.25","tags":{"creation_time":"bad"},"disposition":{"default":0}},{"index":2,"codec_type":"video","codec_name":"hevc","width":320,"height":240,"duration":"","tags":{},"disposition":{"default":1}}],"format":{"duration":"2.5","tags":{"creation_time":"2024-01-02T03:04:05Z"}}}
EOF
`)
	prober := newTestProber(t, writeExecutable(t, "exiftool", "#!/bin/sh\nexit 1\n"), ffprobe, DefaultPolicy())
	result, err := prober.ProbeFile(context.Background(), input, "Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	if result.Width != 320 || result.Height != 240 || result.DurationMS == nil || *result.DurationMS != 2500 || result.Derived.PrimaryStream == nil || *result.Derived.PrimaryStream != 2 || result.Derived.VideoCodec != "hevc" {
		t.Fatalf("video result = %#v", result)
	}
	if result.Derived.Capture.Selected == nil || result.Derived.Capture.Selected.Name != "container_creation_time" {
		t.Fatalf("capture = %#v", result.Derived.Capture)
	}
}

func TestProberPolicyAndStructuralFailures(t *testing.T) {
	validTool := writeExecutable(t, "exiftool", `#!/bin/sh
if [ "$1" = "-ver" ]; then printf '13.36\n'; else printf '[{"ImageWidth":100001,"ImageHeight":1}]'; fi
`)
	prober := newTestProber(t, validTool, writeExecutable(t, "ffprobe", "#!/bin/sh\nexit 1\n"), DefaultPolicy())
	input := writeMedia(t, "too-wide.jpg", []byte{0xff, 0xd8, 0xff, 0xd9})
	if _, err := prober.ProbeFile(context.Background(), input, "UTC"); !errors.Is(err, ErrPolicyViolation) || !errors.Is(err, ErrMaxDimension) {
		t.Fatalf("dimension error = %v", err)
	}

	malformed := writeExecutable(t, "malformed", "#!/bin/sh\nif [ \"$1\" = \"-ver\" ]; then printf '13.36'; else printf '{'; fi\n")
	prober = newTestProber(t, malformed, writeExecutable(t, "unused", "#!/bin/sh\nexit 1\n"), DefaultPolicy())
	if _, err := prober.ProbeFile(context.Background(), input, "UTC"); !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("malformed JSON error = %v", err)
	}

	unknown := writeMedia(t, "spoofed.jpg", []byte("this is not JPEG"))
	if _, err := prober.ProbeFile(context.Background(), unknown, "UTC"); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("spoofed content error = %v", err)
	}
}

func TestPolicyValidationAndToolPaths(t *testing.T) {
	valid := DefaultPolicy()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*Policy){
		func(p *Policy) { p.Timeout = 0 },
		func(p *Policy) { p.AddressSpaceBytes = 1 },
		func(p *Policy) { p.OutputBytes = 1 },
		func(p *Policy) { p.MaxDimension = 0 },
		func(p *Policy) { p.MaxPixels = 0 },
	}
	for index, mutate := range mutations {
		policy := DefaultPolicy()
		mutate(&policy)
		if err := policy.Validate(); err == nil {
			t.Errorf("invalid policy case %d accepted", index)
		}
	}
	if _, err := NewProber(DefaultPolicy(), ToolPaths{ExifTool: "relative", FFProbe: "/x", Prlimit: "/y"}); err == nil {
		t.Fatal("relative tool path accepted")
	}
}

func newTestProber(t *testing.T, exiftool, ffprobe string, policy Policy) *Prober {
	t.Helper()
	prober, err := NewProber(policy, ToolPaths{ExifTool: exiftool, FFProbe: ffprobe, Prlimit: "/usr/bin/prlimit"})
	if err != nil {
		t.Fatal(err)
	}
	return prober
}

func writeMedia(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbeFileCallerCancellation(t *testing.T) {
	input := writeMedia(t, "input.jpg", []byte{0xff, 0xd8, 0xff, 0xd9})
	tool := writeExecutable(t, "slow", "#!/bin/sh\nsleep 30\n")
	policy := DefaultPolicy()
	policy.Timeout = time.Second
	prober := newTestProber(t, tool, tool, policy)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := prober.ProbeFile(ctx, input, "UTC"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ProbeFile error = %v", err)
	}
}
