//go:build linux

package animationprocessor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

const testICCDigest = "01ac75c95d4774800a94e69c0a5f65b669ba9a62e66bded7421f42c3445bc700"
const versionsJSON = `{"giflib":"5.2.2","libwebp":"1.6.0","libwebp-demux":"1.6.0","libwebp-mux":"1.6.0","libheif":"1.23.5","libaom":"v3.8.2","lcms2":"2.14"}`
const capabilitySuccess = `{"protocol":1,"ok":true,"error_code":"","result":{"helper_version":"animation-helper-1","library_versions":` + versionsJSON + `,"decoder_mime_types":["image/gif","image/webp"],"encoders":["animated-webp","avif"],"icc_sha256":"` + testICCDigest + `","threads":1,"build_manifest":"` + BuildManifest + `"}}`
const animationInspection = `{"classification":"animation","width":3,"height":2,"frame_count":3,"frame_durations_ms":[40,100,250],"duration_ms":390,"total_plays":4,"has_alpha":true,"zero_duration_frame_indices":[1],"decoded_pixels":18}`
const staticInspection = `{"classification":"static","width":3,"height":2,"frame_count":1,"frame_durations_ms":[100],"duration_ms":100,"total_plays":1,"has_alpha":false,"zero_duration_frame_indices":[0],"decoded_pixels":6}`
const inspectSuccess = `{"protocol":1,"ok":true,"error_code":"","result":` + animationInspection + `}`
const transformSuccess = `{"protocol":1,"ok":true,"error_code":"","result":{"output_mime":"image/webp","width":3,"height":2,"quality":80,"bit_depth":8,"max_long_edge":1920,"threads":1,"source":` + animationInspection + `,"audit":{"decoder":"giflib-gif","encoder":"libwebp","tool_version":"animation-helper-1","library_versions":` + versionsJSON + `,"icc_sha256":"` + testICCDigest + `","composition":"composited-rgba","timing_normalization":"zero-duration-to-100ms","loop_normalization":"total-play-count","input_color":"assumed-srgb","output_color":"srgb","alpha":"preserved","metadata":"strip-after-normalization-keep-color-tags"}}}`
const webPVerificationSuccess = `{"protocol":1,"ok":true,"error_code":"","result":{"width":3,"height":2,"frame_count":3,"frame_durations_ms":[40,100,250],"duration_ms":390,"total_plays":4,"bit_depth":8}}`

var fakeWebP = string([]byte{
	'R', 'I', 'F', 'F', 186, 0, 0, 0, 'W', 'E', 'B', 'P',
	'V', 'P', '8', 'X', 10, 0, 0, 0, 2, 0, 0, 0, 2, 0, 0, 1, 0, 0,
	'A', 'N', 'I', 'M', 6, 0, 0, 0, 0, 0, 0, 0, 4, 0,
	'A', 'N', 'M', 'F', 42, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 40, 0, 0, 0,
	'V', 'P', '8', 'L', 17, 0, 0, 0, 47, 0, 0, 0, 0, 7, 208, 255, 254, 247, 191, 255, 129, 136, 232, 127, 0, 0,
	'A', 'N', 'M', 'F', 42, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 100, 0, 0, 0,
	'V', 'P', '8', 'L', 17, 0, 0, 0, 47, 0, 0, 0, 0, 7, 208, 255, 254, 247, 191, 255, 129, 136, 232, 127, 0, 0,
	'A', 'N', 'M', 'F', 42, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 250, 0, 0, 0,
	'V', 'P', '8', 'L', 17, 0, 0, 0, 47, 0, 0, 0, 0, 7, 208, 255, 254, 247, 191, 255, 129, 136, 232, 127, 0, 0,
})

