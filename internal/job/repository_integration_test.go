package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/animationprocessor"
	dbmigration "github.com/kzkymur/no-more-cloud-photos/internal/database"
	profiledefinition "github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/stillprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"github.com/kzkymur/no-more-cloud-photos/internal/transformcapability"
	"github.com/kzkymur/no-more-cloud-photos/internal/videoprocessor"
)

func TestClaimIntegrationOrderingTypesAvailabilityAndHydration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{})
	ctx := context.Background()
	future, _ := insertTransformJobAt(t, pool, 3, 1, `clock_timestamp()+interval '1 hour'`)
	first, _ := insertTransformJob(t, pool, 3, 1)
	purge := insertPurgeJob(t, pool, 3, `clock_timestamp()`)

	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil || lease.ID != first || lease.Type != TypeTransform || lease.Token == "" || lease.Attempts != 1 || !lease.StartedAt.Before(lease.LeaseExpiresAt) {
		t.Fatalf("first claim = %+v, %v", lease, err)
	}
	if len(lease.Targets) != 1 {
		t.Fatalf("transform targets = %+v", lease.Targets)
	}
	if lease.Original == nil || lease.Original.ID == "" || lease.Original.MediaID != lease.MediaID ||
		lease.Original.RelativePath == "" || lease.Original.MIMEType != "image/jpeg" || lease.Original.SizeBytes != 1 ||
		len(lease.Original.SHA256) != 64 || lease.Original.Width == nil || lease.Original.Height == nil {
		t.Fatalf("transform original = %+v", lease.Original)
	}
	if _, err := repository.Claim(ctx, []Type{TypeTransform}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("future/unsupported claim error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp() WHERE id=$1`, future); err != nil {
		t.Fatal(err)
	}
	if lease, err := repository.Claim(ctx, []Type{TypeTransform}); err != nil || lease.ID != future {
		t.Fatalf("exact-boundary claim = %+v, %v", lease, err)
	}
	if _, err := repository.Claim(ctx, []Type{TypePurge}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("purge claim boundary error = %v", err)
	}
	var purgeStatus Status
	var purgeStarted *time.Time
	if err := pool.QueryRow(ctx, `SELECT status,started_at FROM jobs WHERE id=$1`, purge).Scan(&purgeStatus, &purgeStarted); err != nil || purgeStatus != StatusQueued || purgeStarted != nil {
		t.Fatalf("purge changed by generic claim: status=%s started=%v err=%v", purgeStatus, purgeStarted, err)
	}
	if _, err := repository.Claim(ctx, []Type{"unsupported"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unregistered API type error = %v", err)
	}
}

func TestClaimableTransformProfilesIntegration(t *testing.T) {
	t.Run("active and retired queued profiles", func(t *testing.T) {
		pool, repository := integrationRepository(t, Options{})
		ctx := context.Background()
		var standardID string
		if err := pool.QueryRow(ctx, `SELECT id::text FROM profiles WHERE key='standard'`).Scan(&standardID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='retired' WHERE key='standard'`); err != nil {
			t.Fatal(err)
		}
		mediaID, originalID := insertPublicationMedia(t, pool)
		insertTransformForProfile(t, pool, mediaID, originalID, standardID)

		definitions, err := repository.ClaimableTransformProfiles(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(definitions) != 2 || definitions[0].Key != "standard" || definitions[1].Key != "thumbnail" {
			t.Fatalf("claimable definitions = %#v", definitions)
		}
		for _, definition := range definitions {
			if err := profiledefinition.ValidateDraft(definition); err != nil {
				t.Fatalf("returned definition %s is invalid: %v", definition.Key, err)
			}
		}

		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='retired' WHERE key='thumbnail'`); err != nil {
			t.Fatal(err)
		}
		definitions, err = repository.ClaimableTransformProfiles(ctx)
		if err != nil || len(definitions) != 1 || definitions[0].Key != "standard" {
			t.Fatalf("retired queued definitions = %#v, %v", definitions, err)
		}
		if _, err := repository.Claim(ctx, []Type{TypeTransform}); err != nil {
			t.Fatal(err)
		}
		definitions, err = repository.ClaimableTransformProfiles(ctx)
		if err != nil || len(definitions) != 1 || definitions[0].Key != "standard" {
			t.Fatalf("retired running definitions = %#v, %v", definitions, err)
		}
	})

	t.Run("exact definition validation", func(t *testing.T) {
		pool, repository := integrationRepository(t, Options{})
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `ALTER TABLE profiles DISABLE TRIGGER USER;
			UPDATE profiles SET parameters=jsonb_set(parameters,'{recipes,image/jpeg,still_output,quality}','0') WHERE key='standard';
			ALTER TABLE profiles ENABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.ClaimableTransformProfiles(ctx); !errors.Is(err, ErrInvariant) {
			t.Fatalf("ClaimableTransformProfiles() invalid definition error = %v", err)
		}
	})
}

func TestBoundTransformClaimsExcludeProfilesAddedAfterStartupIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{})
	ctx := context.Background()
	definitions, err := repository.ClaimableTransformProfiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	still, animation, video := integrationCapabilities()
	envelope, err := transformcapability.ValidateEnvelope(definitions, still, animation, video)
	if err != nil {
		t.Fatal(err)
	}
	claimer, err := repository.Repository.BindTransformClaims(envelope)
	if err != nil {
		t.Fatal(err)
	}

	unsupportedProfileID := ensureVersionProfile(t, pool, "post_start", 1, true)
	unsupportedMediaID, unsupportedOriginalID := insertPublicationMedia(t, pool)
	unsupportedJobID, _ := insertTransformForProfile(t, pool, unsupportedMediaID, unsupportedOriginalID, unsupportedProfileID)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, unsupportedJobID); err != nil {
		t.Fatal(err)
	}
	supportedMediaID, supportedOriginalID := insertPublicationMediaWithSHA(t, pool, strings.Repeat("d", 64))
	supportedJobID, _ := insertTransformForProfile(t, pool, supportedMediaID, supportedOriginalID, definitions[0].ID)

	lease, err := claimer.Claim(ctx, []Type{TypeTransform})
	if err != nil || lease.ID != supportedJobID {
		t.Fatalf("bound claim = %+v, %v; want supported job %s", lease, err, supportedJobID)
	}
	if _, err := claimer.Claim(ctx, []Type{TypeTransform}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("unsupported post-start profile was claimable: %v", err)
	}
	var status Status
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,attempts FROM jobs WHERE id=$1`, unsupportedJobID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != StatusQueued || attempts != 0 {
		t.Fatalf("unsupported job changed: status=%s attempts=%d", status, attempts)
	}
}

func integrationCapabilities() (stillprocessor.Capabilities, animationprocessor.Capabilities, videoprocessor.Capabilities) {
	still := stillprocessor.Capabilities{
		ProtocolVersion: stillprocessor.ProtocolVersion, HelperVersion: "test", LibraryVersions: map[string]string{"libvips": "test"},
		DecoderMIMETypes: []string{"image/bmp", "image/dng", "image/heic", "image/heif", "image/jpeg", "image/png", "image/webp", "image/x-canon-cr2", "image/x-canon-cr3", "image/x-fuji-raf", "image/x-nikon-nef", "image/x-olympus-orf", "image/x-panasonic-rw2", "image/x-sony-arw"},
		AVIFEncoder:      "aom", ICCSHA256: strings.Repeat("a", 64), Threads: stillprocessor.RequiredThreads,
	}
	animation := animationprocessor.Capabilities{
		ProtocolVersion: animationprocessor.ProtocolVersion, HelperVersion: "test", LibraryVersions: map[string]string{"libwebp": "test"},
		DecoderMIMETypes: []string{"image/gif", "image/webp"}, Encoders: []string{"animated-webp", "avif"},
		ICCSHA256: still.ICCSHA256, Threads: animationprocessor.RequiredThreads, BuildManifest: animationprocessor.BuildManifest,
	}
	video := videoprocessor.Capabilities{
		ProtocolVersion: videoprocessor.ProtocolVersion, HelperVersion: "test", LibraryVersions: map[string]string{"ffmpeg": "test"},
		DecoderMIMETypes: []string{"video/mp4", "video/quicktime"}, OutputKinds: []string{"first-frame-avif", "mp4-av1"},
		VideoEncoder: "libsvtav1", AudioEncoder: "aac-lc", VideoMuxer: "mp4", ToneMap: "zscale+hable",
		ICCSHA256: still.ICCSHA256, Threads: videoprocessor.RequiredThreads, BuildManifest: videoprocessor.BuildManifest,
	}
	return still, animation, video
}

func TestClaimRejectsOriginalExtensionMIMEContradictionIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{})
	ctx := context.Background()
	mediaID, originalID := newTestUUID(t), newTestUUID(t)
	if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	badPath := "originals/" + originalID[:2] + "/" + originalID + "/original.png"
	if _, err := pool.Exec(ctx, `INSERT INTO originals
		(id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
		VALUES ($1,$2,$3,$4,'image/jpeg',1,1,1)`, originalID, mediaID, strings.Repeat("d", 64), badPath); err != nil {
		t.Fatal(err)
	}
	var profileID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM profiles WHERE key='standard' AND version=1`).Scan(&profileID); err != nil {
		t.Fatal(err)
	}
	jobID, _ := insertTransformForProfile(t, pool, mediaID, originalID, profileID)
	if _, err := repository.Claim(ctx, []Type{TypeTransform}); !errors.Is(err, ErrInvariant) {
		t.Fatalf("Claim() extension/MIME contradiction = %v", err)
	}
	var status Status
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,attempts FROM jobs WHERE id=$1`, jobID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != StatusQueued || attempts != 0 {
		t.Fatalf("failed hydration committed status=%s attempts=%d", status, attempts)
	}
}

func TestClaimHydratesAndValidatesPrimaryVideoStreamIntegration(t *testing.T) {
	tests := []struct {
		name, mime, sourceMetadata string
		want                       *int
	}{
		{name: "MP4 hydration", mime: "video/mp4", sourceMetadata: `{"primary_stream":2}`, want: intPointer(2)},
		{name: "QuickTime hydration", mime: "video/quicktime", sourceMetadata: `{"primary_stream":0}`, want: intPointer(0)},
		{name: "video missing", mime: "video/mp4", sourceMetadata: `{}`},
		{name: "video malformed", mime: "video/mp4", sourceMetadata: `{"primary_stream":"0"}`},
		{name: "video negative", mime: "video/mp4", sourceMetadata: `{"primary_stream":-1}`},
		{name: "non-video unexpectedly set", mime: "image/jpeg", sourceMetadata: `{"primary_stream":0}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool, repository := integrationRepository(t, Options{})
			jobID := insertPrimaryStreamTransform(t, pool, test.mime, test.sourceMetadata)
			lease, err := repository.Claim(context.Background(), []Type{TypeTransform})
			if test.want != nil {
				if err != nil || lease.Original == nil || lease.Original.PrimaryVideoStreamIndex == nil || *lease.Original.PrimaryVideoStreamIndex != *test.want {
					t.Fatalf("Claim() = %+v, %v", lease, err)
				}
				return
			}
			if !errors.Is(err, ErrInvariant) {
				t.Fatalf("Claim() error = %v, want ErrInvariant", err)
			}
			var status Status
			var attempts int
			if err := pool.QueryRow(context.Background(), `SELECT status,attempts FROM jobs WHERE id=$1`, jobID).Scan(&status, &attempts); err != nil {
				t.Fatal(err)
			}
			if status != StatusQueued || attempts != 0 {
				t.Fatalf("failed hydration committed status=%s attempts=%d", status, attempts)
			}
		})
	}
}

func TestClaimIntegrationExclusivitySkipLockedAndReturnsCommitted(t *testing.T) {
	pool, repository := integrationRepository(t, Options{})
	ctx := context.Background()
	lockedID, _ := insertTransformJob(t, pool, 3, 1)
	nextID, _ := insertTransformJobAt(t, pool, 3, 1, `clock_timestamp()+interval '1 microsecond'`)
	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(ctx)
	if _, err := locker.Exec(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, lockedID); err != nil {
		t.Fatal(err)
	}
	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lease, err := repository.Claim(claimCtx, []Type{TypeTransform})
	if err != nil || lease.ID != nextID {
		t.Fatalf("skip-locked claim = %+v, %v", lease, err)
	}
	if _, err := repository.Claim(claimCtx, []Type{TypeTransform}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("all candidates locked/running error = %v", err)
	}
	if err := locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	lockCtx, lockCancel := context.WithTimeout(ctx, time.Second)
	defer lockCancel()
	committed, err := pool.Begin(lockCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer committed.Rollback(ctx)
	if _, err := committed.Exec(lockCtx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE NOWAIT`, nextID); err != nil {
		t.Fatalf("Claim returned before commit/lock release: %v", err)
	}

	if lease, err := repository.Claim(ctx, []Type{TypeTransform}); err != nil || lease.ID != lockedID {
		t.Fatalf("released candidate claim = %+v, %v", lease, err)
	}

	onlyID, _ := insertTransformJob(t, pool, 3, 1)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			lease, err := repository.Claim(ctx, []Type{TypeTransform})
			if err == nil && lease.ID != onlyID {
				err = fmt.Errorf("claimed %s, want %s", lease.ID, onlyID)
			}
			results <- err
		}()
	}
	close(start)
	firstErr, secondErr := <-results, <-results
	if (firstErr == nil) == (secondErr == nil) || !(errors.Is(firstErr, ErrNoWork) || errors.Is(secondErr, ErrNoWork)) {
		t.Fatalf("two-worker single-job results = %v, %v", firstErr, secondErr)
	}
}

