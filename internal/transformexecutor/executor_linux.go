//go:build linux

// Package transformexecutor coordinates one leased transform attempt from the
// immutable original through durable storage publication and database commit.
package transformexecutor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/animationprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/processrunner"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/stillprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"github.com/kzkymur/no-more-cloud-photos/internal/videoprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/worker"
)

const (
	defaultAbortTimeout  = 5 * time.Second
	maxAuditBytes        = 1 << 20
	maxOriginalSizeBytes = int64(20 * 1024 * 1024 * 1024)
)

type Repository interface {
	BeginTarget(context.Context, string, string, string) error
	MarkTargetFailed(context.Context, string, string, string, job.FailureCode) error
	PublishRendition(context.Context, job.Rendition) (job.Publication, error)
}

type Temporary interface {
	UseWritableFile(func(*os.File) error) error
	Publish(context.Context, storage.Validation) (storage.ObjectInfo, error)
	Abort(context.Context) error
}

type Storage interface {
	OpenOriginal(context.Context, storage.OriginalKey, storage.Validation) (Original, error)
	BeginRendition(context.Context, storage.RenditionKey, storage.AttemptID) (Temporary, error)
}

type Original interface {
	UseReadOnlyFile(func(*os.File) error) error
	Close() error
}

type StillProcessor interface {
	Transform(context.Context, stillprocessor.Request) (stillprocessor.Result, error)
}

type AnimationProcessor interface {
	Inspect(context.Context, animationprocessor.InspectRequest) (animationprocessor.Inspection, error)
	Transform(context.Context, animationprocessor.Request) (animationprocessor.Result, error)
}

type VideoProcessor interface {
	Transform(context.Context, videoprocessor.Request) (videoprocessor.Result, error)
}

type Options struct {
	UUID         func() (string, error)
	AbortTimeout time.Duration
}

type Executor struct {
	repository Repository
	storage    Storage
	still      StillProcessor
	animation  AnimationProcessor
	video      VideoProcessor
	uuid       func() (string, error)
	abortAfter time.Duration
}

// StoreAdapter narrows the concrete filesystem store to the executor's testable
// storage contract.
type StoreAdapter struct{ Store *storage.Store }

func (adapter StoreAdapter) OpenOriginal(ctx context.Context, key storage.OriginalKey, validation storage.Validation) (Original, error) {
	return adapter.Store.OpenOriginalPinned(ctx, key, validation)
}

func (adapter StoreAdapter) BeginRendition(ctx context.Context, key storage.RenditionKey, attempt storage.AttemptID) (Temporary, error) {
	return adapter.Store.BeginRendition(ctx, key, attempt)
}

func New(repository Repository, store Storage, still StillProcessor, animation AnimationProcessor, video VideoProcessor, options Options) (*Executor, error) {
	if repository == nil || store == nil || still == nil || animation == nil || video == nil || options.AbortTimeout < 0 {
		return nil, job.ErrInvalid
	}
	if options.UUID == nil {
		options.UUID = newUUIDv4
	}
	if options.AbortTimeout == 0 {
		options.AbortTimeout = defaultAbortTimeout
	}
	return &Executor{repository: repository, storage: store, still: still, animation: animation, video: video, uuid: options.UUID, abortAfter: options.AbortTimeout}, nil
}

func (executor *Executor) Execute(ctx context.Context, lease job.Lease, limits worker.ExecutionLimits) error {
	if executor == nil || ctx == nil || !validLimits(limits) {
		return job.ErrInvalid
	}
	originalKey, attemptID, err := validateLease(lease)
	if err != nil {
		return err
	}
	expectedSHA256, err := decodeSHA256(lease.Original.SHA256)
	if err != nil {
		return err
	}
	original, err := executor.storage.OpenOriginal(ctx, originalKey, storage.Validation{ExpectedSize: lease.Original.SizeBytes, ExpectedSHA256: &expectedSHA256})
	if err != nil {
		return cancellationCause(ctx, err)
	}
	defer original.Close()
	generatedBytes := lease.GeneratedBytes
	for _, target := range lease.Targets {
		if err := executor.repository.BeginTarget(ctx, lease.ID, lease.Token, target.ID); err != nil {
			return cancellationCause(ctx, err)
		}
		recipe, err := targetRecipe(target.Profile, lease.Original.MIMEType)
		if err != nil {
			return executor.markFailed(ctx, lease, target, err)
		}
		local, size, err := executor.executeTarget(ctx, lease, target, recipe, original, originalKey, attemptID, generatedBytes)
		if err == nil {
			generatedBytes += size
			continue
		}
		err = cancellationCause(ctx, err)
		if !local || ctx.Err() != nil {
			return err
		}
		return executor.markFailed(ctx, lease, target, err)
	}
	return nil
}