var fakeAVIF = string([]byte{
	0, 0, 0, 20, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f', 0, 0, 0, 0, 'a', 'v', 'i', 'f',
	0, 0, 0, 112, 'm', 'e', 't', 'a', 0, 0, 0, 0,
	0, 0, 0, 32, 'h', 'd', 'l', 'r', 0, 0, 0, 0, 0, 0, 0, 0, 'p', 'i', 'c', 't', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 14, 'p', 'i', 't', 'm', 0, 0, 0, 0, 0, 0,
	0, 0, 0, 16, 'i', 'l', 'o', 'c', 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 14, 'i', 'i', 'n', 'f', 0, 0, 0, 0, 0, 0,
	0, 0, 0, 24, 'i', 'p', 'r', 'p', 0, 0, 0, 8, 'i', 'p', 'c', 'o', 0, 0, 0, 8, 'i', 'p', 'm', 'a',
	0, 0, 0, 9, 'm', 'd', 'a', 't', 0,
})

var contradictoryWebP = string([]byte{
	'R', 'I', 'F', 'F', 86, 0, 0, 0, 'W', 'E', 'B', 'P',
	'V', 'P', '8', 'X', 10, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	'A', 'N', 'I', 'M', 6, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	'A', 'N', 'M', 'F', 42, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 100, 0, 0, 0,
	'V', 'P', '8', 'L', 17, 0, 0, 0, 47, 0, 0, 0, 0, 7, 208, 255, 254, 247, 191, 255, 129, 136, 232, 127, 0, 0,
})

func TestCapabilities(t *testing.T) {
	helper := writeExecutable(t, "capabilities", "#!/bin/sh\n"+
		"[ \"$#\" -eq 7 ] && [ \"$1\" = capabilities ] && [ \"$2\" = --protocol ] && [ \"$3\" = 1 ] && [ \"$4\" = --threads ] && [ \"$5\" = 1 ] && [ \"$6\" = --srgb-icc ] && [ \"$7\" = /proc/self/fd/3 ] || exit 7\n"+
		"printf '%s' '"+capabilitySuccess+"'\n")
	got, err := newProcessor(t, helper, Policy{}).Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.ProtocolVersion != 1 || got.HelperVersion != "animation-helper-1" || got.LibraryVersions["libwebp"] != "1.6.0" || len(got.Encoders) != 2 {
		t.Fatalf("Capabilities() = %+v", got)
	}
}

func TestCapabilitiesRejectSubstitutedBuild(t *testing.T) {
	for name, document := range map[string]string{
		"manifest":    strings.Replace(capabilitySuccess, BuildManifest, "untrusted", 1),
		"version":     strings.Replace(capabilitySuccess, `"libwebp":"1.6.0"`, `"libwebp":"1.5.0"`, 1),
		"missing mux": strings.Replace(capabilitySuccess, `,"libwebp-mux":"1.6.0"`, ``, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newProcessor(t, jsonHelper(t, document, ""), Policy{}).Capabilities(context.Background())
			if !errors.Is(err, ErrCapability) {
				t.Fatalf("Capabilities() error = %v", err)
			}
		})
	}
}

