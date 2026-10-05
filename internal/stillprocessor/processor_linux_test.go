//go:build linux

package stillprocessor

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

const capabilitySuccess = `{"protocol":1,"ok":true,"error_code":"","result":{"helper_version":"helper-1.2.3","library_versions":{"libvips":"8.18.7","libheif":"1.23.5","libraw":"0.22.2","libaom":"3.8.2","lcms2":"2.14"},"decoder_mime_types":["image/jpeg","image/webp"],"avif_encoder":"aom","icc_sha256":"` + testICCDigest + `","threads":1}}`

const transformSuccess = `{"protocol":1,"ok":true,"error_code":"","result":{"output_mime":"image/avif","width":1200,"height":800,"quality":60,"bit_depth":8,"max_long_edge":1920,"threads":1,"audit":{"decoder":"libvips-jpeg","encoder":"aom","tool_version":"helper-1.2.3","library_versions":{"libvips":"8.18.7","libheif":"1.23.5","libraw":"0.22.2","libaom":"3.8.2","lcms2":"2.14"},"icc_sha256":"` + testICCDigest + `","orientation":"applied","source_width":1200,"source_height":800,"input_color":"assumed-srgb","input_primaries":"srgb","input_transfer":"srgb","input_range":"not-applicable","output_color":"srgb","output_transfer":"srgb","hdr_disposition":"sdr","tone_map":"not-needed","target_nits":0,"hdr_peak_nits":0,"hlg_reference_nits":0,"raw_processing":"not-applicable","alpha":"opaque","metadata":"strip-after-normalization-keep-color-tags","chroma":"4:2:0"}}}`

func TestCapabilities(t *testing.T) {
	helper := writeExecutable(t, "capabilities", "#!/bin/sh\n"+
		"[ \"$#\" -eq 7 ] && [ \"$1\" = capabilities ] && [ \"$2\" = --protocol ] && [ \"$3\" = 1 ] && [ \"$4\" = --threads ] && [ \"$5\" = 1 ] && [ \"$6\" = --srgb-icc ] || exit 7\n"+
		"printf '%s' '"+capabilitySuccess+"'\n")
	processor := newProcessor(t, helper, Policy{})
	capabilities, err := processor.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.ProtocolVersion != 1 || capabilities.HelperVersion != "helper-1.2.3" || capabilities.AVIFEncoder != "aom" || capabilities.Threads != 1 {
		t.Fatalf("Capabilities() = %+v", capabilities)
	}
	if len(capabilities.DecoderMIMETypes) != 2 || capabilities.LibraryVersions["libvips"] != "8.18.7" {
		t.Fatalf("Capabilities() data = %+v", capabilities)
	}
}