func TestHeartbeatIntegrationCASClockAndExactExpiry(t *testing.T) {
	pool, repository := integrationRepository(t, Options{LeaseDuration: 30 * time.Second, Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	jobID, _ := insertTransformJob(t, pool, 4, 1)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Heartbeat(ctx, jobID, newTestUUID(t)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong token heartbeat = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	expiry, err := repository.Heartbeat(ctx, jobID, lease.Token)
	if err != nil {
		t.Fatal(err)
	}
	var deltaSeconds float64
	if err := pool.QueryRow(ctx, `SELECT EXTRACT(epoch FROM lease_expires_at-clock_timestamp()) FROM jobs WHERE id=$1`, jobID).Scan(&deltaSeconds); err != nil {
		t.Fatal(err)
	}
	if deltaSeconds < 28 || deltaSeconds > 31 || expiry.IsZero() {
		t.Fatalf("heartbeat extension from DB clock = %f seconds, expiry %s", deltaSeconds, expiry)
	}
	var beforeDisconnect time.Time
	if err := pool.QueryRow(ctx, `SELECT lease_expires_at FROM jobs WHERE id=$1`, jobID).Scan(&beforeDisconnect); err != nil {
		t.Fatal(err)
	}
	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Exec(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, jobID); err != nil {
		t.Fatal(err)
	}
	heartbeatResult := make(chan error, 1)
	go func() {
		_, err := repository.Heartbeat(ctx, jobID, lease.Token)
		heartbeatResult <- err
	}()
	var heartbeatPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
			WHERE pid<>pg_backend_pid() AND state='active' AND query LIKE 'UPDATE jobs SET lease_expires_at=clock_timestamp()+%'
			ORDER BY query_start DESC LIMIT 1`).Scan(&heartbeatPID)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if heartbeatPID == 0 {
		t.Fatal("blocked heartbeat backend was not observed")
	}
	var terminated bool
	if err := pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, heartbeatPID).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate heartbeat backend = %v, %v", terminated, err)
	}
	if err := <-heartbeatResult; !errors.Is(err, ErrDatabaseUnavailable) {
		t.Fatalf("disconnected heartbeat error = %v", err)
	}
	if err := locker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var afterDisconnect time.Time
	if err := pool.QueryRow(ctx, `SELECT lease_expires_at FROM jobs WHERE id=$1`, jobID).Scan(&afterDisconnect); err != nil {
		t.Fatal(err)
	}
	if !afterDisconnect.Equal(beforeDisconnect) {
		t.Fatalf("disconnected heartbeat changed expiry: before=%s after=%s", beforeDisconnect, afterDisconnect)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Heartbeat(ctx, jobID, lease.Token); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("exact-expiry heartbeat = %v", err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired owner mutation = %v", err)
	}
	if count, err := repository.ReclaimExpired(ctx); err != nil || count != 1 {
		t.Fatalf("reclaim exact-expiry job = %d, %v", count, err)
	}
	reclaimed, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Heartbeat(ctx, jobID, lease.Token); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old-token heartbeat = %v", err)
	}
	if _, err := repository.Heartbeat(ctx, jobID, reclaimed.Token); err != nil {
		t.Fatalf("new-token heartbeat = %v", err)
	}
}

func TestTargetMutationRevalidatesLeaseAfterBlockedWorkIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{LeaseDuration: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	jobID, targets := insertTransformJob(t, pool, 3, 1)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM job_targets WHERE id=$1 FOR UPDATE`, targets[0]); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- repository.BeginTarget(ctx, jobID, lease.Token, targets[0]) }()
	var mutationPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
			WHERE pid<>pg_backend_pid() AND state='active' AND query LIKE 'UPDATE job_targets AS jt SET attempts=%'
			ORDER BY query_start DESC LIMIT 1`).Scan(&mutationPID)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mutationPID == 0 {
		t.Fatal("blocked target mutation was not observed")
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		var expired bool
		if err := pool.QueryRow(ctx, `SELECT lease_expires_at<=clock_timestamp() FROM jobs WHERE id=$1`, jobID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lease did not expire while target mutation was blocked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("post-work expiry mutation error = %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("blocked target mutation did not return: %v", ctx.Err())
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM job_targets WHERE id=$1`, targets[0]).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("expired target mutation committed attempts=%d err=%v", attempts, err)
	}
}