func TestInspectUsesExactArgumentsAndDescriptor(t *testing.T) {
	helper := writeExecutable(t, "inspect", "#!/bin/sh\n"+
		"[ \"$#\" -eq 19 ] || exit 20\n"+
		"[ \"$1\" = inspect ] && [ \"$2\" = --protocol ] && [ \"$3\" = 1 ] && [ \"$4\" = --input ] && [ \"$5\" = /proc/self/fd/3 ] && [ \"$6\" = --input-mime ] && [ \"$7\" = image/gif ] || exit 21\n"+
		"[ \"$8\" = --max-frames ] && [ \"$9\" = 1000 ] && [ \"${10}\" = --max-duration-ms ] && [ \"${11}\" = 3600000 ] || exit 22\n"+
		"[ \"${12}\" = --max-dimension ] && [ \"${13}\" = 100000 ] && [ \"${14}\" = --max-canvas-pixels ] && [ \"${15}\" = 1000000000 ] || exit 23\n"+
		"[ \"${16}\" = --max-decoded-pixels ] && [ \"${17}\" = 1000000000 ] && [ \"${18}\" = --max-output-bytes ] && [ \"${19}\" = 268435456 ] || exit 24\n"+
		"input=$(cat <&3) || exit 25\n[ \"$input\" = original-bytes ] || exit 26\n"+
		"printf '%s' '"+inspectSuccess+"'\n")
	input, _ := testFiles(t, []byte("original-bytes"))
	if _, err := input.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := newProcessor(t, helper, Policy{}).Inspect(context.Background(), InspectRequest{Input: input, MIMEType: "image/gif"})
	if err != nil {
		t.Fatal(err)
	}
	if got.FrameCount != 3 || got.DurationMS != 390 || got.TotalPlays != 4 || got.ZeroDurationFrameIndices[0] != 1 {
		t.Fatalf("Inspect() = %+v", got)
	}
	if offset, _ := input.Seek(0, io.SeekCurrent); offset != 4 {
		t.Fatalf("caller input offset changed to %d", offset)
	}
}

func TestTransformUsesExactArgumentsAndDescriptors(t *testing.T) {
	helper := writeExecutable(t, "transform", "#!/bin/sh\n"+
		"if [ \"$1\" = verify-output ]; then [ \"$#\" -eq 9 ] && [ \"$5\" = /proc/self/fd/3 ] && [ \"$7\" = image/webp ] || exit 30; printf '%s' '"+webPVerificationSuccess+"'; exit; fi\n"+
		"[ \"$#\" -eq 33 ] || exit 20\n"+
		"[ \"$1\" = transform ] && [ \"$3\" = 1 ] && [ \"$5\" = /proc/self/fd/3 ] && [ \"$7\" = /proc/self/fd/4 ] || exit 21\n"+
		"[ \"$9\" = image/gif ] && [ \"${11}\" = animated-webp ] && [ \"${13}\" = 1920 ] && [ \"${15}\" = 80 ] && [ \"${17}\" = 8 ] && [ \"${19}\" = 1 ] && [ \"${21}\" = /proc/self/fd/5 ] || exit 22\n"+
		"[ \"${23}\" = 1000 ] && [ \"${25}\" = 3600000 ] && [ \"${27}\" = 100000 ] && [ \"${29}\" = 1000000000 ] && [ \"${31}\" = 1000000000 ] && [ \"${33}\" = 268435456 ] || exit 23\n"+
		"[ \"$(cat <&3)\" = original-bytes ] || exit 24\nprintf '%b' '"+shellOctal(fakeWebP)+"' >&4 || exit 25\nprintf '%s' '"+transformSuccess+"'\n")
	input, output := testFiles(t, []byte("original-bytes"))
	got, err := newProcessor(t, helper, Policy{}).Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "image/gif", Recipe: standardRecipe()})
	if err != nil {
		t.Fatal(err)
	}
	if got.OutputMIME != "image/webp" || got.OutputExtension != "webp" || got.Quality != 80 || got.Source.FrameCount != 3 {
		t.Fatalf("Transform() = %+v", got)
	}
	if offset, _ := output.Seek(0, io.SeekCurrent); offset != 0 {
		t.Fatalf("output offset = %d", offset)
	}
}

func TestThumbnailRejectsImpossibleSuccessFixture(t *testing.T) {
	document := strings.NewReplacer(`"output_mime":"image/webp"`, `"output_mime":"image/avif"`, `"quality":80`, `"quality":50`, `"max_long_edge":1920`, `"max_long_edge":640`, `"encoder":"libwebp"`, `"encoder":"aom"`).Replace(transformSuccess)
	helper := jsonHelper(t, document, fakeAVIF)
	input, output := testFiles(t, []byte("input"))
	if _, err := newProcessor(t, helper, Policy{}).Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "image/gif", Recipe: thumbnailRecipe()}); !errors.Is(err, ErrProcess) {
		t.Fatalf("Transform() error = %v", err)
	}
}