func TestTransformUsesExactArgumentsAndDescriptors(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "injected")
	icc := writeICC(t, directory)
	helper := writeExecutable(t, "transform", "#!/bin/sh\n"+
		"[ \"$#\" -eq 23 ] || exit 20\n"+
		"[ \"$1\" = transform ] && [ \"$2\" = --protocol ] && [ \"$3\" = 1 ] || exit 21\n"+
		"[ \"$4\" = --input ] && [ \"$5\" = /proc/self/fd/3 ] && [ \"$6\" = --output ] && [ \"$7\" = /proc/self/fd/4 ] || exit 22\n"+
		"[ \"$8\" = --input-mime ] && [ \"$9\" = image/jpeg ] && [ \"${10}\" = --source-mode ] && [ \"${11}\" = still ] || exit 23\n"+
		"[ \"${12}\" = --max-long-edge ] && [ \"${13}\" = 1920 ] && [ \"${14}\" = --quality ] && [ \"${15}\" = 60 ] || exit 24\n"+
		"[ \"${16}\" = --bit-depth ] && [ \"${17}\" = 8 ] && [ \"${18}\" = --threads ] && [ \"${19}\" = 1 ] || exit 25\n"+
		"[ \"${20}\" = --max-output-bytes ] && [ \"${21}\" = 67108864 ] && [ \"${22}\" = --srgb-icc ] && [ \"${23}\" = /proc/self/fd/5 ] || exit 26\n"+
		"input=$(cat <&3) || exit 27\n"+
		"[ \"$input\" = original-bytes ] || exit 28\n"+
		"printf avif-bytes >&4 || exit 29\n"+
		"printf '%s' '"+transformSuccess+"'\n")
	processor := newProcessorWithICC(t, helper, icc, Policy{})
	input, output := testFiles(t, []byte("original-bytes"))
	if _, err := input.Seek(4, 0); err != nil {
		t.Fatal(err)
	}
	result, err := processor.Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "image/jpeg", Recipe: stillRecipe()})
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputMIME != OutputMIME || result.OutputExtension != "avif" || result.Width != 1200 || result.Audit.Encoder != "aom" {
		t.Fatalf("Transform() = %+v", result)
	}
	if offset, err := output.Seek(0, io.SeekCurrent); err != nil || offset != 0 {
		t.Fatalf("output offset = %d, %v", offset, err)
	}
	offset, err := input.Seek(0, 1)
	if err != nil || offset != 4 {
		t.Fatalf("input offset = %d, %v", offset, err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(input.Name())
	if err != nil || string(contents) != "original-bytes" {
		t.Fatalf("input contents = %q, %v", contents, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("argument was interpreted by a shell: %v", err)
	}
}

func TestConfigurationAndPolicyValidation(t *testing.T) {
	helper := writeExecutable(t, "ok", "#!/bin/sh\nexit 0\n")
	valid := Config{Helper: helper, Prlimit: "/usr/bin/prlimit", SRGBICC: writeICC(t, t.TempDir()), SRGBICCSHA256: testICCDigest}
	for name, mutate := range map[string]func(*Config){
		"relative helper":  func(c *Config) { c.Helper = "helper" },
		"unclean prlimit":  func(c *Config) { c.Prlimit = "/usr/bin/../bin/prlimit" },
		"relative ICC":     func(c *Config) { c.SRGBICC = "srgb.icc" },
		"bad ICC digest":   func(c *Config) { c.SRGBICCSHA256 = strings.Repeat("0", 64) },
		"timeout ceiling":  func(c *Config) { c.Policy.Timeout = MaxTimeout + time.Nanosecond },
		"address ceiling":  func(c *Config) { c.Policy.AddressSpaceBytes = MaxAddressSpaceBytes + 1 },
		"log ceiling":      func(c *Config) { c.Policy.LogBytesPerStream = MaxLogBytesPerStream + 1 },
		"output ceiling":   func(c *Config) { c.Policy.GeneratedOutputMaxBytes = MaxGeneratedOutputBytes + 1 },
		"threads":          func(c *Config) { c.Policy.Threads = 2 },
		"negative timeout": func(c *Config) { c.Policy.Timeout = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := New(config); !errors.Is(err, ErrInvalid) {
				t.Fatalf("New() error = %v", err)
			}
		})
	}
	tight := Policy{Timeout: time.Second, AddressSpaceBytes: 1 << 30, LogBytesPerStream: 1024, GeneratedOutputMaxBytes: 2048, Threads: 1}
	valid.Policy = tight
	if _, err := New(valid); err != nil {
		t.Fatalf("tight policy: %v", err)
	}
	if DefaultPolicy().Threads != 1 || DefaultPolicy().Timeout != MaxTimeout {
		t.Fatalf("DefaultPolicy() = %+v", DefaultPolicy())
	}
}

func TestRequestValidation(t *testing.T) {
	helper := writeExecutable(t, "unused", "#!/bin/sh\nexit 99\n")
	processor := newProcessor(t, helper, Policy{})
	input, output := testFiles(t, []byte("input"))
	valid := Request{Input: input, Output: output, MIMEType: "image/jpeg", Recipe: stillRecipe()}

	tests := map[string]func(*Request){
		"nil input":       func(r *Request) { r.Input = nil },
		"same file":       func(r *Request) { r.Output = input },
		"unknown MIME":    func(r *Request) { r.MIMEType = "image/jpg" },
		"GIF":             func(r *Request) { r.MIMEType = "image/gif"; r.Recipe = webpRecipe() },
		"video":           func(r *Request) { r.MIMEType = "video/mp4" },
		"WebP wrong mode": func(r *Request) { r.MIMEType = "image/webp" },
		"upscale":         func(r *Request) { r.Recipe.AllowUpscale = true },
		"crop":            func(r *Request) { r.Recipe.Crop = "center" },
		"edge zero":       func(r *Request) { r.Recipe.MaxLongEdge = 0 },
		"edge ceiling":    func(r *Request) { r.Recipe.MaxLongEdge = 1921 },
		"quality":         func(r *Request) { r.Recipe.StillOutput.Quality = 0 },
		"bit depth":       func(r *Request) { r.Recipe.StillOutput.BitDepth = 10 },
		"animation output": func(r *Request) {
			r.Recipe.AnimationOutput = &profile.AnimationOutput{Format: "animated-webp", Quality: 80, BitDepth: 8}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := valid
			copyOutput := *valid.Recipe.StillOutput
			request.Recipe.StillOutput = &copyOutput
			mutate(&request)
			if _, err := processor.Transform(context.Background(), request); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Transform() error = %v", err)
			}
		})
	}

	request := valid
	request.MIMEType = "image/webp"
	request.Recipe = webpRecipe()
	if _, err := processor.Transform(context.Background(), request); errors.Is(err, ErrInvalid) {
		t.Fatalf("static WebP was rejected as invalid: %v", err)
	}
	request.Recipe = profile.StandardV1Parameters().Recipes["image/webp"]
	if _, err := processor.Transform(context.Background(), request); errors.Is(err, ErrInvalid) {
		t.Fatalf("standard static WebP was rejected as invalid: %v", err)
	}
}

