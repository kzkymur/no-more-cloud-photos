//go:build linux

package videohelper

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type engine struct {
	ffprobe  string
	ffmpeg   string
	run      commandRunner
	manifest string
	prefix   string
	closure  func() error
}

func Main(args []string, stdout, stderr io.Writer) int {
	_ = stderr // Codec diagnostics are intentionally not copied to the protocol streams.
	e, err := newEngine()
	if err == nil {
		err = e.execute(args, stdout)
	}
	if err == nil {
		return 0
	}
	code := "internal_error"
	var protocolErr helperError
	if errors.As(err, &protocolErr) {
		code = protocolErr.code
	}
	_ = writeEnvelope(stdout, false, code, nil)
	return 0
}

func newEngine() (*engine, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fail("capability_failed")
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil || !filepath.IsAbs(executable) {
		return nil, fail("capability_failed")
	}
	dir := filepath.Dir(executable)
	ffprobe, err := siblingExecutable(dir, "ffprobe")
	if err != nil {
		return nil, err
	}
	ffmpeg, err := siblingExecutable(dir, "ffmpeg")
	if err != nil {
		return nil, err
	}
	prefix := filepath.Dir(dir)
	manifestBytes, err := os.ReadFile(filepath.Join(prefix, "share", "nmcp", "video-toolchain.manifest"))
	if err != nil {
		return nil, fail("capability_failed")
	}
	e := &engine{ffprobe: ffprobe, ffmpeg: ffmpeg, run: osCommandRunner{}, manifest: string(manifestBytes), prefix: prefix}
	e.closure = e.validateRuntimeClosure
	if err := e.closure(); err != nil {
		return nil, fail("capability_failed")
	}
	return e, nil
}

func siblingExecutable(dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fail("capability_failed")
	}
	return path, nil
}

func (e *engine) execute(args []string, out io.Writer) error {
	r, err := parseRequest(args)
	if err != nil {
		return err
	}
	switch r.command {
	case "capabilities":
		return e.capabilities(r, out)
	case "inspect":
		i, _, err := e.inspect(r.input, r.mime, r.limits)
		if err != nil {
			return err
		}
		return writeEnvelope(out, true, "", i)
	case "transform":
		return e.transform(r, out)
	case "verify-output":
		return e.verify(r, out)
	default:
		return fail("policy_violation")
	}
}

func writeEnvelope(w io.Writer, ok bool, code string, result any) error {
	return json.NewEncoder(w).Encode(struct {
		Protocol  int    `json:"protocol"`
		OK        bool   `json:"ok"`
		ErrorCode string `json:"error_code"`
		Result    any    `json:"result"`
	}{protocolVersion, ok, code, result})
}