func TestPolicyCeilings(t *testing.T) {
	helper := writeExecutable(t, "unused", "#!/bin/sh\nexit 0\n")
	valid := Config{Helper: helper, Prlimit: "/usr/bin/prlimit", SRGBICC: writeICC(t), SRGBICCSHA256: testICCDigest}
	for name, mutate := range map[string]func(*Policy){
		"timeout":   func(p *Policy) { p.Timeout = MaxTimeout + time.Nanosecond },
		"address":   func(p *Policy) { p.AddressSpaceBytes = MaxAddressSpaceBytes + 1 },
		"logs":      func(p *Policy) { p.LogBytesPerStream = MaxLogBytesPerStream + 1 },
		"output":    func(p *Policy) { p.GeneratedOutputMaxBytes = MaxGeneratedOutputBytes + 1 },
		"frames":    func(p *Policy) { p.Frames = MaxFrames + 1 },
		"duration":  func(p *Policy) { p.DurationMS = MaxDurationMS + 1 },
		"dimension": func(p *Policy) { p.Dimension = MaxDimension + 1 },
		"canvas":    func(p *Policy) { p.CanvasPixels = MaxCanvasPixels + 1 },
		"decoded":   func(p *Policy) { p.CumulativeDecodedPixels = MaxCumulativeDecodedPixels + 1 },
		"threads":   func(p *Policy) { p.Threads = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			config.Policy = DefaultPolicy()
			mutate(&config.Policy)
			if _, err := New(config); !errors.Is(err, ErrInvalid) {
				t.Fatalf("New() error = %v", err)
			}
		})
	}
	valid.Policy = Policy{Timeout: time.Second, AddressSpaceBytes: 1 << 30, LogBytesPerStream: 1024, GeneratedOutputMaxBytes: 2048, Frames: 1, DurationMS: 1, Dimension: 1, CanvasPixels: 1, CumulativeDecodedPixels: 1, Threads: 1}
	if _, err := New(valid); err != nil {
		t.Fatalf("tight exact policy: %v", err)
	}
}

func TestRequestValidationAndRouting(t *testing.T) {
	processor := newProcessor(t, writeExecutable(t, "unused", "#!/bin/sh\nexit 9\n"), Policy{})
	input, output := testFiles(t, []byte("input"))
	valid := Request{Input: input, Output: output, MIMEType: "image/gif", Recipe: standardRecipe()}
	for name, mutate := range map[string]func(*Request){
		"unknown MIME": func(r *Request) { r.MIMEType = "image/png" }, "wrong mode": func(r *Request) { r.Recipe.SourceMode = profile.SourceStill },
		"upscale": func(r *Request) { r.Recipe.AllowUpscale = true }, "crop": func(r *Request) { r.Recipe.Crop = "center" }, "edge": func(r *Request) { r.Recipe.MaxLongEdge = 1921 },
		"wrong timing": func(r *Request) { r.Recipe.AnimationTiming = "first-frame" }, "wrong loop": func(r *Request) { r.Recipe.AnimationLoop = "discard" },
		"missing animation": func(r *Request) { r.Recipe.AnimationOutput = nil }, "video output": func(r *Request) { r.Recipe.VideoOutput = &profile.VideoOutput{} },
	} {
		t.Run(name, func(t *testing.T) {
			r := valid
			r.Recipe = cloneRecipe(valid.Recipe)
			mutate(&r)
			if _, err := processor.Transform(context.Background(), r); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Transform() error = %v", err)
			}
		})
	}
	staticDoc := `{"protocol":1,"ok":true,"error_code":"","result":` + staticInspection + `}`
	static, err := newProcessor(t, jsonHelper(t, staticDoc, ""), Policy{}).Inspect(context.Background(), InspectRequest{Input: input, MIMEType: "image/webp"})
	if err != nil || static.Classification != ClassificationStatic {
		t.Fatalf("static Inspect() = %+v, %v", static, err)
	}
	singleGIF := strings.Replace(staticDoc, `"classification":"static"`, `"classification":"animation"`, 1)
	if got, err := newProcessor(t, jsonHelper(t, singleGIF, ""), Policy{}).Inspect(context.Background(), InspectRequest{Input: input, MIMEType: "image/gif"}); err != nil || got.FrameCount != 1 {
		t.Fatalf("single GIF Inspect() = %+v, %v", got, err)
	}
}