func TestRequestRequiresRegularFiles(t *testing.T) {
	helper := writeExecutable(t, "unused", "#!/bin/sh\nexit 99\n")
	processor := newProcessor(t, helper, Policy{})
	input, output := testFiles(t, []byte("input"))
	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pipeRead.Close() })
	t.Cleanup(func() { _ = pipeWrite.Close() })
	for name, request := range map[string]Request{
		"pipe input":  {Input: pipeRead, Output: output, MIMEType: "image/jpeg", Recipe: stillRecipe()},
		"pipe output": {Input: input, Output: pipeWrite, MIMEType: "image/jpeg", Recipe: stillRecipe()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := processor.Transform(context.Background(), request); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Transform() error = %v", err)
			}
		})
	}
}

func TestStrictCapabilityJSON(t *testing.T) {
	tests := map[string]string{
		"malformed":       `{`,
		"trailing":        capabilitySuccess + ` {}`,
		"unknown":         strings.Replace(capabilitySuccess, `"threads":1`, `"threads":1,"extra":1`, 1),
		"missing":         strings.Replace(capabilitySuccess, `,"threads":1`, ``, 1),
		"duplicate":       strings.Replace(capabilitySuccess, `"threads":1`, `"threads":1,"threads":1`, 1),
		"unsafe version":  strings.Replace(capabilitySuccess, "helper-1.2.3", `helper\nsecret`, 1),
		"bad MIME":        strings.Replace(capabilitySuccess, `image/jpeg`, `image/gif`, 1),
		"duplicate MIME":  strings.Replace(capabilitySuccess, `"image/jpeg","image/webp"`, `"image/jpeg","image/jpeg"`, 1),
		"bad digest":      strings.Replace(capabilitySuccess, testICCDigest, strings.Repeat("a", 64), 1),
		"missing library": strings.Replace(capabilitySuccess, `,"lcms2":"2.14"`, ``, 1),
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			helper := jsonHelper(t, document, "")
			_, err := newProcessor(t, helper, Policy{}).Capabilities(context.Background())
			if !errors.Is(err, ErrCapability) {
				t.Fatalf("Capabilities() error = %v", err)
			}
		})
	}
}