func TestTerminalTransitionRevalidatesAfterLeaseExpiresDuringWorkIntegration(t *testing.T) {
	jitterStarted := make(chan struct{})
	releaseJitter := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseJitter) })
	pool, repository := integrationRepository(t, Options{
		LeaseDuration: time.Second,
		Jitter: func(time.Duration) time.Duration {
			close(jitterStarted)
			<-releaseJitter
			return 0
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	jobID, _ := insertTransformJob(t, pool, 3, 1)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed) }()
	select {
	case <-jitterStarted:
	case <-ctx.Done():
		t.Fatalf("terminal transition did not reach blocked work: %v", ctx.Err())
	}
	for {
		var expired bool
		if err := pool.QueryRow(ctx, `SELECT lease_expires_at<=clock_timestamp() FROM jobs WHERE id=$1`, jobID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("lease did not expire during terminal work: %v", ctx.Err())
		}
	}
	releaseOnce.Do(func() { close(releaseJitter) })
	select {
	case err := <-result:
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("post-work terminal expiry error = %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("terminal transition did not return: %v", ctx.Err())
	}
	var status Status
	var retainedToken string
	var errorCode *string
	if err := pool.QueryRow(ctx, `SELECT status,lease_token::text,error_code FROM jobs WHERE id=$1`, jobID).Scan(&status, &retainedToken, &errorCode); err != nil {
		t.Fatal(err)
	}
	if status != StatusRunning || retainedToken != lease.Token || errorCode != nil {
		t.Fatalf("expired terminal transition committed: status=%s token=%s error=%v", status, retainedToken, errorCode)
	}
}

func TestCompleteSucceededRejectsExpiredTransformAndPurgeIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{})
	ctx := context.Background()
	jobID, targets := insertTransformJob(t, pool, 3, 1)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteSucceeded(ctx, jobID, lease.Token); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired transform completion = %v", err)
	}
	var transformStatus Status
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&transformStatus); err != nil || transformStatus != StatusRunning {
		t.Fatalf("expired transform status = %s, %v", transformStatus, err)
	}

	purgeID := newTestUUID(t)
	purgeMediaID := newTestUUID(t)
	if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source,deleted_at,purge_after)
		VALUES ($1,'image/jpeg','unknown',clock_timestamp(),clock_timestamp())`, purgeMediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts)
		VALUES ($1,'purge',$2,'queued',3)`, purgeID, purgeMediaID); err != nil {
		t.Fatal(err)
	}
	var deletedBefore, purgeBefore time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at,purge_after FROM media WHERE id=$1`, purgeMediaID).Scan(&deletedBefore, &purgeBefore); err != nil {
		t.Fatal(err)
	}
	purgeToken := newTestUUID(t)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,lease_token=$2,
		lease_expires_at=clock_timestamp()+interval '1 minute',started_at=clock_timestamp() WHERE id=$1`, purgeID, purgeToken); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteSucceeded(ctx, purgeID, purgeToken); !errors.Is(err, ErrConflict) {
		t.Fatalf("generic purge completion = %v", err)
	}
	var purgeStatus Status
	var retainedToken string
	var finished *time.Time
	if err := pool.QueryRow(ctx, `SELECT status,lease_token::text,finished_at FROM jobs WHERE id=$1`, purgeID).Scan(&purgeStatus, &retainedToken, &finished); err != nil {
		t.Fatal(err)
	}
	if purgeStatus != StatusRunning || retainedToken != purgeToken || finished != nil {
		t.Fatalf("purge changed by generic completion: status=%s token=%s finished=%v", purgeStatus, retainedToken, finished)
	}
	var deletedAfter, purgeAfter time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at,purge_after FROM media WHERE id=$1`, purgeMediaID).Scan(&deletedAfter, &purgeAfter); err != nil {
		t.Fatal(err)
	}
	if !deletedAfter.Equal(deletedBefore) || !purgeAfter.Equal(purgeBefore) {
		t.Fatalf("purge completion changed Media: deleted %s→%s purge %s→%s", deletedBefore, deletedAfter, purgeBefore, purgeAfter)
	}
}

func TestTransformTargetsRetryAndCompletionIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	jobID, targets := insertTransformJob(t, pool, 3, 2)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil || lease.ID != jobID || len(lease.Targets) != 2 {
		t.Fatalf("transform claim = %+v, %v", lease, err)
	}
	for _, target := range lease.Targets {
		if target.Profile.ID == "" || target.Profile.Key == "" || target.Profile.Version <= 0 || target.Profile.Processor == "" ||
			target.Profile.ParametersSchemaVersion <= 0 || len(target.Profile.InputMIMETypes) == 0 || len(target.Profile.Parameters) == 0 {
			t.Fatalf("incomplete pinned profile hydration: %+v", target.Profile)
		}
	}
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[0]); err != nil {
		t.Fatal(err)
	}
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[0]); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate begin = %v", err)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM job_targets WHERE id=$1`, targets[0]).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("target begin attempts = %d, %v", attempts, err)
	}
	firstPublication, err := repository.PublishRendition(ctx, testRendition(t, lease, targets[0]))
	if err != nil || !firstPublication.Current || firstPublication.JobFinished {
		t.Fatalf("partial publication = %+v, %v", firstPublication, err)
	}
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[1]); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkTargetFailed(ctx, jobID, lease.Token, targets[1], FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteSucceeded(ctx, jobID, lease.Token); !errors.Is(err, ErrConflict) {
		t.Fatalf("partial completion error = %v", err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	retry, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil || len(retry.Targets) != 1 || retry.Targets[0].ID != targets[1] || retry.Targets[0].Status != TargetPending || retry.Targets[0].ErrorCode != nil || retry.GeneratedBytes != 1 {
		t.Fatalf("retry hydration = %+v, %v", retry, err)
	}
	if err := repository.BeginTarget(ctx, jobID, retry.Token, targets[1]); err != nil {
		t.Fatal(err)
	}
	finalPublication, err := repository.PublishRendition(ctx, testRendition(t, retry, targets[1]))
	if err != nil || !finalPublication.Current || !finalPublication.JobFinished {
		t.Fatalf("final publication = %+v, %v", finalPublication, err)
	}
	var status Status
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&status); err != nil || status != StatusSucceeded {
		t.Fatalf("completed status = %s, %v", status, err)
	}
	var eventCount int
	var positions []int64
	var latestPayload []byte
	if err := pool.QueryRow(ctx, `SELECT count(*),array_agg(position ORDER BY position),
		(array_agg(payload ORDER BY position DESC))[1]::text FROM change_events`).Scan(&eventCount, &positions, &latestPayload); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 || len(positions) != 2 || positions[0] != 1 || positions[1] != 2 ||
		strings.Contains(string(latestPayload), `"jobs"`) || !strings.Contains(string(latestPayload), `"current_renditions"`) ||
		!strings.Contains(string(latestPayload), `https://files.example.test/files/renditions/`) {
		t.Fatalf("publication events count=%d positions=%v payload=%s", eventCount, positions, latestPayload)
	}
}

func TestPublishRenditionRejectsInvalidOwnershipAndStateIntegration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Rendition, *pgxpool.Pool)
		want   error
	}{
		{name: "wrong token", mutate: func(value *Rendition, _ *pgxpool.Pool) { value.LeaseToken = newTestUUID(t) }, want: ErrLeaseLost},
		{name: "wrong profile", mutate: func(value *Rendition, _ *pgxpool.Pool) { value.ProfileID = newTestUUID(t) }, want: ErrConflict},
		{name: "exact expiry", mutate: func(value *Rendition, pool *pgxpool.Pool) {
			if _, err := pool.Exec(context.Background(), `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, value.JobID); err != nil {
				t.Fatal(err)
			}
		}, want: ErrLeaseLost},
		{name: "deleted media", mutate: func(value *Rendition, pool *pgxpool.Pool) {
			if _, err := pool.Exec(context.Background(), `UPDATE media SET deleted_at=clock_timestamp() WHERE id=$1`, value.MediaID); err != nil {
				t.Fatal(err)
			}
		}, want: ErrConflict},
		{name: "empty processor audit", mutate: func(value *Rendition, _ *pgxpool.Pool) {
			value.ProcessorAudit = []byte(`{}`)
		}, want: ErrInvalid},
		{name: "malformed processor audit", mutate: func(value *Rendition, _ *pgxpool.Pool) {
			value.ProcessorAudit = []byte(`{"schema_version":1,"family":"still","result":{}}`)
		}, want: ErrInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool, repository := integrationRepository(t, Options{})
			jobID, targets := insertTransformJob(t, pool, 3, 1)
			lease, err := repository.Claim(context.Background(), []Type{TypeTransform})
			if err != nil {
				t.Fatal(err)
			}
			candidate := testRendition(t, lease, targets[0])
			test.mutate(&candidate, pool)
			if _, err := repository.PublishRendition(context.Background(), candidate); !errors.Is(err, test.want) {
				t.Fatalf("PublishRendition(%s) = %v, want %v", jobID, err, test.want)
			}
			var renditions, events int
			if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM renditions),(SELECT count(*) FROM change_events)`).Scan(&renditions, &events); err != nil {
				t.Fatal(err)
			}
			if renditions != 0 || events != 0 {
				t.Fatalf("rejected publication persisted renditions=%d events=%d", renditions, events)
			}
		})
	}

	t.Run("duplicate", func(t *testing.T) {
		pool, repository := integrationRepository(t, Options{})
		_, targets := insertTransformJob(t, pool, 3, 1)
		lease, err := repository.Claim(context.Background(), []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		candidate := testRendition(t, lease, targets[0])
		if _, err := repository.PublishRendition(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.PublishRendition(context.Background(), candidate); !errors.Is(err, ErrLeaseLost) && !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate publication = %v", err)
		}
		var count int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM renditions`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("rendition count=%d err=%v", count, err)
		}
	})

	t.Run("JPEG still cannot publish canonical MP4", func(t *testing.T) {
		pool, repository := integrationRepository(t, Options{})
		_, targets := insertTransformJob(t, pool, 3, 1)
		lease, err := repository.Claim(context.Background(), []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		candidate := testRendition(t, lease, targets[0])
		candidate.MIMEType = "video/mp4"
		candidate.RelativePath = strings.TrimSuffix(candidate.RelativePath, ".avif") + ".mp4"
		if _, err := repository.PublishRendition(context.Background(), candidate); !errors.Is(err, ErrConflict) {
			t.Fatalf("JPEG/MP4 publication = %v, want ErrConflict", err)
		}
		assertPublicationCounts(t, pool, 0, 0, 0)
	})

	t.Run("draft pinned profile", func(t *testing.T) {
		pool, repository := integrationRepository(t, Options{})
		mediaID, originalID := insertPublicationMedia(t, pool)
		profileID := ensureVersionProfile(t, pool, "draft_target", 1, false)
		_, targetID := insertTransformForProfile(t, pool, mediaID, originalID, profileID)
		lease, err := repository.Claim(context.Background(), []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.PublishRendition(context.Background(), testRendition(t, lease, targetID)); !errors.Is(err, ErrInvariant) {
			t.Fatalf("draft profile publication = %v, want ErrInvariant", err)
		}
		assertPublicationCounts(t, pool, 0, 0, 0)
	})

	t.Run("malformed pinned profile", func(t *testing.T) {
		pool, repository := integrationRepository(t, Options{})
		ctx := context.Background()
		mediaID, originalID := insertPublicationMedia(t, pool)
		profileID := newTestUUID(t)
		if _, err := pool.Exec(ctx, `ALTER TABLE profiles DISABLE TRIGGER profiles_definition_validate`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO profiles
			(id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,'malformed_target',1,'draft',ARRAY['image/jpeg'],'nmcp-media',1,'{}')`, profileID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, profileID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `ALTER TABLE profiles ENABLE TRIGGER profiles_definition_validate`); err != nil {
			t.Fatal(err)
		}
		_, targetID := insertTransformForProfile(t, pool, mediaID, originalID, profileID)
		lease, err := repository.Claim(ctx, []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.PublishRendition(ctx, testRendition(t, lease, targetID)); !errors.Is(err, ErrInvariant) {
			t.Fatalf("malformed profile publication = %v, want ErrInvariant", err)
		}
		assertPublicationCounts(t, pool, 0, 0, 0)
	})
}