func TestInspectionBoundaryValidation(t *testing.T) {
	for name, document := range map[string]string{
		"frame length":        strings.Replace(inspectSuccess, `[40,100,250]`, `[40,100]`, 1),
		"duration sum":        strings.Replace(inspectSuccess, `"duration_ms":390`, `"duration_ms":391`, 1),
		"zero index unsorted": strings.Replace(inspectSuccess, `[1]`, `[1,1]`, 1),
		"zero not normalized": strings.Replace(inspectSuccess, `[40,100,250]`, `[40,99,251]`, 1),
		"axis plus one":       strings.Replace(inspectSuccess, `"width":3`, `"width":100001`, 1),
		"frames plus one":     strings.NewReplacer(`"frame_count":3`, `"frame_count":1001`, `[40,100,250]`, `[100]`).Replace(inspectSuccess),
		"decoded plus one":    strings.Replace(inspectSuccess, `"decoded_pixels":18`, `"decoded_pixels":1000000001`, 1),
		"loop plus one":       strings.Replace(inspectSuccess, `"total_plays":4`, `"total_plays":65536`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			input, _ := testFiles(t, []byte("x"))
			_, err := newProcessor(t, jsonHelper(t, document, ""), Policy{}).Inspect(context.Background(), InspectRequest{Input: input, MIMEType: "image/gif"})
			if !errors.Is(err, ErrProcess) {
				t.Fatalf("Inspect() error = %v", err)
			}
		})
	}
}

func TestStrictProtocolJSON(t *testing.T) {
	for name, document := range map[string]string{
		"malformed": `{`, "trailing": inspectSuccess + `{}`, "unknown": strings.Replace(inspectSuccess, `"width":3`, `"width":3,"path":"secret"`, 1),
		"duplicate": strings.Replace(inspectSuccess, `"width":3`, `"width":3,"width":3`, 1), "null nested": strings.Replace(inspectSuccess, `"width":3`, `"width":null`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			input, _ := testFiles(t, []byte("x"))
			_, err := newProcessor(t, jsonHelper(t, document, ""), Policy{}).Inspect(context.Background(), InspectRequest{Input: input, MIMEType: "image/gif"})
			if !errors.Is(err, ErrProcess) {
				t.Fatalf("Inspect() error = %v", err)
			}
		})
	}
}

