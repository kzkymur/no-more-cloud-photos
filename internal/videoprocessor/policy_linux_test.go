//go:build linux

package videoprocessor

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestDefaultPolicyMatchesFixedCeilings(t *testing.T) {
	p, err := normalizePolicy(Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if p != DefaultPolicy() {
		t.Fatalf("policy = %#v", p)
	}
}

func TestPolicyRejectsEachCeilingPlusOne(t *testing.T) {
	tests := map[string]func(*Policy){
		"timeout":          func(p *Policy) { p.Timeout = MaxTimeout + time.Nanosecond },
		"address space":    func(p *Policy) { p.AddressSpaceBytes++ },
		"log":              func(p *Policy) { p.LogBytesPerStream++ },
		"video output":     func(p *Policy) { p.VideoOutputMaxBytes++ },
		"thumbnail output": func(p *Policy) { p.ThumbnailOutputMaxBytes++ },
		"generated output": func(p *Policy) { p.GeneratedOutputMaxBytes++ },
		"streams":          func(p *Policy) { p.Streams++ },
		"dimension":        func(p *Policy) { p.Dimension++ },
		"pixels":           func(p *Policy) { p.PixelsPerFrame++ },
		"duration":         func(p *Policy) { p.Duration++ },
		"fps":              func(p *Policy) { p.EffectiveFPSNumerator = 481; p.EffectiveFPSDenominator = 2 },
		"frames":           func(p *Policy) { p.Frames++ },
		"decoded pixels":   func(p *Policy) { p.CumulativeDecodedPixels++ },
		"channels":         func(p *Policy) { p.AudioChannels++ },
		"sample rate":      func(p *Policy) { p.AudioSampleRate++ },
		"threads":          func(p *Policy) { p.Threads++ },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := DefaultPolicy()
			mutate(&p)
			if _, err := normalizePolicy(p); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRationalComparisonDoesNotOverflow(t *testing.T) {
	if !rationalGreater(math.MaxInt64, 1, 240, 1) {
		t.Fatal("large rational accepted")
	}
	if rationalGreater(480, 2, 240, 1) {
		t.Fatal("exact boundary rejected")
	}
}

func TestPrimarySelection(t *testing.T) {
	video := []streamCandidate{{Index: 1}, {Index: 3, Default: true}, {Index: 2, Default: true}, {Index: 0, Default: true, AttachedPicture: true}}
	if got, ok := selectPrimary(video, true); !ok || got != 2 {
		t.Fatalf("video = %d, %v", got, ok)
	}
	audio := []streamCandidate{{Index: 7}, {Index: 5}}
	if got, ok := selectPrimary(audio, false); !ok || got != 5 {
		t.Fatalf("audio = %d, %v", got, ok)
	}
}

func TestRotationResolution(t *testing.T) {
	for _, value := range []int{0, 90, 180, 270} {
		got, err := resolveRotation(true, value, true, value)
		if err != nil || got != value {
			t.Fatalf("rotation %d = %d, %v", value, got, err)
		}
	}
	if _, err := resolveRotation(true, 90, true, 270); !errors.Is(err, ErrUnsupportedInput) {
		t.Fatalf("conflict = %v", err)
	}
	if _, err := resolveRotation(true, 45, false, 0); !errors.Is(err, ErrUnsupportedInput) {
		t.Fatalf("nonorthogonal = %v", err)
	}
}

func TestFitDimensions(t *testing.T) {
	tests := []struct {
		name         string
		w, h, edge   int
		even         bool
		wantW, wantH int
		wantErr      bool
	}{
		{"odd large video", 1931, 1087, 1920, true, 1920, 1080, false},
		{"small video floors odd", 63, 47, 1920, true, 62, 46, false},
		{"tiny video unsupported", 1, 2, 1920, true, 0, 0, true},
		{"thumbnail keeps odd", 63, 47, 640, false, 63, 47, false},
		{"thumbnail scales", 1001, 501, 640, false, 640, 320, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, h, err := fitDimensions(tc.w, tc.h, tc.edge, tc.even)
			if (err != nil) != tc.wantErr || w != tc.wantW || h != tc.wantH {
				t.Fatalf("got %dx%d, %v", w, h, err)
			}
		})
	}
}

func TestCheckedArithmeticBoundaries(t *testing.T) {
	if !checkedProductWithin(MaxPixelsPerFrame, 1, MaxPixelsPerFrame) || checkedProductWithin(MaxPixelsPerFrame, 2, MaxPixelsPerFrame) {
		t.Fatal("product boundary")
	}
	if checkedProductWithin(math.MaxUint64, 2, math.MaxUint64) {
		t.Fatal("product overflow")
	}
	limit := uint64(MaxGeneratedOutputBytes)
	if !checkedAddWithin(limit-1, 1, limit) || checkedAddWithin(limit, 1, limit) {
		t.Fatal("sum boundary")
	}
}
