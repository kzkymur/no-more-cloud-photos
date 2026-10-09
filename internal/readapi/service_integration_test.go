package readapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	dbmigration "github.com/kzkymur/no-more-cloud-photos/internal/database"
)

func TestReadServiceIntegration(t *testing.T) {
	ctx := context.Background()
	pool := readIntegrationPool(t)
	codec, err := NewCursorCodec([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(pool, codec, "https://files.example/prefix/files")
	if err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	media := []integrationMedia{
		{ID: integrationUUID(1), OriginalID: integrationUUID(1001), TakenAt: timePointer(base.Add(time.Hour))},
		{ID: integrationUUID(2), OriginalID: integrationUUID(1002), TakenAt: timePointer(base)},
		{ID: integrationUUID(3), OriginalID: integrationUUID(1003), TakenAt: timePointer(base)},
		{ID: integrationUUID(4), OriginalID: integrationUUID(1004)},
		{ID: integrationUUID(5), OriginalID: integrationUUID(1005), Deleted: true},
	}
	for index := range media {
		insertIntegrationMedia(t, pool, media[index], base.Add(time.Duration(index)*time.Minute))
	}

	t.Run("media pagination null ties deleted modes and profile existence", func(t *testing.T) {
		request := NewMediaListRequest()
		request.Limit = 2
		var got []string
		for {
			page, err := service.ListMedia(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range page.Items {
				got = append(got, item.ID)
				if item.CreatedAt.Location() != time.UTC || item.Rendition != nil {
					t.Fatalf("unexpected media projection: %#v", item)
				}
			}
			if page.NextCursor == nil {
				break
			}
			request.Cursor = *page.NextCursor
		}
		want := []string{media[0].ID, media[2].ID, media[1].ID, media[3].ID}
		if !slices.Equal(got, want) {
			t.Fatalf("all pages = %v, want %v", got, want)
		}
		seen := make(map[string]bool)
		for _, id := range got {
			if seen[id] {
				t.Fatalf("duplicate media across pages: %s", id)
			}
			seen[id] = true
		}

		only := NewMediaListRequest()
		only.Deleted = DeletedOnly
		page, err := service.ListMedia(ctx, only)
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != media[4].ID {
			t.Fatalf("deleted only = %#v, %v", page, err)
		}
		include := NewMediaListRequest()
		include.Deleted = DeletedInclude
		page, err = service.ListMedia(ctx, include)
		if err != nil || len(page.Items) != len(media) {
			t.Fatalf("deleted include count = %d, %v", len(page.Items), err)
		}
		unknown := NewMediaListRequest()
		unknown.Profile = "missing"
		if _, err := service.ListMedia(ctx, unknown); !IsKind(err, KindInvalidProfile) {
			t.Fatalf("missing profile error = %#v", err)
		}
	})

	t.Run("original detail and rendition semantics", func(t *testing.T) {
		original, err := service.GetOriginal(ctx, media[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		wantURL := "https://files.example/prefix/files/originals/" + media[0].OriginalID[:2] + "/" + media[0].OriginalID + "/original.jpg"
		if original.FileURL != wantURL || strings.Contains(original.FileURL, "/files/files/") {
			t.Fatalf("original URL = %q, want %q", original.FileURL, wantURL)
		}
		if _, err := service.GetCurrentRendition(ctx, media[0].ID, "standard"); !IsKind(err, KindRenditionNotReady) {
			t.Fatalf("not-ready error = %#v", err)
		}

		renditionID, targetID := integrationUUID(2001), integrationUUID(3001)
		insertIntegrationRendition(t, pool, media[0], integrationUUID(4001), targetID, renditionID, base.Add(2*time.Hour))
		current, err := service.GetCurrentRendition(ctx, media[0].ID, "standard")
		if err != nil {
			t.Fatal(err)
		}
		if current.ID != renditionID || current.Profile.Key != "standard" || !strings.HasSuffix(current.FileURL, renditionID+".avif") {
			t.Fatalf("current rendition = %#v", current)
		}
		historical, err := service.GetRendition(ctx, renditionID)
		if err != nil || historical.ID != renditionID {
			t.Fatalf("historical rendition = %#v, %v", historical, err)
		}
		detail, err := service.GetMedia(ctx, media[0].ID)
		if err != nil || len(detail.CurrentRenditions) != 1 || detail.CurrentRenditions[0].ID != renditionID || detail.Jobs == nil {
			t.Fatalf("media detail = %#v, %v", detail, err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM renditions WHERE id=$1`, renditionID); err == nil {
			t.Fatal("direct current Rendition deletion succeeded")
		}
		if _, err := pool.Exec(ctx, `UPDATE renditions SET is_current=false,purge_after=$2 WHERE id=$1`, renditionID, base); err != nil {
			t.Fatal(err)
		}
		insertIntegrationRendition(t, pool, media[0], integrationUUID(4002), integrationUUID(3002), integrationUUID(2002), base.Add(3*time.Hour))
		cleanupID := integrationUUID(3101)
		cleanupTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = cleanupTx.Exec(ctx, `SET LOCAL ROLE nmcp_worker_runtime`); err == nil {
			_, err = cleanupTx.Exec(ctx, `INSERT INTO rendition_cleanup_progress
				(id,media_id_snapshot,rendition_id,job_target_id,relative_path,size_bytes,purge_after)
				SELECT $1,media_id,id,job_target_id,relative_path,size_bytes,purge_after FROM renditions WHERE id=$2`, cleanupID, renditionID)
		}
		if err == nil {
			_, err = cleanupTx.Exec(ctx, `SELECT nmcp_complete_rendition_cleanup($1,$2,$3,'missing')`, cleanupID, media[0].ID, renditionID)
		}
		if err != nil {
			_ = cleanupTx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := cleanupTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE renditions SET is_current=false,purge_after=$2 WHERE id=$1`, integrationUUID(2002), base.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		job, err := service.GetJob(ctx, integrationUUID(4001))
		if err != nil || len(job.Targets) != 1 || job.Targets[0].RenditionID != nil {
			t.Fatalf("cleaned rendition target = %#v, %v", job, err)
		}
		if _, err := service.GetRendition(ctx, renditionID); !IsKind(err, KindNotFound) {
			t.Fatalf("cleaned rendition error = %#v", err)
		}
	})

	t.Run("jobs pagination filters targets cleanup and purge history", func(t *testing.T) {
		purged := integrationMedia{ID: integrationUUID(10), OriginalID: integrationUUID(1010)}
		insertIntegrationMedia(t, pool, purged, base)
		if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at=$2,purge_after=$2 WHERE id=$1`, purged.ID, base); err != nil {
			t.Fatal(err)
		}
		created := base.Add(3 * time.Hour)
		jobIDs := make([]string, 0, 101)
		for index := range 101 {
			jobID := integrationUUID(5000 + index)
			jobIDs = append(jobIDs, jobID)
			if _, err := pool.Exec(ctx, `
				INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts,available_at,created_at,updated_at)
				VALUES ($1,'purge',$2,'queued',3,$3,$3,$3)`, jobID, purged.ID, created); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `
				UPDATE jobs SET status='cancelled',finished_at=$2,cancelled_at=$2,cancel_reason='media_restored',updated_at=$2
				WHERE id=$1`, jobID, created); err != nil {
				t.Fatal(err)
			}
		}

		detail, err := service.GetMedia(ctx, purged.ID)
		if err != nil || len(detail.Jobs) != 100 {
			t.Fatalf("detail jobs = %d, %v", len(detail.Jobs), err)
		}
		request := NewJobListRequest()
		request.MediaID, request.Limit = purged.ID, 13
		var got []string
		for {
			page, err := service.ListJobs(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			for _, job := range page.Items {
				if job.Targets == nil || len(job.Targets) != 0 || job.MediaID != purged.ID {
					t.Fatalf("purge job projection = %#v", job)
				}
				got = append(got, job.ID)
			}
			if page.NextCursor == nil {
				break
			}
			request.Cursor = *page.NextCursor
		}
		if len(got) != 101 {
			t.Fatalf("paged jobs count = %d", len(got))
		}
		for index := 1; index < len(got); index++ {
			if got[index-1] <= got[index] {
				t.Fatalf("job tie order is not descending: %s then %s", got[index-1], got[index])
			}
		}

		purgeJobID := integrationUUID(5200)
		purgeToken := integrationUUID(6200)
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, purgeJobID, purged.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running',attempts=1,lease_token=$2,lease_expires_at=clock_timestamp()+interval '1 minute',started_at=clock_timestamp() WHERE id=$1`, purgeJobID, purgeToken); err != nil {
			t.Fatal(err)
		}
		manifestTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = manifestTx.Exec(ctx, `SELECT pg_catalog.set_config('nmcp.purge_lease_token',$1,true)`, purgeToken); err == nil {
			_, err = manifestTx.Exec(ctx, `INSERT INTO purge_file_progress
				(job_id,media_id_snapshot,object_kind,object_id,relative_path,size_bytes)
				SELECT $1::nmcp_uuid_v4,$2::nmcp_uuid_v4,'original',id,relative_path,size_bytes FROM originals WHERE media_id=$2::nmcp_uuid_v4`, purgeJobID, purged.ID)
		}
		if err != nil {
			_ = manifestTx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := manifestTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		purgeTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = purgeTx.Exec(ctx, `SELECT nmcp_complete_purge_file_progress($1,$2,'original',$3,$4,'missing')`, purgeJobID, purged.ID, purged.OriginalID, purgeToken)
		if err == nil {
			_, err = purgeTx.Exec(ctx, `SELECT pg_catalog.set_config('nmcp.purge_job_id',$1,true),pg_catalog.set_config('nmcp.purge_lease_token',$2,true)`, purgeJobID, purgeToken)
		}
		if err == nil {
			_, err = purgeTx.Exec(ctx, `DELETE FROM media WHERE id=$1`, purged.ID)
		}
		if err == nil {
			var position int64
			if err = purgeTx.QueryRow(ctx, `UPDATE change_feed_state SET last_position=last_position+1 WHERE id=1 RETURNING last_position`).Scan(&position); err == nil {
				_, err = purgeTx.Exec(ctx, `INSERT INTO change_events (id,position,event_type,reason,media_id,payload) VALUES ($1,$2,'media_purged','physical_purge',$3,NULL)`, integrationUUID(999), position, purged.ID)
			}
		}
		if err == nil {
			_, err = purgeTx.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,finished_at=clock_timestamp() WHERE id=$1`, purgeJobID)
		}
		if err != nil {
			_ = purgeTx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := purgeTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		request.Cursor = ""
		page, err := service.ListJobs(ctx, request)
		if err != nil || len(page.Items) != 13 {
			t.Fatalf("purged media job history = %#v, %v", page, err)
		}
		job, err := service.GetJob(ctx, jobIDs[0])
		if err != nil || job.MediaID != purged.ID || len(job.Targets) != 0 {
			t.Fatalf("durable purge job = %#v, %v", job, err)
		}
	})

	t.Run("job lookups preserve UUID indexes", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO media (id,media_type,taken_at_source,deleted_at,purge_after)
			SELECT ('82000000-0000-4000-8000-' || lpad(to_hex(value),12,'0'))::uuid,
			       'image/jpeg','unknown',$1,$1
			FROM generate_series(1,2048) AS value`, base.Add(4*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts,available_at,created_at,updated_at)
			SELECT ('81000000-0000-4000-8000-' || lpad(to_hex(value),12,'0'))::uuid,
			       'purge',('82000000-0000-4000-8000-' || lpad(to_hex(value),12,'0'))::uuid,'queued',3,
			       $1::timestamptz + value * interval '1 microsecond',$1::timestamptz + value * interval '1 microsecond',
			       $1::timestamptz + value * interval '1 microsecond'
			FROM generate_series(1,2048) AS value`, base.Add(4*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `ANALYZE jobs`); err != nil {
			t.Fatal(err)
		}

		idPlan := integrationExplain(t, pool, jobHeadersSQL,
			"81000000-0000-4000-8000-000000000001", "", nil, false, nil, nil, 1)
		if !strings.Contains(idPlan, "jobs_pkey") {
			t.Fatalf("job ID plan does not use jobs_pkey:\n%s", idPlan)
		}
		mediaPlan := integrationExplain(t, pool, jobHeadersSQL,
			nil, "", integrationUUID(10), false, nil, nil, 13)
		if !strings.Contains(mediaPlan, "jobs_media_list_idx") {
			t.Fatalf("job media plan does not use jobs_media_list_idx:\n%s", mediaPlan)
		}
	})

	t.Run("profiles preserve arrays raw JSON and ordering", func(t *testing.T) {
		page, err := service.ListProfiles(ctx, ProfileListRequest{Status: ProfileDraft})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) < 2 || page.Items[0].Key != "standard" || page.Items[1].Key != "thumbnail" {
			t.Fatalf("profiles order = %#v", page.Items)
		}
		for _, profile := range page.Items {
			if profile.InputMIMETypes == nil || len(profile.InputMIMETypes) == 0 || len(profile.Parameters) == 0 || profile.Parameters[0] != '{' {
				t.Fatalf("profile projection = %#v", profile)
			}
		}
	})

	t.Run("custom profile old current and duplicate-current diagnostics", func(t *testing.T) {
		const (
			standardV1 = "60000000-0000-4000-8000-000000000001"
			standardV2 = "60000000-0000-4000-8000-000000000011"
			customV1   = "60000000-0000-4000-8000-000000000012"
		)
		if _, err := pool.Exec(ctx, `INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			SELECT $1,'standard',2,'draft',input_mime_types,processor,parameters_schema_version,parameters
			FROM profiles WHERE id=$2`, standardV2, standardV1); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			SELECT $1,'custom',1,'draft',input_mime_types,processor,parameters_schema_version,parameters
			FROM profiles WHERE id=$2`, customV1, standardV1); err != nil {
			t.Fatal(err)
		}
		customRenditionID := integrationUUID(2201)
		insertIntegrationRenditionForProfile(t, pool, media[1], integrationUUID(4201), integrationUUID(3201), customRenditionID, customV1, "custom", base.Add(5*time.Hour), true)
		customRequest := NewMediaListRequest()
		customRequest.Profile = "custom"
		customPage, err := service.ListMedia(ctx, customRequest)
		if err != nil {
			t.Fatal(err)
		}
		var customFound bool
		for _, item := range customPage.Items {
			if item.ID == media[1].ID {
				customFound = item.Rendition != nil && item.Rendition.ID == customRenditionID && item.Rendition.Profile.Key == "custom"
			}
		}
		if !customFound {
			t.Fatalf("custom profile current missing from page: %#v", customPage.Items)
		}
		oldStandardRenditionID := integrationUUID(2203)
		insertIntegrationRenditionForProfile(t, pool, media[0], integrationUUID(4203), integrationUUID(3203), oldStandardRenditionID, standardV1, "standard", base.Add(5*time.Hour+time.Minute), true)

		certifyIntegrationProfiles(t, pool)
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, customV1); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, standardV1); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, standardV2); err != nil {
			t.Fatal(err)
		}
		var customStatus, standardV1Status, standardV2Status string
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT status FROM profiles WHERE id=$1),
			(SELECT status FROM profiles WHERE id=$2),
			(SELECT status FROM profiles WHERE id=$3)`, customV1, standardV1, standardV2).Scan(&customStatus, &standardV1Status, &standardV2Status); err != nil {
			t.Fatal(err)
		}
		if customStatus != "active" || standardV1Status != "retired" || standardV2Status != "active" {
			t.Fatalf("production lifecycle statuses = custom:%s standard-v1:%s standard-v2:%s", customStatus, standardV1Status, standardV2Status)
		}
		standardRequest := NewMediaListRequest()
		standardPage, err := service.ListMedia(ctx, standardRequest)
		if err != nil {
			t.Fatal(err)
		}
		var oldCurrentFound bool
		for _, item := range standardPage.Items {
			if item.ID == media[0].ID && item.Rendition != nil {
				oldCurrentFound = item.Rendition.Profile.ID == standardV1 && item.Rendition.Profile.Version == 1
			}
		}
		if !oldCurrentFound {
			t.Fatalf("standard v1 current disappeared after v2 activation: %#v", standardPage.Items)
		}

		if _, err := pool.Exec(ctx, `DROP INDEX renditions_one_current_key_idx`); err != nil {
			t.Fatal(err)
		}
		insertIntegrationRenditionForProfile(t, pool, media[0], integrationUUID(4202), integrationUUID(3202), integrationUUID(2202), standardV2, "standard", base.Add(6*time.Hour), true)
		if _, err := service.ListMedia(ctx, standardRequest); !IsKind(err, KindInvariant) {
			t.Fatalf("duplicate current list error = %#v, want invariant", err)
		}
		if _, err := service.GetCurrentRendition(ctx, media[0].ID, "standard"); !IsKind(err, KindInvariant) {
			t.Fatalf("duplicate current route error = %#v, want invariant", err)
		}
	})
}

type integrationMedia struct {
	ID, OriginalID string
	TakenAt        *time.Time
	Deleted        bool
}

func insertIntegrationMedia(t *testing.T, pool *pgxpool.Pool, media integrationMedia, createdAt time.Time) {
	t.Helper()
	source := "unknown"
	if media.TakenAt != nil {
		source = "embedded_offset"
	}
	var deletedAt, purgeAfter *time.Time
	if media.Deleted {
		deleted, purge := createdAt.Add(time.Hour), createdAt.Add(24*time.Hour)
		deletedAt, purgeAfter = &deleted, &purge
	}
	path := "originals/" + media.OriginalID[:2] + "/" + media.OriginalID + "/original.jpg"
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media (id,media_type,taken_at,taken_at_source,created_at,deleted_at,purge_after)
		VALUES ($1,'image/jpeg',$2,$3,$4,$5,$6)`, media.ID, media.TakenAt, source, createdAt, deletedAt, purgeAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,created_at)
		VALUES ($1,$2,$3,$4,'image/jpeg',42,$5)`, media.OriginalID, media.ID, integrationDigest(media.OriginalID), path, createdAt); err != nil {
		t.Fatal(err)
	}
}

func insertIntegrationRendition(t *testing.T, pool *pgxpool.Pool, media integrationMedia, jobID, targetID, renditionID string, now time.Time) {
	insertIntegrationRenditionForProfile(t, pool, media, jobID, targetID, renditionID, "60000000-0000-4000-8000-000000000001", "standard", now, true)
}

func insertIntegrationRenditionForProfile(t *testing.T, pool *pgxpool.Pool, media integrationMedia, jobID, targetID, renditionID, profileID, profileKey string, now time.Time, current bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// A storage-attempt token identifies one exact transform claim globally;
	// using the same fixture token for multiple Jobs hides that invariant.
	leaseID := jobID
	path := "renditions/" + media.OriginalID[:2] + "/" + media.OriginalID + "/" + targetID + "/" + renditionID + ".avif"
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts,available_at,created_at,updated_at) VALUES ($1,'transform',$2,$3,'queued',3,$4,$4,$4)`, []any{jobID, media.OriginalID, media.ID, now}},
		{`INSERT INTO job_targets (id,job_id,profile_id,status,updated_at) VALUES ($1,$2,$3,'pending',$4)`, []any{targetID, jobID, profileID, now}},
		{`UPDATE jobs SET status='running',attempts=1,lease_token=$2,lease_expires_at=$3,started_at=$4,updated_at=$4 WHERE id=$1`, []any{jobID, leaseID, now.Add(time.Minute), now}},
		{`UPDATE job_targets SET status='succeeded',attempts=1,updated_at=$2 WHERE id=$1`, []any{targetID, now}},
		{`INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256,created_at,processor_audit) VALUES ($1,$2,$3,$4,$5,$6,'image/avif',21,$7,$8,'{"fixture":"read-service"}')`, []any{renditionID, media.ID, targetID, profileKey, current, path, integrationDigest(renditionID), now}},
		{`UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,finished_at=$2,updated_at=$2 WHERE id=$1`, []any{jobID, now}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func integrationUUID(value int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", value)
}

func integrationDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func timePointer(value time.Time) *time.Time { return &value }

func integrationExplain(t *testing.T, pool *pgxpool.Pool, query string, arguments ...any) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "EXPLAIN (COSTS OFF) "+query, arguments...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}

func certifyIntegrationProfiles(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		WITH candidates AS (
			SELECT capability.processor,capability.parameters_schema_version,
			       capability.input_mime_type,capability.source_mode,output.output_kind
			FROM profile_processor_capabilities AS capability
			CROSS JOIN LATERAL unnest(
				CASE capability.source_mode
				WHEN 'still' THEN ARRAY['still-avif']::text[]
				WHEN 'probe-animation' THEN ARRAY['still-avif','animation-webp']::text[]
				WHEN 'video' THEN ARRAY['still-avif','video-av1']::text[]
				END
			) AS output(output_kind)
		), numbered AS (
			SELECT candidates.*,row_number() OVER (ORDER BY input_mime_type,output_kind) AS ordinal
			FROM candidates
		)
		INSERT INTO profile_processor_certifications (
			id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
			max_long_edge,minimum_setting,maximum_setting,evidence
		)
		SELECT ('90000000-0000-4000-8000-' || lpad(to_hex(ordinal),12,'0'))::uuid,
		       processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
		       100000,CASE WHEN output_kind='video-av1' THEN 0 ELSE 1 END,
		       CASE WHEN output_kind='video-av1' THEN 63 ELSE 100 END,
		       'read API integration certification fixture'
		FROM numbered`)
	if err != nil {
		t.Fatal(err)
	}
}

func readIntegrationPool(t *testing.T) *pgxpool.Pool {
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
	schema := "readapi_" + strings.ReplaceAll(integrationUUID(int(time.Now().UnixNano()&0xffffffffff)), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop read API integration schema: %v", err)
		}
	})
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
	t.Cleanup(pool.Close)
	migrator, err := dbmigration.NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}