func (executor *Executor) markFailed(ctx context.Context, lease job.Lease, target job.Target, failure error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	markErr := executor.repository.MarkTargetFailed(ctx, lease.ID, lease.Token, target.ID, failureCode(failure))
	if markErr != nil {
		return errors.Join(failure, cancellationCause(ctx, markErr))
	}
	return failure
}

type transformResult struct {
	mimeType      string
	extension     storage.RenditionExtension
	width, height int
	durationMS    *int64
	audit         processorAudit
}

type processorAudit struct {
	SchemaVersion       int                               `json:"schema_version"`
	Family              worker.Family                     `json:"family"`
	InputClassification animationprocessor.Classification `json:"input_classification,omitempty"`
	Result              any                               `json:"result"`
}

func (executor *Executor) executeTarget(ctx context.Context, lease job.Lease, target job.Target, recipe profile.Recipe, original Original, originalKey storage.OriginalKey, attemptID storage.AttemptID, generatedBytes int64) (local bool, size int64, returnErr error) {
	kind := recipe.SourceMode
	classification := animationprocessor.Classification("")
	if lease.Original.MIMEType == "image/webp" {
		var inspection animationprocessor.Inspection
		err := original.UseReadOnlyFile(func(input *os.File) error {
			var inspectErr error
			inspection, inspectErr = executor.animation.Inspect(ctx, animationprocessor.InspectRequest{Input: input, MIMEType: lease.Original.MIMEType})
			return inspectErr
		})
		if err != nil {
			return true, 0, mapProcessorError(ctx, err)
		}
		classification = inspection.Classification
		if classification == animationprocessor.ClassificationStatic {
			kind = profile.SourceStill
		} else if classification != animationprocessor.ClassificationAnimation {
			return true, 0, animationprocessor.ErrProcess
		}
	}

	renditionIDText, err := executor.uuid()
	if err != nil {
		return true, 0, fmt.Errorf("generate rendition ID: %w", err)
	}
	renditionID, err := storage.ParseRenditionID(renditionIDText)
	if err != nil {
		return true, 0, storage.ErrInvalidKey
	}
	extension := renditionExtension(recipe, kind)
	key, err := storage.NewRenditionKey(originalKey.OriginalID(), mustTargetID(target.ID), renditionID, extension)
	if err != nil {
		return true, 0, err
	}
	temporary, err := executor.storage.BeginRendition(ctx, key, attemptID)
	if err != nil {
		return true, 0, err
	}
	published := false
	defer func() {
		if published {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), executor.abortAfter)
		defer cancel()
		if abortErr := temporary.Abort(abortCtx); abortErr != nil {
			returnErr = errors.Join(returnErr, abortErr)
		}
	}()

	var transformed transformResult
	err = temporary.UseWritableFile(func(output *os.File) error {
		return original.UseReadOnlyFile(func(input *os.File) error {
			var processErr error
			switch kind {
			case profile.SourceStill:
				result, err := executor.still.Transform(ctx, stillprocessor.Request{Input: input, Output: output, MIMEType: lease.Original.MIMEType, Recipe: recipe})
				processErr = err
				transformed = transformResult{mimeType: result.OutputMIME, extension: storage.RenditionAVIF, width: result.Width, height: result.Height,
					audit: processorAudit{SchemaVersion: 1, Family: worker.FamilyStill, InputClassification: classification, Result: result}}
			case profile.SourceProbeAnimation:
				result, err := executor.animation.Transform(ctx, animationprocessor.Request{Input: input, Output: output, MIMEType: lease.Original.MIMEType, Recipe: recipe})
				processErr = err
				transformed = transformResult{mimeType: result.OutputMIME, extension: extension, width: result.Width, height: result.Height,
					audit: processorAudit{SchemaVersion: 1, Family: worker.FamilyAnimation, InputClassification: classification, Result: result}}
				if result.OutputMIME == "image/webp" {
					duration := result.Source.DurationMS
					transformed.durationMS = &duration
				}
			case profile.SourceVideo:
				result, err := executor.video.Transform(ctx, videoprocessor.Request{Input: input, Output: output, MIMEType: lease.Original.MIMEType, Recipe: recipe, GeneratedBytesBefore: generatedBytes, ExpectedVideoStreamIndex: lease.Original.PrimaryVideoStreamIndex})
				processErr = err
				transformed = transformResult{mimeType: result.OutputMIME, extension: extension, width: result.Width, height: result.Height,
					audit: processorAudit{SchemaVersion: 1, Family: worker.FamilyVideo, Result: result}}
				if result.OutputMIME == "video/mp4" {
					duration := result.OutputDurationUS / 1000
					transformed.durationMS = &duration
				}
			default:
				processErr = job.ErrInvariant
			}
			return mapProcessorError(ctx, processErr)
		})
	})
	if err != nil {
		return true, 0, err
	}
	if err := validateResult(transformed, extension); err != nil {
		return true, 0, err
	}
	audit, err := marshalAudit(transformed.audit)
	if err != nil {
		return true, 0, err
	}
	info, err := temporary.Publish(ctx, storage.Validation{ExpectedSize: -1})
	if err != nil {
		var publishError *storage.PublishError
		if errors.As(err, &publishError) && publishError.Published {
			published = true
		}
		return true, 0, err
	}
	published = true
	if info.Size < 0 || generatedBytes > math.MaxInt64-info.Size {
		return false, info.Size, job.ErrInvariant
	}
	width, height := transformed.width, transformed.height
	_, err = executor.repository.PublishRendition(ctx, job.Rendition{
		ID: renditionIDText, JobID: lease.ID, LeaseToken: lease.Token, TargetID: target.ID,
		OriginalID: lease.Original.ID, MediaID: lease.MediaID, ProfileID: target.Profile.ID,
		RelativePath: key.String(), MIMEType: transformed.mimeType, Width: &width, Height: &height,
		DurationMS: transformed.durationMS, SizeBytes: info.Size, SHA256: hex.EncodeToString(info.SHA256[:]), ProcessorAudit: audit,
	})
	if err != nil {
		return false, info.Size, cancellationCause(ctx, err)
	}
	return false, info.Size, nil
}