func TestPublishRenditionVersionOrderingAndRetentionIntegration(t *testing.T) {
	orders := []struct {
		name       string
		versions   []int
		wantEvents int
	}{
		{name: "v1 then v2", versions: []int{1, 2}, wantEvents: 2},
		{name: "v2 then v1", versions: []int{2, 1}, wantEvents: 1},
		{name: "equal version last wins", versions: []int{1, 1}, wantEvents: 2},
	}
	for _, test := range orders {
		t.Run(test.name, func(t *testing.T) {
			pool, repository := integrationRepository(t, Options{})
			mediaID, originalID := insertPublicationMedia(t, pool)
			ensureVersionProfile(t, pool, "ordered", 1, true)
			ensureVersionProfile(t, pool, "ordered", 2, true)
			var lastRendition string
			for index, version := range test.versions {
				_, targetID := insertVersionTransform(t, pool, mediaID, originalID, "ordered", version)
				lease, err := repository.Claim(context.Background(), []Type{TypeTransform})
				if err != nil {
					t.Fatal(err)
				}
				candidate := testRendition(t, lease, targetID)
				publication, err := repository.PublishRendition(context.Background(), candidate)
				if err != nil {
					t.Fatal(err)
				}
				if index == 1 && version < test.versions[0] && publication.Current {
					t.Fatal("late lower version became current")
				}
				if publication.Current {
					lastRendition = candidate.ID
				}
			}
			var currentID string
			var events int
			if err := pool.QueryRow(context.Background(), `SELECT r.id::text,(SELECT count(*) FROM change_events)
				FROM renditions r WHERE r.media_id=$1 AND r.profile_key='ordered' AND r.is_current`, mediaID).Scan(&currentID, &events); err != nil {
				t.Fatal(err)
			}
			if currentID != lastRendition || events != test.wantEvents {
				t.Fatalf("current=%s want=%s events=%d want=%d", currentID, lastRendition, events, test.wantEvents)
			}
		})
	}

	for _, days := range []*int{nil, intPointer(0), intPointer(3)} {
		name := "null"
		if days != nil {
			name = fmt.Sprintf("%d days", *days)
		}
		t.Run(name, func(t *testing.T) {
			pool, repository := integrationRepository(t, Options{})
			if _, err := pool.Exec(context.Background(), `UPDATE system_config SET superseded_rendition_retention_days=$1`, days); err != nil {
				t.Fatal(err)
			}
			mediaID, originalID := insertPublicationMedia(t, pool)
			var firstID string
			for index := range 2 {
				_, targetID := insertVersionTransform(t, pool, mediaID, originalID, "retained", 1)
				lease, err := repository.Claim(context.Background(), []Type{TypeTransform})
				if err != nil {
					t.Fatal(err)
				}
				candidate := testRendition(t, lease, targetID)
				if index == 0 {
					firstID = candidate.ID
				}
				if _, err := repository.PublishRendition(context.Background(), candidate); err != nil {
					t.Fatal(err)
				}
			}
			var purgeAfter *time.Time
			var currentNull bool
			if err := pool.QueryRow(context.Background(), `SELECT
				(SELECT purge_after FROM renditions WHERE id=$1),
				(SELECT purge_after IS NULL FROM renditions WHERE media_id=$2 AND profile_key='retained' AND is_current)`, firstID, mediaID).Scan(&purgeAfter, &currentNull); err != nil {
				t.Fatal(err)
			}
			if !currentNull || days == nil && purgeAfter != nil || days != nil && purgeAfter == nil {
				t.Fatalf("retention days=%v old deadline=%v current null=%v", days, purgeAfter, currentNull)
			}
			if days != nil {
				remaining := time.Until(*purgeAfter)
				want := time.Duration(*days) * 24 * time.Hour
				if remaining < want-time.Minute || remaining > want+time.Minute {
					t.Fatalf("retention days=%d remaining=%s", *days, remaining)
				}
			}
		})
	}
}