func parseRequest(a []string) (request, error) {
	if len(a) == 0 {
		return request{}, fail("policy_violation")
	}
	r := request{command: a[0]}
	var names []string
	switch r.command {
	case "capabilities":
		names = []string{"--protocol", "--threads", "--srgb-icc"}
	case "inspect":
		names = append([]string{"--protocol", "--input", "--input-mime"}, limitNames...)
	case "transform":
		names = append([]string{"--protocol", "--input", "--output", "--input-mime", "--output-kind", "--max-long-edge", "--quality", "--bit-depth", "--threads", "--srgb-icc", "--generated-bytes-before"}, limitNames...)
	case "verify-output":
		names = append([]string{"--protocol", "--source", "--output", "--source-mime", "--output-kind"}, limitNames...)
	default:
		return request{}, fail("policy_violation")
	}
	if len(a) != 1+2*len(names) {
		return request{}, fail("policy_violation")
	}
	values := make(map[string]string, len(names))
	for i, name := range names {
		if a[1+i*2] != name || a[2+i*2] == "" || strings.ContainsRune(a[2+i*2], '\x00') {
			return request{}, fail("policy_violation")
		}
		values[name] = a[2+i*2]
	}
	if values["--protocol"] != "1" {
		return request{}, fail("policy_violation")
	}
	r.input, r.output, r.source = values["--input"], values["--output"], values["--source"]
	r.mime, r.kind, r.icc = values["--input-mime"], values["--output-kind"], values["--srgb-icc"]
	if r.mime == "" {
		r.mime = values["--source-mime"]
	}
	if !oneOf(r.mime, "", "video/mp4", "video/quicktime") || !oneOf(r.kind, "", "mp4-av1", "first-frame-avif") {
		return request{}, fail("unsupported_input")
	}
	paths := map[string]string{"--input": "/proc/self/fd/3", "--source": "/proc/self/fd/3", "--output": "/proc/self/fd/4"}
	for flag, expected := range paths {
		if value := values[flag]; value != "" && value != expected {
			return request{}, fail("policy_violation")
		}
	}
	if r.command == "capabilities" && r.icc != "/proc/self/fd/3" || r.command == "transform" && r.icc != "/proc/self/fd/5" {
		return request{}, fail("policy_violation")
	}
	var err error
	if value := values["--threads"]; value != "" {
		r.threads, err = decimalInt(value)
		if err != nil || r.threads != 1 {
			return request{}, fail("policy_violation")
		}
	}
	if r.command == "transform" {
		r.generatedBytesBefore, err = strconv.ParseInt(values["--generated-bytes-before"], 10, 64)
		if err != nil || r.generatedBytesBefore < 0 || strconv.FormatInt(r.generatedBytesBefore, 10) != values["--generated-bytes-before"] {
			return request{}, fail("policy_violation")
		}
		if r.maxLongEdge, err = decimalInt(values["--max-long-edge"]); err != nil || r.maxLongEdge <= 0 || r.maxLongEdge > map[bool]int{true: 1920, false: 640}[r.kind == "mp4-av1"] {
			return request{}, fail("policy_violation")
		}
		if r.quality, err = decimalInt(values["--quality"]); err != nil || r.quality < 0 || r.quality > 100 || r.kind == "mp4-av1" && r.quality != 0 || r.kind == "first-frame-avif" && r.quality == 0 {
			return request{}, fail("policy_violation")
		}
		if r.bitDepth, err = decimalInt(values["--bit-depth"]); err != nil || r.kind == "mp4-av1" && r.bitDepth != 10 || r.kind == "first-frame-avif" && r.bitDepth != 8 {
			return request{}, fail("policy_violation")
		}
	}
	if r.command != "capabilities" {
		if err := parseLimits(values, &r.limits); err != nil {
			return request{}, err
		}
	}
	if r.command == "transform" && r.generatedBytesBefore > r.limits.GeneratedBytes {
		return request{}, fail("policy_violation")
	}
	return r, nil
}

var limitNames = []string{
	"--max-streams", "--max-dimension", "--max-pixels-per-frame", "--max-duration-us", "--max-fps-numerator", "--max-fps-denominator", "--max-frames", "--max-decoded-pixels", "--max-audio-channels", "--max-audio-sample-rate", "--max-video-output-bytes", "--max-thumbnail-output-bytes", "--max-generated-output-bytes",
}

func parseLimits(v map[string]string, l *limits) error {
	ints := []struct {
		name string
		to   *int
		max  int
	}{{"--max-streams", &l.Streams, maxStreams}, {"--max-dimension", &l.Dimension, maxDimension}, {"--max-frames", &l.Frames, maxFrames}, {"--max-audio-channels", &l.AudioChannels, maxAudioChannels}, {"--max-audio-sample-rate", &l.AudioRate, maxAudioRate}}
	for _, item := range ints {
		n, err := decimalInt(v[item.name])
		if err != nil || n <= 0 || n > item.max {
			return fail("policy_violation")
		}
		*item.to = n
	}
	u64s := []struct {
		name string
		to   *uint64
		max  uint64
	}{{"--max-pixels-per-frame", &l.Pixels, maxPixels}, {"--max-decoded-pixels", &l.DecodedPixels, maxDecodedPixels}}
	for _, item := range u64s {
		n, err := strconv.ParseUint(v[item.name], 10, 64)
		if err != nil || n == 0 || n > item.max || strconv.FormatUint(n, 10) != v[item.name] {
			return fail("policy_violation")
		}
		*item.to = n
	}
	i64s := []struct {
		name string
		to   *int64
		max  int64
	}{{"--max-duration-us", &l.DurationUS, maxDurationUS}, {"--max-fps-numerator", &l.FPSNum, 240}, {"--max-fps-denominator", &l.FPSDen, 1<<62 - 1}, {"--max-video-output-bytes", &l.VideoBytes, maxVideoBytes}, {"--max-thumbnail-output-bytes", &l.ThumbBytes, maxThumbBytes}, {"--max-generated-output-bytes", &l.GeneratedBytes, maxGenerated}}
	for _, item := range i64s {
		n, err := strconv.ParseInt(v[item.name], 10, 64)
		if err != nil || n <= 0 || n > item.max || strconv.FormatInt(n, 10) != v[item.name] {
			return fail("policy_violation")
		}
		*item.to = n
	}
	if fractionGreater(l.FPSNum, l.FPSDen, 240, 1) || l.VideoBytes > l.GeneratedBytes || l.ThumbBytes > l.GeneratedBytes {
		return fail("policy_violation")
	}
	return nil
}