func validateLease(lease job.Lease) (storage.OriginalKey, storage.AttemptID, error) {
	video := lease.Original != nil && (lease.Original.MIMEType == "video/mp4" || lease.Original.MIMEType == "video/quicktime")
	if lease.Type != job.TypeTransform || lease.Original == nil || lease.ID == "" || lease.Token == "" || lease.MediaID == "" ||
		lease.Original.MediaID != lease.MediaID || lease.Original.SizeBytes < 0 || lease.Original.SizeBytes > maxOriginalSizeBytes || !validSHA256(lease.Original.SHA256) ||
		(lease.Original.Width == nil) != (lease.Original.Height == nil) ||
		(lease.Original.Width != nil && (*lease.Original.Width <= 0 || *lease.Original.Height <= 0)) ||
		(lease.Original.DurationMS != nil && *lease.Original.DurationMS < 0) ||
		(video && (lease.Original.PrimaryVideoStreamIndex == nil || *lease.Original.PrimaryVideoStreamIndex < 0)) ||
		(!video && lease.Original.PrimaryVideoStreamIndex != nil) || lease.GeneratedBytes < 0 || lease.Attempts <= 0 ||
		lease.MaxAttempts < lease.Attempts || len(lease.Targets) == 0 {
		return storage.OriginalKey{}, storage.AttemptID{}, job.ErrInvalid
	}
	if _, err := storage.ParseRenditionID(lease.ID); err != nil {
		return storage.OriginalKey{}, storage.AttemptID{}, job.ErrInvalid
	}
	if _, err := storage.ParseRenditionID(lease.MediaID); err != nil {
		return storage.OriginalKey{}, storage.AttemptID{}, job.ErrInvalid
	}
	attemptID, err := storage.ParseAttemptID(lease.Token)
	if err != nil {
		return storage.OriginalKey{}, storage.AttemptID{}, job.ErrInvalid
	}
	originalKey, err := storage.ParseOriginalKey(lease.Original.RelativePath)
	if err != nil || originalKey.OriginalID().String() != lease.Original.ID {
		return storage.OriginalKey{}, storage.AttemptID{}, job.ErrInvalid
	}
	expectedExtension, err := storage.OriginalExtensionForMIME(lease.Original.MIMEType)
	if err != nil {
		return storage.OriginalKey{}, storage.AttemptID{}, job.ErrInvalid
	}
	expectedKey, err := storage.NewOriginalKey(originalKey.OriginalID(), expectedExtension)
	if err != nil || expectedKey.String() != originalKey.String() {
		return storage.OriginalKey{}, storage.AttemptID{}, job.ErrInvalid
	}
	seenTargets := make(map[string]struct{}, len(lease.Targets))
	for _, target := range lease.Targets {
		if target.Status != job.TargetPending || target.Attempts < 0 || target.Attempts >= lease.Attempts {
			return storage.OriginalKey{}, storage.AttemptID{}, job.ErrInvalid
		}
		if _, err := storage.ParseJobTargetID(target.ID); err != nil {
			return storage.OriginalKey{}, storage.AttemptID{}, job.ErrInvalid
		}
		if _, duplicate := seenTargets[target.ID]; duplicate {
			return storage.OriginalKey{}, storage.AttemptID{}, job.ErrInvalid
		}
		seenTargets[target.ID] = struct{}{}
	}
	return originalKey, attemptID, nil
}