func TestPublishRenditionSerializesOnMediaAndKeepsMaximumVersionIntegration(t *testing.T) {
	for _, order := range [][]int{{1, 2}, {2, 1}} {
		name := fmt.Sprintf("v%d acquires before v%d", order[0], order[1])
		t.Run(name, func(t *testing.T) {
			pool, repository := integrationRepository(t, Options{LeaseDuration: 30 * time.Second})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			mediaID, originalID := insertPublicationMedia(t, pool)
			ensureVersionProfile(t, pool, "concurrent", 1, true)
			ensureVersionProfile(t, pool, "concurrent", 2, true)
			for _, version := range []int{1, 2} {
				insertVersionTransform(t, pool, mediaID, originalID, "concurrent", version)
			}
			leases := make(map[int]Lease)
			for range 2 {
				lease, err := repository.Claim(ctx, []Type{TypeTransform})
				if err != nil {
					t.Fatal(err)
				}
				leases[lease.Targets[0].Profile.Version] = lease
			}

			locker, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer locker.Rollback(context.Background())
			if _, err := locker.Exec(ctx, `SELECT id FROM media WHERE id=$1 FOR UPDATE`, mediaID); err != nil {
				t.Fatal(err)
			}
			type result struct {
				version     int
				publication Publication
				err         error
			}
			results := make(chan result, 2)
			start := func(version int) {
				lease := leases[version]
				candidate := testRendition(t, lease, lease.Targets[0].ID)
				go func() {
					publication, err := repository.PublishRendition(ctx, candidate)
					results <- result{version: version, publication: publication, err: err}
				}()
			}
			start(order[0])
			waitForBlockedPublications(t, ctx, pool, 1)
			start(order[1])
			waitForBlockedPublications(t, ctx, pool, 2)
			if err := locker.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			seen := make(map[int]Publication)
			for range 2 {
				result := <-results
				if result.err != nil {
					t.Fatalf("v%d publication error: %v", result.version, result.err)
				}
				seen[result.version] = result.publication
			}
			if len(seen) != 2 || !seen[1].JobFinished || !seen[2].JobFinished || !seen[2].Current || order[0] == 2 && seen[1].Current {
				t.Fatalf("publication outcomes by version = %+v", seen)
			}
			var currentVersion, eventCount int
			if err := pool.QueryRow(ctx, `SELECT p.version,(SELECT count(*) FROM change_events)
				FROM renditions r JOIN job_targets jt ON jt.id=r.job_target_id JOIN profiles p ON p.id=jt.profile_id
				WHERE r.media_id=$1 AND r.profile_key='concurrent' AND r.is_current`, mediaID).Scan(&currentVersion, &eventCount); err != nil {
				t.Fatal(err)
			}
			wantEvents := 2
			if order[0] == 2 {
				wantEvents = 1
			}
			if currentVersion != 2 || eventCount != wantEvents {
				t.Fatalf("current version=%d events=%d, want version=2 events=%d", currentVersion, eventCount, wantEvents)
			}
		})
	}
}