func TestStrictTransformJSONAndOutputValidation(t *testing.T) {
	tests := map[string]struct {
		document string
		output   string
		want     error
	}{
		"malformed":        {`{`, "avif", ErrProcess},
		"trailing":         {transformSuccess + ` []`, "avif", ErrProcess},
		"unknown":          {strings.Replace(transformSuccess, `"width":1200`, `"width":1200,"secret":"x"`, 1), "avif", ErrProcess},
		"unsafe audit":     {strings.Replace(transformSuccess, "libvips-jpeg", `libvips-jpeg\npath`, 1), "avif", ErrProcess},
		"wrong dimensions": {strings.Replace(transformSuccess, `"width":1200`, `"width":1921`, 1), "avif", ErrProcess},
		"distorted aspect": {strings.Replace(transformSuccess, `"height":800`, `"height":799`, 1), "avif", ErrProcess},
		"PQ not mapped":    {strings.Replace(transformSuccess, `"input_color":"assumed-srgb"`, `"input_color":"nclx-pq"`, 1), "avif", ErrProcess},
		"raw on JPEG":      {strings.Replace(transformSuccess, `"input_color":"assumed-srgb"`, `"input_color":"raw-camera-matrix"`, 1), "avif", ErrProcess},
		"alpha mismatch":   {strings.Replace(transformSuccess, `"alpha":"opaque"`, `"alpha":"preserved"`, 1), "avif", ErrProcess},
		"ICC mismatch":     {strings.Replace(transformSuccess, testICCDigest, strings.Repeat("a", 64), 1), "avif", ErrProcess},
		"null scalar":      {strings.Replace(transformSuccess, `"target_nits":0`, `"target_nits":null`, 1), "avif", ErrProcess},
		"odd one pixel": {strings.NewReplacer(
			`"width":1200`, `"width":1`, `"height":800`, `"height":1`,
			`"source_width":1200`, `"source_width":1`, `"source_height":800`, `"source_height":1`,
		).Replace(transformSuccess), "a", nil},
		"empty output":    {transformSuccess, "", ErrResourcePolicy},
		"oversize output": {transformSuccess, "12345", ErrResourcePolicy},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			helper := jsonHelper(t, test.document, test.output)
			processor := newProcessor(t, helper, Policy{GeneratedOutputMaxBytes: 4})
			input, output := testFiles(t, []byte("input"))
			_, err := processor.Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "image/jpeg", Recipe: stillRecipe()})
			if !errors.Is(err, test.want) {
				t.Fatalf("Transform() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestHDRAndRAWAuditSuccess(t *testing.T) {
	hdr := strings.NewReplacer(
		`"decoder":"libvips-jpeg"`, `"decoder":"libheif-heif"`,
		`"input_color":"assumed-srgb"`, `"input_color":"nclx-pq"`,
		`"input_primaries":"srgb"`, `"input_primaries":"bt2020"`,
		`"input_transfer":"srgb"`, `"input_transfer":"pq"`,
		`"input_range":"not-applicable"`, `"input_range":"limited"`,
		`"hdr_disposition":"sdr"`, `"hdr_disposition":"tone-mapped"`,
		`"tone_map":"not-needed"`, `"tone_map":"bt2446a-method-a"`,
		`"target_nits":0`, `"target_nits":100`,
		`"hdr_peak_nits":0`, `"hdr_peak_nits":1000`,
	).Replace(transformSuccess)
	raw := strings.NewReplacer(
		`"decoder":"libvips-jpeg"`, `"decoder":"libraw-dng"`,
		`"input_color":"assumed-srgb"`, `"input_color":"raw-camera-matrix"`,
		`"input_primaries":"srgb"`, `"input_primaries":"camera-matrix"`,
		`"input_transfer":"srgb"`, `"input_transfer":"libraw-srgb"`,
		`"raw_processing":"not-applicable"`, `"raw_processing":"camera-wb-camera-matrix-16bit-no-auto-bright"`,
	).Replace(transformSuccess)
	for name, test := range map[string]struct {
		document string
		mimeType string
	}{
		"HDR": {document: hdr, mimeType: "image/heif"},
		"RAW": {document: raw, mimeType: "image/dng"},
	} {
		t.Run(name, func(t *testing.T) {
			helper := jsonHelper(t, test.document, "avif")
			input, output := testFiles(t, []byte("input"))
			recipe := profile.StandardV1Parameters().Recipes[test.mimeType]
			if _, err := newProcessor(t, helper, Policy{}).Transform(context.Background(), Request{
				Input: input, Output: output, MIMEType: test.mimeType, Recipe: recipe,
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResizeGeometry(t *testing.T) {
	for name, test := range map[string]struct {
		sourceWidth, sourceHeight int
		outputWidth, outputHeight int
		want                      bool
	}{
		"exact scaled":     {3000, 2000, 1920, 1280, true},
		"one pixel short":  {3000, 2000, 1920, 1279, false},
		"rounded floor":    {3000, 2001, 1920, 1280, false},
		"rounded ceil":     {3000, 2001, 1920, 1281, true},
		"one pixel source": {1, 1, 1, 1, true},
		"upscale":          {100, 100, 200, 200, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := validResize(test.sourceWidth, test.sourceHeight, test.outputWidth, test.outputHeight, 1920); got != test.want {
				t.Fatalf("validResize() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestICCReplacementFailsClosed(t *testing.T) {
	directory := t.TempDir()
	icc := writeICC(t, directory)
	helper := jsonHelper(t, capabilitySuccess, "")
	processor := newProcessorWithICC(t, helper, icc, Policy{})
	if err := os.WriteFile(icc, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.Capabilities(context.Background()); !errors.Is(err, ErrCapability) {
		t.Fatalf("Capabilities() error = %v", err)
	}
}

func TestHelperErrorCodes(t *testing.T) {
	for code, want := range map[string]error{
		"animated_input":    ErrAnimatedInput,
		"unsupported_input": ErrUnsupportedInput,
		"decode_failed":     ErrDecode,
		"resource_limit":    ErrResourcePolicy,
		"processing_failed": ErrProcess,
	} {
		t.Run(code, func(t *testing.T) {
			document := `{"protocol":1,"ok":false,"error_code":"` + code + `","result":null}`
			helper := jsonHelper(t, document, "")
			input, output := testFiles(t, []byte("input"))
			_, err := newProcessor(t, helper, Policy{}).Transform(context.Background(), Request{Input: input, Output: output, MIMEType: "image/jpeg", Recipe: stillRecipe()})
			if !errors.Is(err, want) || strings.Contains(err.Error(), code) {
				t.Fatalf("Transform() error = %v, want %v", err, want)
			}
		})
	}
}

func TestProcessFailureMappings(t *testing.T) {
	t.Run("nonzero", func(t *testing.T) {
		helper := writeExecutable(t, "nonzero", "#!/bin/sh\nprintf 'SECRET=/private/input' >&2\nexit 9\n")
		err := transformError(t, newProcessor(t, helper, Policy{}), context.Background())
		if !errors.Is(err, ErrProcess) || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "/private") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		helper := writeExecutable(t, "timeout", "#!/bin/sh\nsleep 30\n")
		err := transformError(t, newProcessor(t, helper, Policy{Timeout: 30 * time.Millisecond}), context.Background())
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		helper := writeExecutable(t, "cancel", "#!/bin/sh\nsleep 30\n")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := transformError(t, newProcessor(t, helper, Policy{}), ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("output limit", func(t *testing.T) {
		helper := writeExecutable(t, "overflow", "#!/bin/sh\nwhile :; do printf 0123456789abcdef >&2; done\n")
		err := transformError(t, newProcessor(t, helper, Policy{LogBytesPerStream: 32}), context.Background())
		if !errors.Is(err, ErrLogOutputLimit) {
			t.Fatalf("error = %v", err)
		}
	})
}

func newProcessor(t *testing.T, helper string, policy Policy) *Processor {
	t.Helper()
	return newProcessorWithICC(t, helper, writeICC(t, t.TempDir()), policy)
}

func newProcessorWithICC(t *testing.T, helper, icc string, policy Policy) *Processor {
	t.Helper()
	data, err := os.ReadFile(icc)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	processor, err := New(Config{Helper: helper, Prlimit: "/usr/bin/prlimit", SRGBICC: icc, SRGBICCSHA256: digest, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func writeICC(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "srgb.icc")
	if err := os.WriteFile(path, []byte("test-srgb-icc"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func stillRecipe() profile.Recipe {
	return profile.StandardV1Parameters().Recipes["image/jpeg"]
}

func webpRecipe() profile.Recipe {
	return profile.ThumbnailV1Parameters().Recipes["image/webp"]
}

func testFiles(t *testing.T, inputBytes []byte) (*os.File, *os.File) {
	t.Helper()
	inputPath := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(inputPath, inputBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	output, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
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
	body := "#!/bin/sh\n"
	if output != "" {
		body += "printf '%s' '" + output + "' >&4\n"
	}
	body += "printf '%s' '" + document + "'\n"
	return writeExecutable(t, "json-helper", body)
}

func transformError(t *testing.T, processor *Processor, ctx context.Context) error {
	t.Helper()
	input, output := testFiles(t, []byte("input"))
	_, err := processor.Transform(ctx, Request{Input: input, Output: output, MIMEType: "image/jpeg", Recipe: stillRecipe()})
	return err
}