func targetRecipe(pinned job.Profile, mimeType string) (profile.Recipe, error) {
	definition := profile.Definition{ID: pinned.ID, Key: pinned.Key, Version: pinned.Version,
		Processor: pinned.Processor, ParametersSchemaVersion: pinned.ParametersSchemaVersion,
		InputMIMETypes: pinned.InputMIMETypes, Parameters: pinned.Parameters}
	if err := profile.ValidateDraft(definition); err != nil {
		return profile.Recipe{}, job.ErrInvalid
	}
	parameters, err := profile.DecodeParameters(pinned.Parameters)
	if err != nil {
		return profile.Recipe{}, job.ErrInvalid
	}
	recipe, ok := parameters.Recipes[mimeType]
	if !ok {
		return profile.Recipe{}, job.ErrInvalid
	}
	return recipe, nil
}

func renditionExtension(recipe profile.Recipe, kind profile.SourceMode) storage.RenditionExtension {
	if kind == profile.SourceStill || recipe.FramePolicy == profile.FrameFirst {
		return storage.RenditionAVIF
	}
	if kind == profile.SourceProbeAnimation {
		return storage.RenditionAnimatedWebP
	}
	return storage.RenditionMP4
}

func validateResult(result transformResult, expected storage.RenditionExtension) error {
	if result.width <= 0 || result.height <= 0 || result.extension != expected {
		return job.ErrInvariant
	}
	switch expected {
	case storage.RenditionAVIF:
		if result.mimeType != "image/avif" {
			return job.ErrInvariant
		}
	case storage.RenditionAnimatedWebP:
		if result.mimeType != "image/webp" {
			return job.ErrInvariant
		}
	case storage.RenditionMP4:
		if result.mimeType != "video/mp4" {
			return job.ErrInvariant
		}
	default:
		return job.ErrInvariant
	}
	return nil
}

func marshalAudit(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxAuditBytes {
		return nil, job.ErrInvariant
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(encoded, &object) != nil || len(object) == 0 {
		return nil, job.ErrInvariant
	}
	return encoded, nil
}

func mapProcessorError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	switch {
	case errors.Is(err, stillprocessor.ErrTimeout), errors.Is(err, animationprocessor.ErrTimeout), errors.Is(err, videoprocessor.ErrTimeout):
		return errors.Join(processrunner.ErrTimeout, err)
	case errors.Is(err, stillprocessor.ErrLogOutputLimit), errors.Is(err, animationprocessor.ErrLogOutputLimit), errors.Is(err, videoprocessor.ErrLogOutputLimit):
		return errors.Join(processrunner.ErrOutputLimit, err)
	default:
		return err
	}
}

func cancellationCause(ctx context.Context, fallback error) error {
	if ctx != nil && ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return fallback
}

func failureCode(err error) job.FailureCode {
	if errors.Is(err, processrunner.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return job.FailureProcessTimeout
	}
	if errors.Is(err, processrunner.ErrOutputLimit) {
		return job.FailureProcessOutputLimit
	}
	return job.FailureProcessFailed
}

func validLimits(limits worker.ExecutionLimits) bool {
	return limits.Threads > 0 && limits.OutputBytesPerStream > 0 && limits.StillTimeoutCeiling > 0 && limits.AnimationTimeoutCeiling > 0 && limits.VideoTimeoutCeiling > 0
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func decodeSHA256(value string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(digest) {
		return digest, job.ErrInvalid
	}
	copy(digest[:], decoded)
	return digest, nil
}

func mustTargetID(value string) storage.JobTargetID {
	id, _ := storage.ParseJobTargetID(value)
	return id
}

func newUUIDv4() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	encoded := hex.EncodeToString(id[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:]), nil
}