func TestPublishRenditionRechecksLeaseAfterMediaLockWaitIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{LeaseDuration: 200 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	jobID, targets := insertTransformJob(t, pool, 3, 1)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	if _, err := locker.Exec(ctx, `SELECT id FROM media WHERE id=$1 FOR UPDATE`, lease.MediaID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	candidate := testRendition(t, lease, targets[0])
	go func() {
		_, err := repository.PublishRendition(ctx, candidate)
		result <- err
	}()
	waitForBlockedPublications(t, ctx, pool, 1)
	for {
		var expired bool
		if err := pool.QueryRow(ctx, `SELECT lease_expires_at<=clock_timestamp() FROM jobs WHERE id=$1`, jobID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("lease did not expire while publication waited: %v", ctx.Err())
		}
	}
	if err := locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("publication after Media wait = %v, want ErrLeaseLost", err)
	}
	assertPublicationCounts(t, pool, 0, 0, 0)
	var jobStatus Status
	var targetStatus TargetStatus
	if err := pool.QueryRow(ctx, `SELECT j.status,jt.status FROM jobs j JOIN job_targets jt ON jt.job_id=j.id WHERE j.id=$1`, jobID).Scan(&jobStatus, &targetStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != StatusRunning || targetStatus != TargetPending {
		t.Fatalf("expired blocked publication changed job=%s target=%s", jobStatus, targetStatus)
	}
}

func TestPublishRenditionRechecksLeaseAfterChangeFeedWaitIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{LeaseDuration: 200 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	jobID, targets := insertTransformJob(t, pool, 3, 2)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[0]); err != nil {
		t.Fatal(err)
	}
	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	if _, err := locker.Exec(ctx, `SELECT id FROM change_feed_state WHERE id=1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	candidate := testRendition(t, lease, targets[0])
	go func() {
		_, err := repository.PublishRendition(ctx, candidate)
		result <- err
	}()
	waitForBlockedChangeFeedPublication(t, ctx, pool)
	for {
		var expired bool
		if err := pool.QueryRow(ctx, `SELECT lease_expires_at<=clock_timestamp() FROM jobs WHERE id=$1`, jobID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("lease did not expire while publication waited on change feed: %v", ctx.Err())
		}
	}
	if err := locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("publication after change-feed wait = %v, want ErrLeaseLost", err)
	}
	assertPublicationCounts(t, pool, 0, 0, 0)
	var jobStatus Status
	var firstStatus, secondStatus TargetStatus
	var currentCount int
	if err := pool.QueryRow(ctx, `SELECT j.status,
		(SELECT status FROM job_targets WHERE id=$2),(SELECT status FROM job_targets WHERE id=$3),
		(SELECT count(*) FROM renditions WHERE media_id=j.media_id_snapshot AND is_current)
		FROM jobs j WHERE j.id=$1`, jobID, targets[0], targets[1]).Scan(&jobStatus, &firstStatus, &secondStatus, &currentCount); err != nil {
		t.Fatal(err)
	}
	if jobStatus != StatusRunning || firstStatus != TargetPending || secondStatus != TargetPending || currentCount != 0 {
		t.Fatalf("expired feed-blocked publication committed job=%s targets=%s/%s current=%d", jobStatus, firstStatus, secondStatus, currentCount)
	}
}

func TestClaimHydratesPartialRetryGeneratedBytesIntegration(t *testing.T) {
	for _, size := range []int64{videoprocessor.MaxGeneratedOutputBytes, videoprocessor.MaxGeneratedOutputBytes + 1} {
		t.Run(fmt.Sprintf("%d", size), func(t *testing.T) {
			pool, repository := integrationRepository(t, Options{Jitter: func(time.Duration) time.Duration { return 0 }})
			ctx := context.Background()
			jobID, targets := insertTransformJob(t, pool, 3, 2)
			lease, err := repository.Claim(ctx, []Type{TypeTransform})
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[0]); err != nil {
				t.Fatal(err)
			}
			candidate := testRendition(t, lease, targets[0])
			candidate.SizeBytes = size
			if _, err := repository.PublishRendition(ctx, candidate); err != nil {
				t.Fatal(err)
			}
			if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[1]); err != nil {
				t.Fatal(err)
			}
			if err := repository.MarkTargetFailed(ctx, jobID, lease.Token, targets[1], FailureProcessFailed); err != nil {
				t.Fatal(err)
			}
			if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); err != nil {
				t.Fatal(err)
			}
			retry, err := repository.Claim(ctx, []Type{TypeTransform})
			if err != nil || retry.GeneratedBytes != size || len(retry.Targets) != 1 || retry.Targets[0].ID != targets[1] {
				t.Fatalf("retry = %+v, %v; want generated bytes %d", retry, err, size)
			}
		})
	}
}

func TestClaimRejectsGeneratedByteOverflowIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	ensureVersionProfile(t, pool, "overflow-third-target", 1, true)
	jobID, targets := insertTransformJob(t, pool, 3, 3)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	for index, size := range []int64{math.MaxInt64, 1} {
		if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[index]); err != nil {
			t.Fatal(err)
		}
		candidate := testRendition(t, lease, targets[index])
		candidate.SizeBytes = size
		if _, err := repository.PublishRendition(ctx, candidate); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[2]); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkTargetFailed(ctx, jobID, lease.Token, targets[2], FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Claim(ctx, []Type{TypeTransform}); !errors.Is(err, ErrInvariant) {
		t.Fatalf("overflowing generated-byte claim = %v, want ErrInvariant", err)
	}
	var status Status
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,attempts FROM jobs WHERE id=$1`, jobID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != StatusQueued || attempts != 1 {
		t.Fatalf("overflowing hydration committed claim status=%s attempts=%d", status, attempts)
	}
}

func TestClaimRejectsSuccessfulTargetWithoutRenditionIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	jobID, targets := insertTransformJob(t, pool, 3, 2)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE job_targets DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `ALTER TABLE job_targets ENABLE TRIGGER USER`) })
	if _, err := pool.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, targets[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE job_targets ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Claim(ctx, []Type{TypeTransform}); !errors.Is(err, ErrInvariant) {
		t.Fatalf("successful target without rendition claim = %v, want ErrInvariant", err)
	}
	var status Status
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,attempts FROM jobs WHERE id=$1`, jobID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != StatusQueued || attempts != 1 {
		t.Fatalf("invalid hydration committed claim status=%s attempts=%d", status, attempts)
	}
}

func TestPublishRenditionDatabaseBoundariesIntegration(t *testing.T) {
	t.Run("known pre-commit rollback", func(t *testing.T) {
		fault := errors.New("before commit")
		pool, repository := integrationRepository(t, Options{Checkpoint: func(_ context.Context, boundary storage.Boundary, _ string) error {
			if boundary == storage.BoundaryBeforeDBCommit {
				return fault
			}
			return nil
		}})
		_, targets := insertTransformJob(t, pool, 3, 1)
		lease, err := repository.Claim(context.Background(), []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.PublishRendition(context.Background(), testRendition(t, lease, targets[0])); !errors.Is(err, fault) {
			t.Fatalf("pre-commit error=%v", err)
		}
		assertPublicationCounts(t, pool, 0, 0, 0)
	})

	t.Run("commit rejected", func(t *testing.T) {
		pool, repository := integrationRepository(t, Options{})
		if _, err := pool.Exec(context.Background(), `CREATE FUNCTION test_reject_publication_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'deferred publication rejection' USING ERRCODE='23514'; END $$;
		CREATE CONSTRAINT TRIGGER test_reject_publication_commit AFTER INSERT ON renditions
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION test_reject_publication_commit()`); err != nil {
			t.Fatal(err)
		}
		_, targets := insertTransformJob(t, pool, 3, 1)
		lease, err := repository.Claim(context.Background(), []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		_, err = repository.PublishRendition(context.Background(), testRendition(t, lease, targets[0]))
		var rolledBack *CommitRolledBack
		var unknown *CommitOutcomeUnknown
		if !errors.As(err, &rolledBack) || errors.As(err, &unknown) {
			t.Fatalf("commit error=%#v", err)
		}
		assertPublicationCounts(t, pool, 0, 0, 0)
	})

	t.Run("post-commit outcome unknown", func(t *testing.T) {
		fault := errors.New("lost commit response")
		pool, repository := integrationRepository(t, Options{Checkpoint: func(_ context.Context, boundary storage.Boundary, _ string) error {
			if boundary == storage.BoundaryAfterDBCommit {
				return fault
			}
			return nil
		}})
		_, targets := insertTransformJob(t, pool, 3, 1)
		lease, err := repository.Claim(context.Background(), []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		_, err = repository.PublishRendition(context.Background(), testRendition(t, lease, targets[0]))
		var unknown *CommitOutcomeUnknown
		if !errors.As(err, &unknown) || !errors.Is(err, fault) {
			t.Fatalf("post-commit error=%#v", err)
		}
		assertPublicationCounts(t, pool, 1, 1, 1)
	})
}

func TestFinishAttemptCeilingAndSafeErrorsIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	jobID, _ := insertTransformJob(t, pool, 1, 1)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureCode("stderr /secret/file")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsafe failure accepted: %v", err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessOutputLimit); err != nil {
		t.Fatal(err)
	}
	var status Status
	var code, message string
	var finished *time.Time
	if err := pool.QueryRow(ctx, `SELECT status,error_code,error_message,finished_at FROM jobs WHERE id=$1`, jobID).Scan(&status, &code, &message, &finished); err != nil {
		t.Fatal(err)
	}
	if status != StatusFailed || code != string(FailureProcessOutputLimit) || message != safeFailureMessages[FailureProcessOutputLimit] || finished == nil || strings.Contains(message, "secret") {
		t.Fatalf("durable failure = %s %q %q %v", status, code, message, finished)
	}
}

func TestRetryBackoffPersistsDatabaseAvailabilityBoundaryIntegration(t *testing.T) {
	delay := 200 * time.Millisecond
	pool, repository := integrationRepository(t, Options{Jitter: func(maximum time.Duration) time.Duration {
		if delay > maximum {
			return maximum
		}
		return delay
	}})
	ctx := context.Background()
	jobID, _ := insertTransformJob(t, pool, 2, 1)
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	var remaining float64
	if err := pool.QueryRow(ctx, `SELECT EXTRACT(epoch FROM available_at-clock_timestamp()) FROM jobs WHERE id=$1`, jobID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining <= 0 || remaining > delay.Seconds()+0.1 {
		t.Fatalf("persisted retry delay = %f seconds", remaining)
	}
	if _, err := repository.Claim(ctx, []Type{TypeTransform}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("claim before backoff boundary = %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		var available bool
		if err := pool.QueryRow(ctx, `SELECT available_at<=clock_timestamp() FROM jobs WHERE id=$1`, jobID).Scan(&available); err != nil {
			t.Fatal(err)
		}
		if available {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry did not become available at database-clock boundary")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if retry, err := repository.Claim(ctx, []Type{TypeTransform}); err != nil || retry.ID != jobID || retry.Attempts != 2 {
		t.Fatalf("claim after backoff boundary = %+v, %v", retry, err)
	}
}

func TestWorkerSIGKILLNaturalExpiryReclaimIntegration(t *testing.T) {
	pool, repository := integrationRepository(t, Options{Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	jobID, _ := insertTransformJob(t, pool, 3, 1)
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	readyPath := t.TempDir() + "/claimed"
	command := exec.Command(os.Args[0], "-test.run=^TestJobClaimProcessHelper$")
	command.Env = append(os.Environ(),
		"NMCP_JOB_HELPER=1",
		"NMCP_JOB_HELPER_SCHEMA="+schema,
		"NMCP_JOB_HELPER_READY="+readyPath,
	)
	var helperOutput bytes.Buffer
	command.Stdout = &helperOutput
	command.Stderr = &helperOutput
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatalf("claim helper did not become ready: %s", helperOutput.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	var firstToken string
	var firstAttempts int
	if err := pool.QueryRow(ctx, `SELECT lease_token::text,attempts FROM jobs WHERE id=$1 AND status='running'`, jobID).Scan(&firstToken, &firstAttempts); err != nil {
		t.Fatal(err)
	}
	if firstAttempts != 1 {
		t.Fatalf("first process attempts = %d", firstAttempts)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("SIGKILLed claim helper exited successfully")
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		var expired bool
		if err := pool.QueryRow(ctx, `SELECT lease_expires_at<=clock_timestamp() FROM jobs WHERE id=$1`, jobID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("naturally expiring lease did not reach database-clock boundary")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if count, err := repository.ReclaimExpired(ctx); err != nil || count != 1 {
		t.Fatalf("reclaim SIGKILLed owner = %d, %v", count, err)
	}
	var status Status
	var code string
	if err := pool.QueryRow(ctx, `SELECT status,error_code FROM jobs WHERE id=$1`, jobID).Scan(&status, &code); err != nil || status != StatusQueued || code != string(FailureLeaseExpired) {
		t.Fatalf("reclaimed state = %s %q, %v", status, code, err)
	}
	retry, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil || retry.ID != jobID || retry.Attempts != 2 || retry.Token == firstToken {
		t.Fatalf("replacement lease = %+v, %v", retry, err)
	}
	if _, err := repository.Heartbeat(ctx, jobID, firstToken); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("SIGKILLed owner's token remained live: %v", err)
	}
}

func TestJobClaimProcessHelper(t *testing.T) {
	if os.Getenv("NMCP_JOB_HELPER") != "1" {
		t.Skip("claim subprocess helper")
	}
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	schema := os.Getenv("NMCP_JOB_HELPER_SCHEMA")
	readyPath := os.Getenv("NMCP_JOB_HELPER_READY")
	if databaseURL == "" || schema == "" || readyPath == "" {
		t.Fatal("incomplete claim helper environment")
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		_, err := connection.Exec(ctx, `SELECT pg_catalog.set_config('search_path',$1,false)`, schema+",pg_catalog,pg_temp")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repository, err := NewRepository(pool, Options{LeaseDuration: 150 * time.Millisecond, FileBaseURL: "https://files.example.test/files/"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT id::text FROM profiles ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var profileIDs []string
	for rows.Next() {
		var profileID string
		if err := rows.Scan(&profileID); err != nil {
			t.Fatal(err)
		}
		profileIDs = append(profileIDs, profileID)
	}
	rows.Close()
	if _, err := repository.claim(ctx, []Type{TypeTransform}, profileIDs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readyPath, []byte("claimed"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}

func TestReclaimExpiredIntegrationBudgetBatchConcurrencyAndRace(t *testing.T) {
	pool, repository := integrationRepository(t, Options{ReclaimBatch: 2, Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	remaining, _ := insertTransformJob(t, pool, 2, 1)
	ceiling, _ := insertTransformJob(t, pool, 1, 1)
	notExpired, _ := insertTransformJob(t, pool, 2, 1)
	leases := make(map[string]Lease)
	for range 3 {
		lease, err := repository.Claim(ctx, []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		leases[lease.ID] = lease
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=ANY($1::uuid[])`, []string{remaining, ceiling}); err != nil {
		t.Fatal(err)
	}
	count, err := repository.ReclaimExpired(ctx)
	if err != nil || count != 2 {
		t.Fatalf("reclaim = %d, %v", count, err)
	}
	rows, err := pool.Query(ctx, `SELECT id::text,status,error_code FROM jobs WHERE error_code IS NOT NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := map[string][2]string{}
	for rows.Next() {
		var id, status, code string
		if err := rows.Scan(&id, &status, &code); err != nil {
			t.Fatal(err)
		}
		states[id] = [2]string{status, code}
	}
	if states[remaining] != [2]string{"queued", "lease_expired"} || states[ceiling] != [2]string{"failed", "lease_expired"} {
		t.Fatalf("reclaimed states = %#v", states)
	}
	if err := repository.CompleteSucceeded(ctx, remaining, leases[remaining].Token); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("reclaimed stale owner = %v", err)
	}
	if _, err := repository.Heartbeat(ctx, notExpired, leases[notExpired].Token); err != nil {
		t.Fatalf("live owner lost reclaim race: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, remaining); err != nil {
		t.Fatal(err)
	}

	for range 4 {
		id, _ := insertTransformJob(t, pool, 2, 1)
		lease, err := repository.Claim(ctx, []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		if lease.ID != id {
			t.Fatalf("claim ID = %s, want %s", lease.ID, id)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	var total atomic.Int32
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			count, err := repository.ReclaimExpired(ctx)
			if err == nil {
				total.Add(int32(count))
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if total.Load() != 4 {
		t.Fatalf("two-reclaimer total = %d, want 4", total.Load())
	}
}

func TestAdminRetryIntegrationCeilingAuditAndConcurrency(t *testing.T) {
	pool, repository := integrationRepository(t, Options{Jitter: func(time.Duration) time.Duration { return 0 }})
	ctx := context.Background()
	jobID, targets := insertTransformJob(t, pool, 3, 2)
	for attempt := 1; attempt <= 2; attempt++ {
		lease, err := repository.Claim(ctx, []Type{TypeTransform})
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := repository.Claim(ctx, []Type{TypeTransform})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[0]); err != nil {
		t.Fatal(err)
	}
	publishTargetFixture(t, pool, targets[0])
	if err := repository.BeginTarget(ctx, jobID, lease.Token, targets[1]); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkTargetFailed(ctx, jobID, lease.Token, targets[1], FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	if err := repository.FinishAttempt(ctx, jobID, lease.Token, FailureProcessFailed); err != nil {
		t.Fatal(err)
	}
	audits := []AdminAudit{
		{ID: newTestUUID(t), Actor: "admin-a", Host: "host-a", SanitizedArguments: []byte(`{"additional_attempts":3}`)},
		{ID: newTestUUID(t), Actor: "admin-b", Host: "host-b", SanitizedArguments: []byte(`{"additional_attempts":5}`)},
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for index := range audits {
		go func(index int) {
			<-start
			results <- repository.AdminRetryFailed(ctx, jobID, 3+index*2, audits[index])
		}(index)
	}
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) || !(errors.Is(first, ErrConflict) || errors.Is(second, ErrConflict)) {
		t.Fatalf("concurrent retry errors = %v, %v", first, second)
	}
	var status Status
	var attempts, maximum, auditsCount int
	var immediatelyAvailable bool
	if err := pool.QueryRow(ctx, `SELECT status,attempts,max_attempts,available_at<=clock_timestamp(),(SELECT count(*) FROM admin_audit) FROM jobs WHERE id=$1`, jobID).Scan(&status, &attempts, &maximum, &immediatelyAvailable, &auditsCount); err != nil {
		t.Fatal(err)
	}
	if status != StatusQueued || attempts != 3 || maximum < 6 || !immediatelyAvailable || auditsCount != 2 {
		t.Fatalf("retry state = %s attempts=%d max=%d available=%v audits=%d", status, attempts, maximum, immediatelyAvailable, auditsCount)
	}
	var succeeded, pending string
	if err := pool.QueryRow(ctx, `SELECT (SELECT status FROM job_targets WHERE id=$1),(SELECT status FROM job_targets WHERE id=$2)`, targets[0], targets[1]).Scan(&succeeded, &pending); err != nil {
		t.Fatal(err)
	}
	if succeeded != "succeeded" || pending != "pending" {
		t.Fatalf("target states after retry = %s, %s", succeeded, pending)
	}
	var succeededAudits, failedAudits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE outcome='succeeded'),count(*) FILTER (WHERE outcome='failed' AND error_code='conflict' AND error_message='job is not failed') FROM admin_audit`).Scan(&succeededAudits, &failedAudits); err != nil {
		t.Fatal(err)
	}
	if succeededAudits != 1 || failedAudits != 1 {
		t.Fatalf("admin retry audit outcomes = succeeded:%d failed:%d", succeededAudits, failedAudits)
	}
	if _, err := pool.Exec(ctx, `UPDATE admin_audit SET actor='tampered'`); err == nil {
		t.Fatal("admin audit was mutable")
	}
	lease, err = repository.Claim(ctx, []Type{TypeTransform})
	if err != nil || lease.ID != jobID {
		t.Fatalf("admin-retried immediate claim = %+v, %v", lease, err)
	}
}

