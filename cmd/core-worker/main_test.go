//go:build linux

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kzkymur/no-more-cloud-photos/internal/animationprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/stillprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/transformcapability"
	"github.com/kzkymur/no-more-cloud-photos/internal/videoprocessor"
)

type startupProfileLoader struct {
	definitions []profile.Definition
	err         error
	called      bool
}

type loopFunc func(context.Context) error

func (f loopFunc) Run(ctx context.Context) error { return f(ctx) }

func TestRunWorkersCancelsSiblingOnFailure(t *testing.T) {
	fault := errors.New("worker failed")
	cancelled := make(chan struct{})
	err := runWorkers(context.Background(), loopFunc(func(context.Context) error { return fault }), loopFunc(func(ctx context.Context) error { <-ctx.Done(); close(cancelled); return nil }))
	if !errors.Is(err, fault) {
		t.Fatalf("error=%v", err)
	}
	<-cancelled
}

func TestRunWorkersRejectsUnexpectedCleanExit(t *testing.T) {
	err := runWorkers(context.Background(), loopFunc(func(context.Context) error { return nil }), loopFunc(func(ctx context.Context) error { <-ctx.Done(); return nil }))
	if err == nil {
		t.Fatal("clean early exit accepted")
	}
}

func (loader *startupProfileLoader) ClaimableTransformProfiles(context.Context) ([]profile.Definition, error) {
	loader.called = true
	return loader.definitions, loader.err
}

func TestValidateTransformStartupLoadsProfilesAndFailsClosed(t *testing.T) {
	loader := &startupProfileLoader{definitions: profile.BundledDefinitions()}
	still := stillprocessor.Capabilities{ProtocolVersion: 1, HelperVersion: "still-1", LibraryVersions: map[string]string{"libvips": "test"}, DecoderMIMETypes: []string{"image/dng"}, AVIFEncoder: "aom", ICCSHA256: testICC, Threads: 1}
	animation := animationprocessor.Capabilities{ProtocolVersion: 1, HelperVersion: "animation-1", LibraryVersions: map[string]string{"libwebp": "test"}, DecoderMIMETypes: []string{"image/gif", "image/webp"}, Encoders: []string{"animated-webp", "avif"}, ICCSHA256: testICC, Threads: 1, BuildManifest: animationprocessor.BuildManifest}
	video := videoprocessor.Capabilities{ProtocolVersion: 1, HelperVersion: "video-1", LibraryVersions: map[string]string{"ffmpeg": "test"}, DecoderMIMETypes: []string{"video/mp4", "video/quicktime"}, OutputKinds: []string{"first-frame-avif", "mp4-av1"}, VideoEncoder: "libsvtav1", AudioEncoder: "aac-lc", VideoMuxer: "mp4", ToneMap: "zscale+hable", ICCSHA256: testICC, Threads: 1, BuildManifest: videoprocessor.BuildManifest}

	if _, err := validateTransformStartup(context.Background(), loader, still, animation, video); !errors.Is(err, transformcapability.ErrIncompatible) {
		t.Fatalf("validateTransformStartup() error = %v", err)
	}
	if !loader.called {
		t.Fatal("startup did not load claimable profiles")
	}

	loader = &startupProfileLoader{err: job.ErrDatabaseUnavailable}
	if _, err := validateTransformStartup(context.Background(), loader, still, animation, video); !errors.Is(err, job.ErrDatabaseUnavailable) {
		t.Fatalf("validateTransformStartup() repository error = %v", err)
	}
}

const testICC = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