func decimalInt(s string) (int, error) {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || strconv.FormatInt(n, 10) != s {
		return 0, errors.New("invalid decimal")
	}
	return int(n), nil
}

func (e *engine) capabilities(r request, out io.Writer) error {
	digest, err := digestFile(r.icc)
	if err != nil || e.closure == nil || e.closure() != nil || e.checkTools() != nil {
		return fail("capability_failed")
	}
	result := struct {
		HelperVersion    string            `json:"helper_version"`
		LibraryVersions  map[string]string `json:"library_versions"`
		DecoderMIMETypes []string          `json:"decoder_mime_types"`
		OutputKinds      []string          `json:"output_kinds"`
		VideoEncoder     string            `json:"video_encoder"`
		AudioEncoder     string            `json:"audio_encoder"`
		VideoMuxer       string            `json:"video_muxer"`
		ToneMap          string            `json:"tone_map"`
		ICCSHA256        string            `json:"icc_sha256"`
		Threads          int               `json:"threads"`
		BuildManifest    string            `json:"build_manifest"`
	}{helperVersion, versions, []string{"video/mp4", "video/quicktime"}, []string{"first-frame-avif", "mp4-av1"}, "libsvtav1", "aac-lc", "mp4", "zscale+hable", digest, 1, buildManifest}
	return writeEnvelope(out, true, "", result)
}

func (e *engine) validateRuntimeClosure() error {
	object, digest, err := parseToolchainManifest(e.manifest)
	if err != nil || e.prefix == "" {
		return errors.New("invalid toolchain manifest")
	}
	objectPath := filepath.Join(e.prefix, filepath.FromSlash(object))
	info, err := os.Lstat(objectPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid libaom object")
	}
	actualDigest, err := digestFile(objectPath)
	if err != nil || actualDigest != digest {
		return errors.New("libaom digest mismatch")
	}
	return validateAOMDependency(e.ffmpeg, e.prefix, objectPath)
}

func parseToolchainManifest(manifest string) (string, string, error) {
	if !strings.HasPrefix(manifest, toolchainManifestPrefix) {
		return "", "", errors.New("invalid manifest prefix")
	}
	remainder := strings.TrimPrefix(manifest, toolchainManifestPrefix)
	lines := strings.Split(remainder, "\n")
	if len(lines) != 4 || lines[3] != "" || lines[2]+"\n" != toolchainManifestSuffix {
		return "", "", errors.New("invalid manifest fields")
	}
	const objectKey = "libaom_object="
	const digestKey = "libaom_object_sha256="
	if !strings.HasPrefix(lines[0], objectKey) || !strings.HasPrefix(lines[1], digestKey) {
		return "", "", errors.New("invalid libaom fields")
	}
	object := strings.TrimPrefix(lines[0], objectKey)
	digest := strings.TrimPrefix(lines[1], digestKey)
	parts := strings.Split(object, "/")
	if len(parts) != 2 || parts[0] != "lib" && parts[0] != "lib64" || !validVersionedAOM(parts[1]) || !validSHA256(digest) {
		return "", "", errors.New("invalid libaom identity")
	}
	return object, digest, nil
}