func TestQueuedAttemptInvariantCounterexamplesIntegration(t *testing.T) {
	pool, _ := integrationRepository(t, Options{})
	ctx := context.Background()
	jobID := insertPurgeJob(t, pool, 1, `clock_timestamp()`)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET attempts=max_attempts WHERE id=$1`, jobID); err == nil {
		t.Fatal("queued attempts=max_attempts accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET max_attempts=attempts WHERE id=$1`, jobID); err == nil {
		t.Fatal("queued max_attempts=attempts accepted")
	}
}

type integrationTestRepository struct {
	*Repository
}

func (r *integrationTestRepository) Claim(ctx context.Context, registeredTypes []Type) (Lease, error) {
	rows, err := r.pool.Query(ctx, `SELECT id::text FROM profiles ORDER BY id`)
	if err != nil {
		return Lease{}, err
	}
	defer rows.Close()
	var profileIDs []string
	for rows.Next() {
		var profileID string
		if err := rows.Scan(&profileID); err != nil {
			return Lease{}, err
		}
		profileIDs = append(profileIDs, profileID)
	}
	if err := rows.Err(); err != nil {
		return Lease{}, err
	}
	return r.Repository.claim(ctx, registeredTypes, profileIDs)
}

func integrationRepository(t *testing.T, options Options) (*pgxpool.Pool, *integrationTestRepository) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(base.Close)
	id := strings.ReplaceAll(newTestUUID(t), "-", "")
	schema := "job_" + id
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop job integration schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxConns < 6 {
		config.MaxConns = 6
	}
	config.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		_, err := connection.Exec(ctx, `SELECT pg_catalog.set_config('search_path',$1,false)`, schema+",pg_catalog,pg_temp")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrator, err := dbmigration.NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	// Publication tests need executable pinned profiles. Capability
	// certification/activation behavior is covered by the profile/schema suites;
	// this package isolates publication by installing valid bundled definitions
	// as previously activated profiles.
	if _, err := pool.Exec(ctx, `ALTER TABLE profiles DISABLE TRIGGER USER;
		UPDATE profiles SET status='active',activated_at=clock_timestamp() WHERE status='draft';
		ALTER TABLE profiles ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if options.FileBaseURL == "" {
		options.FileBaseURL = "https://files.example.test/files/"
	}
	repository, err := NewRepository(pool, options)
	if err != nil {
		t.Fatal(err)
	}
	return pool, &integrationTestRepository{Repository: repository}
}

func insertPurgeJob(t *testing.T, pool *pgxpool.Pool, maxAttempts int, availableExpression string) string {
	t.Helper()
	id := newTestUUID(t)
	mediaID := newTestUUID(t)
	query := fmt.Sprintf(`INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts,available_at) VALUES ($1,'purge',$2,'queued',$3,%s)`, availableExpression)
	if _, err := pool.Exec(context.Background(), query, id, mediaID, maxAttempts); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertTransformJob(t *testing.T, pool *pgxpool.Pool, maxAttempts, targetCount int) (string, []string) {
	return insertTransformJobAt(t, pool, maxAttempts, targetCount, `clock_timestamp()`)
}

func insertTransformJobAt(t *testing.T, pool *pgxpool.Pool, maxAttempts, targetCount int, availableExpression string) (string, []string) {
	t.Helper()
	ctx := context.Background()
	mediaID, originalID, jobID := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height) VALUES ($1,$2,$3,$4,'image/jpeg',1,1,1)`,
		originalID, mediaID, strings.Repeat(strings.ReplaceAll(mediaID, "-", ""), 2), "originals/"+originalID[:2]+"/"+originalID+"/original.jpg"); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT id::text FROM profiles ORDER BY key,version LIMIT $1`, targetCount)
	if err != nil {
		t.Fatal(err)
	}
	var profiles []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, id)
	}
	rows.Close()
	if len(profiles) != targetCount {
		t.Fatalf("profile fixtures = %d, want %d", len(profiles), targetCount)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	query := fmt.Sprintf(`INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts,available_at) VALUES ($1,'transform',$2,$3,'queued',$4,%s)`, availableExpression)
	if _, err := tx.Exec(ctx, query, jobID, originalID, mediaID, maxAttempts); err != nil {
		t.Fatal(err)
	}
	targets := make([]string, 0, targetCount)
	for _, profileID := range profiles {
		targetID := newTestUUID(t)
		if _, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profileID); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, targetID)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return jobID, targets
}

func insertPrimaryStreamTransform(t *testing.T, pool *pgxpool.Pool, mime, sourceMetadata string) string {
	t.Helper()
	ctx := context.Background()
	mediaID, originalID := newTestUUID(t), newTestUUID(t)
	if _, err := pool.Exec(ctx, `INSERT INTO media (id,media_type,source_metadata,taken_at_source) VALUES ($1,$2,$3::jsonb,'unknown')`, mediaID, mime, sourceMetadata); err != nil {
		t.Fatal(err)
	}
	extension, err := storage.OriginalExtensionForMIME(mime)
	if err != nil {
		t.Fatal(err)
	}
	original, err := storage.ParseOriginalID(originalID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := storage.NewOriginalKey(original, extension)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height) VALUES ($1,$2,$3,$4,$5,1,1,1)`, originalID, mediaID, strings.Repeat("e", 64), key.String(), mime); err != nil {
		t.Fatal(err)
	}
	var profileID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM profiles WHERE key='standard' AND version=1`).Scan(&profileID); err != nil {
		t.Fatal(err)
	}
	jobID, _ := insertTransformForProfile(t, pool, mediaID, originalID, profileID)
	return jobID
}

// publishTargetFixture establishes the publication cross-table invariants
// without exposing a repository operation that could mark a target succeeded
// before its rendition is durable and referenced.
func publishTargetFixture(t *testing.T, pool *pgxpool.Pool, targetID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	renditionID := newTestUUID(t)
	if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='succeeded',error_code=NULL,error_message=NULL,updated_at=clock_timestamp() WHERE id=$1`, targetID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO renditions
		(id,media_id,job_target_id,profile_key,is_current,purge_after,relative_path,mime_type,width,height,size_bytes,sha256,processor_audit)
		SELECT $1,j.media_id_snapshot,jt.id,'fixture',false,clock_timestamp()+interval '1 day',$2,'image/avif',1,1,1,$3,'{"fixture":"job-repository"}'
		FROM job_targets jt JOIN jobs j ON j.id=jt.job_id WHERE jt.id=$4`,
		renditionID, "renditions/fixture/"+renditionID+".avif", strings.Repeat("a", 64), targetID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,
		finished_at=clock_timestamp(),error_code=NULL,error_message=NULL,updated_at=clock_timestamp()
		WHERE id=(SELECT job_id FROM job_targets WHERE id=$1)
		  AND NOT EXISTS (
		      SELECT 1 FROM job_targets
		      WHERE job_id=(SELECT job_id FROM job_targets WHERE id=$1) AND status<>'succeeded'
		  )`, targetID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func testRendition(t *testing.T, lease Lease, targetID string) Rendition {
	t.Helper()
	if lease.Original == nil {
		t.Fatal("transform lease has no original")
	}
	var profileID string
	for _, target := range lease.Targets {
		if target.ID == targetID {
			profileID = target.Profile.ID
			break
		}
	}
	if profileID == "" {
		t.Fatalf("target %s is not in lease", targetID)
	}
	renditionID := newTestUUID(t)
	width, height := 1, 1
	return Rendition{
		ID: renditionID, JobID: lease.ID, LeaseToken: lease.Token, TargetID: targetID,
		OriginalID: lease.Original.ID, MediaID: lease.MediaID, ProfileID: profileID,
		RelativePath: "renditions/" + lease.Original.ID[:2] + "/" + lease.Original.ID + "/" + targetID + "/" + renditionID + ".avif",
		MIMEType:     "image/avif", Width: &width, Height: &height, SizeBytes: 1,
		SHA256: strings.Repeat("b", 64), ProcessorAudit: []byte(`{"schema_version":1,"family":"still","result":{"processor":"integration"}}`),
	}
}