func TestHelperAndProcessErrors(t *testing.T) {
	for code, want := range map[string]error{"static_input": ErrStaticInput, "unsupported_input": ErrUnsupportedInput, "decode_failed": ErrDecode, "resource_limit": ErrResourcePolicy, "processing_failed": ErrProcess} {
		t.Run(code, func(t *testing.T) {
			helper := jsonHelper(t, `{"protocol":1,"ok":false,"error_code":"`+code+`","result":null}`, "")
			err := transformError(t, newProcessor(t, helper, Policy{}), context.Background())
			if !errors.Is(err, want) || strings.Contains(err.Error(), code) {
				t.Fatalf("error = %v, want %v", err, want)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		err := transformError(t, newProcessor(t, writeExecutable(t, "timeout", "#!/bin/sh\nsleep 30\n"), Policy{Timeout: 30 * time.Millisecond}), context.Background())
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := transformError(t, newProcessor(t, writeExecutable(t, "cancel", "#!/bin/sh\nsleep 30\n"), Policy{}), ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("log limit", func(t *testing.T) {
		err := transformError(t, newProcessor(t, writeExecutable(t, "overflow", "#!/bin/sh\nwhile :; do printf 0123456789abcdef >&2; done\n"), Policy{LogBytesPerStream: 32}), context.Background())
		if !errors.Is(err, ErrLogOutputLimit) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestTransformFailureClearsPartialOutput(t *testing.T) {
	helper := writeExecutable(t, "partial", "#!/bin/sh\nprintf partial-output >&4\nexit 9\n")
	processor := newProcessor(t, helper, Policy{})
	input, output := testFiles(t, []byte("input"))
	if _, err := processor.Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "image/gif", Recipe: standardRecipe()}); !errors.Is(err, ErrProcess) {
		t.Fatalf("Transform() error = %v", err)
	}
	info, err := output.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("failed output size = %d", info.Size())
	}
	if offset, err := output.Seek(0, io.SeekCurrent); err != nil || offset != 0 {
		t.Fatalf("failed output offset = %d, %v", offset, err)
	}
}

func TestTransformRejectsInvalidOutputSignature(t *testing.T) {
	helper := jsonHelper(t, transformSuccess, "not-a-webp!")
	err := transformError(t, newProcessor(t, helper, Policy{}), context.Background())
	if !errors.Is(err, ErrProcess) {
		t.Fatalf("Transform() error = %v", err)
	}
}

func TestTransformRejectsMalformedOutputContainer(t *testing.T) {
	wrongRIFFSize := []byte(fakeWebP)
	wrongRIFFSize[4]++
	missingAnimation := strings.Replace(fakeWebP, "ANIM", "JUNK", 1)
	truncatedChunk := fakeWebP[:len(fakeWebP)-1]
	for name, output := range map[string]string{
		"RIFF declared size": string(wrongRIFFSize),
		"missing ANIM":       missingAnimation,
		"truncated chunk":    truncatedChunk,
	} {
		t.Run(name, func(t *testing.T) {
			if err := transformError(t, newProcessor(t, jsonHelper(t, transformSuccess, output), Policy{}), context.Background()); !errors.Is(err, ErrProcess) {
				t.Fatalf("Transform() error = %v", err)
			}
		})
	}
	document := strings.NewReplacer(`"output_mime":"image/webp"`, `"output_mime":"image/avif"`, `"quality":80`, `"quality":50`, `"max_long_edge":1920`, `"max_long_edge":640`, `"encoder":"libwebp"`, `"encoder":"aom"`).Replace(transformSuccess)
	for name, bytes := range map[string]string{
		"AVIF truncated box":       fakeAVIF[:len(fakeAVIF)-1],
		"AVIF missing iprp":        strings.Replace(fakeAVIF, "iprp", "free", 1),
		"AVIF incompatible brands": strings.ReplaceAll(fakeAVIF, "avif", "xxxx"),
	} {
		t.Run(name, func(t *testing.T) {
			input, output := testFiles(t, []byte("input"))
			_, err := newProcessor(t, jsonHelper(t, document, bytes), Policy{}).Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "image/gif", Recipe: thumbnailRecipe()})
			if !errors.Is(err, ErrProcess) {
				t.Fatalf("Transform() error = %v", err)
			}
		})
	}
}

func TestTransformRejectsContradictoryOutputFacts(t *testing.T) {
	if err := transformError(t, newProcessor(t, jsonHelper(t, transformSuccess, contradictoryWebP), Policy{}), context.Background()); !errors.Is(err, ErrProcess) {
		t.Fatalf("one-frame 1x1 WebP reported as three-frame 3x2 output: %v", err)
	}
}

func TestCommandSpecificHelperErrors(t *testing.T) {
	errorDocument := func(code string) string {
		return `{"protocol":1,"ok":false,"error_code":"` + code + `","result":null}`
	}
	if _, err := newProcessor(t, jsonHelper(t, errorDocument("static_input"), ""), Policy{}).Capabilities(context.Background()); !errors.Is(err, ErrCapability) {
		t.Fatalf("Capabilities() cross-command error = %v", err)
	}
	input, _ := testFiles(t, []byte("input"))
	if _, err := newProcessor(t, jsonHelper(t, errorDocument("encode_failed"), ""), Policy{}).Inspect(context.Background(), InspectRequest{Input: input, MIMEType: "image/gif"}); !errors.Is(err, ErrProcess) {
		t.Fatalf("Inspect() cross-command error = %v", err)
	}
	input, _ = testFiles(t, []byte("input"))
	if _, err := newProcessor(t, jsonHelper(t, errorDocument("capability_failed"), ""), Policy{}).Inspect(context.Background(), InspectRequest{Input: input, MIMEType: "image/webp"}); !errors.Is(err, ErrCapability) {
		t.Fatalf("Inspect() capability error = %v", err)
	}
}

func TestResizeGeometryPreservesOddAndOnePixel(t *testing.T) {
	for _, test := range []struct {
		sw, sh, ow, oh, edge int
		want                 bool
	}{{3, 2, 3, 2, 1920, true}, {3000, 2001, 1920, 1281, 1920, true}, {100000, 1, 1920, 1, 1920, true}, {1, 100000, 1, 1920, 1920, true}, {100, 100, 200, 200, 1920, false}} {
		if got := validResize(test.sw, test.sh, test.ow, test.oh, test.edge); got != test.want {
			t.Fatalf("validResize(%+v) = %v", test, got)
		}
	}
}

func standardRecipe() profile.Recipe  { return profile.StandardV1Parameters().Recipes["image/gif"] }
func thumbnailRecipe() profile.Recipe { return profile.ThumbnailV1Parameters().Recipes["image/gif"] }
func cloneRecipe(r profile.Recipe) profile.Recipe {
	if r.StillOutput != nil {
		v := *r.StillOutput
		r.StillOutput = &v
	}
	if r.AnimationOutput != nil {
		v := *r.AnimationOutput
		r.AnimationOutput = &v
	}
	return r
}

func newProcessor(t *testing.T, helper string, policy Policy) *Processor {
	t.Helper()
	icc := writeICC(t)
	processor, err := New(Config{Helper: helper, Prlimit: "/usr/bin/prlimit", SRGBICC: icc, SRGBICCSHA256: testICCDigest, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return processor
}
func writeICC(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "srgb.icc")
	if err := os.WriteFile(path, []byte("test-srgb-icc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte("test-srgb-icc"))); got != testICCDigest {
		t.Fatalf("test digest = %s", got)
	}
	return path
}
func testFiles(t *testing.T, data []byte) (*os.File, *os.File) {
	t.Helper()
	inputPath := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(inputPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close(); output.Close() })
	return input, output
}
func writeExecutable(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
func jsonHelper(t *testing.T, document, output string) string {
	t.Helper()
	body := "#!/bin/sh\nif [ \"$1\" = verify-output ]; then printf '%s' '" + webPVerificationSuccess + "'; exit; fi\n"
	if output != "" {
		body += "printf '%b' '" + shellOctal(output) + "' >&4\n"
	}
	body += "printf '%s' '" + document + "'\n"
	return writeExecutable(t, "json-helper", body)
}

func shellOctal(value string) string {
	var result strings.Builder
	for _, value := range []byte(value) {
		fmt.Fprintf(&result, `\%03o`, value)
	}
	return result.String()
}
func transformError(t *testing.T, p *Processor, ctx context.Context) error {
	t.Helper()
	input, output := testFiles(t, []byte("input"))
	_, err := p.Transform(ctx, Request{Input: input, Output: output, MIMEType: "image/gif", Recipe: standardRecipe()})
	return err
}