func validVersionedAOM(name string) bool {
	const prefix = "libaom.so."
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(name, prefix), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func validateAOMDependency(ffmpegPath, prefix, aomPath string) error {
	ffmpeg, err := elf.Open(ffmpegPath)
	if err != nil {
		return err
	}
	defer ffmpeg.Close()
	pinnedLibDir := filepath.Join(prefix, "lib")
	if err := requirePinnedRPATH(ffmpeg, pinnedLibDir); err != nil {
		return err
	}
	needed, err := ffmpeg.ImportedLibraries()
	if err != nil {
		return err
	}
	libavcodecName := ""
	for _, name := range needed {
		if strings.HasPrefix(name, "libavcodec.so.") {
			libavcodecName = name
			break
		}
	}
	if libavcodecName == "" {
		return errors.New("ffmpeg does not require libavcodec")
	}
	libavcodecPath, err := filepath.EvalSymlinks(filepath.Join(prefix, "lib", libavcodecName))
	if err != nil || filepath.Dir(libavcodecPath) != filepath.Join(prefix, "lib") {
		return errors.New("libavcodec escapes pinned prefix")
	}
	libavcodec, err := elf.Open(libavcodecPath)
	if err != nil {
		return err
	}
	defer libavcodec.Close()
	if err := requirePinnedRPATH(libavcodec, pinnedLibDir); err != nil {
		return err
	}
	aom, err := elf.Open(aomPath)
	if err != nil {
		return err
	}
	defer aom.Close()
	sonames, err := aom.DynString(elf.DT_SONAME)
	if err != nil || len(sonames) != 1 {
		return errors.New("invalid libaom SONAME")
	}
	loadedAOMPath, err := filepath.EvalSymlinks(filepath.Join(pinnedLibDir, sonames[0]))
	if err != nil || filepath.Clean(loadedAOMPath) != filepath.Clean(aomPath) {
		return errors.New("libaom SONAME does not resolve to pinned object")
	}
	loadedInfo, err := os.Stat(loadedAOMPath)
	pinnedInfo, pinnedErr := os.Stat(aomPath)
	if err != nil || pinnedErr != nil || !os.SameFile(pinnedInfo, loadedInfo) {
		return errors.New("libaom SONAME identity mismatch")
	}
	codecNeeded, err := libavcodec.ImportedLibraries()
	if err != nil {
		return err
	}
	for _, name := range codecNeeded {
		if name == sonames[0] {
			return nil
		}
	}
	return errors.New("libavcodec does not require pinned libaom SONAME")
}

func requirePinnedRPATH(file *elf.File, pinnedLibDir string) error {
	runpath, err := file.DynString(elf.DT_RUNPATH)
	if err != nil || len(runpath) != 0 {
		return errors.New("RUNPATH permits host fallback")
	}
	rpath, err := file.DynString(elf.DT_RPATH)
	if err != nil || len(rpath) != 1 || rpath[0] != pinnedLibDir {
		return errors.New("missing pinned RPATH")
	}
	return nil
}

func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("not regular")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (e *engine) checkTools() error {
	checks := []struct {
		path string
		args []string
		want []string
	}{
		{e.ffprobe, []string{"-version"}, []string{"ffprobe version 9.0.2"}},
		{e.ffmpeg, []string{"-version"}, []string{"ffmpeg version 9.0.2"}},
		{e.ffmpeg, []string{"-buildconf"}, []string{"--disable-autodetect", "--disable-network", "--disable-gpl", "--disable-nonfree", "--enable-libsvtav1", "--enable-libzimg", "--enable-libaom"}},
		{e.ffmpeg, []string{"-hide_banner", "-decoders"}, []string{" h264", " hevc", " aac", " av1"}},
		{e.ffmpeg, []string{"-hide_banner", "-demuxers"}, []string{"mov,mp4,m4a,3gp,3g2,mj2"}},
		{e.ffmpeg, []string{"-hide_banner", "-muxers"}, []string{" mp4", " avif"}},
		{e.ffmpeg, []string{"-hide_banner", "-encoders"}, []string{"libsvtav1", "libaom-av1", " aac"}},
		{e.ffmpeg, []string{"-hide_banner", "-filters"}, []string{"zscale", "tonemap"}},
	}
	for _, check := range checks {
		stdout, stderr, err := e.run.run(check.path, check.args, 1<<20)
		if err != nil {
			return err
		}
		text := string(stdout) + string(stderr)
		for _, want := range check.want {
			if !strings.Contains(text, want) {
				return fmt.Errorf("missing capability")
			}
		}
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