func insertPublicationMedia(t *testing.T, pool *pgxpool.Pool) (string, string) {
	return insertPublicationMediaWithSHA(t, pool, strings.Repeat("c", 64))
}

func insertPublicationMediaWithSHA(t *testing.T, pool *pgxpool.Pool, digest string) (string, string) {
	t.Helper()
	mediaID, originalID := newTestUUID(t), newTestUUID(t)
	if _, err := pool.Exec(context.Background(), `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image/jpeg','unknown')`, mediaID); err != nil {
		t.Fatal(err)
	}
	path := "originals/" + originalID[:2] + "/" + originalID + "/original.jpg"
	if _, err := pool.Exec(context.Background(), `INSERT INTO originals
		(id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
		VALUES ($1,$2,$3,$4,'image/jpeg',1,1,1)`, originalID, mediaID, digest, path); err != nil {
		t.Fatal(err)
	}
	return mediaID, originalID
}

func insertVersionTransform(t *testing.T, pool *pgxpool.Pool, mediaID, originalID, key string, version int) (string, string) {
	t.Helper()
	profileID := ensureVersionProfile(t, pool, key, version, true)
	return insertTransformForProfile(t, pool, mediaID, originalID, profileID)
}

func insertTransformForProfile(t *testing.T, pool *pgxpool.Pool, mediaID, originalID, profileID string) (string, string) {
	t.Helper()
	ctx := context.Background()
	jobID, targetID := newTestUUID(t), newTestUUID(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts)
		VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profileID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return jobID, targetID
}

func ensureVersionProfile(t *testing.T, pool *pgxpool.Pool, key string, version int, activate bool) string {
	t.Helper()
	ctx := context.Background()
	var profileID, status string
	err := pool.QueryRow(ctx, `SELECT id::text,status FROM profiles WHERE key=$1 AND version=$2`, key, version).Scan(&profileID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		profileID = newTestUUID(t)
		if _, err = pool.Exec(ctx, `INSERT INTO profiles
			(id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			SELECT $1,$2,$3,'draft',input_mime_types,processor,parameters_schema_version,parameters
			FROM profiles WHERE key='standard' AND version=1`, profileID, key, version); err != nil {
			t.Fatal(err)
		}
		status = "draft"
	} else if err != nil {
		t.Fatal(err)
	}
	if activate && status == "draft" {
		// Model a profile that was activated while its older queued Jobs retained
		// their pinned IDs. The schema's certification and monotonic activation
		// paths are independently tested; publication only consumes that history.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `ALTER TABLE profiles DISABLE TRIGGER USER`); err == nil {
			_, err = tx.Exec(ctx, `UPDATE profiles SET status='retired',retired_at=COALESCE(retired_at,clock_timestamp())
				WHERE key=$2 AND status='active' AND id<>$1`, profileID, key)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE profiles SET status='active',activated_at=COALESCE(activated_at,clock_timestamp()),retired_at=NULL WHERE id=$1`, profileID)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `ALTER TABLE profiles ENABLE TRIGGER USER`)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return profileID
}

func assertPublicationCounts(t *testing.T, pool *pgxpool.Pool, renditions, events int, position int64) {
	t.Helper()
	var gotRenditions, gotEvents int
	var gotPosition int64
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM renditions),(SELECT count(*) FROM change_events),
		(SELECT last_position FROM change_feed_state WHERE id=1)`).Scan(&gotRenditions, &gotEvents, &gotPosition); err != nil {
		t.Fatal(err)
	}
	if gotRenditions != renditions || gotEvents != events || gotPosition != position {
		t.Fatalf("publication counts renditions=%d events=%d position=%d", gotRenditions, gotEvents, gotPosition)
	}
}

func waitForBlockedPublications(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()
	for {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
			WHERE wait_event_type='Lock' AND query LIKE 'SELECT deleted_at FROM media WHERE id=$1 FOR UPDATE%'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count >= want {
			return
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("blocked publications=%d, want at least %d: %v", count, want, ctx.Err())
		}
	}
}

func waitForBlockedChangeFeedPublication(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity
			WHERE wait_event_type='Lock' AND query LIKE 'UPDATE change_feed_state SET last_position=%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("publication did not block on change feed: %v", ctx.Err())
		}
	}
}

func intPointer(value int) *int { return &value }

func newTestUUID(t *testing.T) string {
	t.Helper()
	id, err := newUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
