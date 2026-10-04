package database

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

func runInitialSchemaIntegrationTests(t *testing.T, databaseURL string) {
	t.Helper()
	t.Run("initial schema seeds and checks", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()

		var deletedRetention, renditionRetention *int
		var timezone string
		var interval, backupRetention, profiles int
		if err := pool.QueryRow(ctx, `
			SELECT deleted_media_retention_days, superseded_rendition_retention_days,
			       default_timezone, db_backup_interval_hours, db_backup_retention_days,
			       (SELECT count(*) FROM profiles)
			FROM system_config WHERE id = 1`).Scan(
			&deletedRetention, &renditionRetention, &timezone, &interval, &backupRetention, &profiles,
		); err != nil {
			t.Fatalf("read initial config: %v", err)
		}
		if deletedRetention != nil || renditionRetention != nil || timezone != "Asia/Tokyo" || interval != 24 || backupRetention != 30 {
			t.Fatalf("unexpected initial config: deleted=%v rendition=%v timezone=%q interval=%d retention=%d",
				deletedRetention, renditionRetention, timezone, interval, backupRetention)
		}
		if profiles != 2 {
			t.Fatalf("profile seed count = %d, want 2", profiles)
		}
		assertBundledProfileSeeds(t, pool)
		var position int64
		var mode string
		if err := pool.QueryRow(ctx, `SELECT last_position FROM change_feed_state WHERE id=1`).Scan(&position); err != nil {
			t.Fatalf("read feed seed: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT mode FROM maintenance_state WHERE id=1`).Scan(&mode); err != nil {
			t.Fatalf("read maintenance seed: %v", err)
		}
		if position != 0 || mode != "normal" {
			t.Fatalf("singleton seeds = position %d, mode %q", position, mode)
		}

		expectExecError(t, pool, `INSERT INTO system_config (id, default_timezone, db_backup_interval_hours, db_backup_retention_days) VALUES (2, 'UTC', 24, 30)`)
		expectExecError(t, pool, `UPDATE system_config SET default_timezone='Not/A_Real_Zone' WHERE id=1`)
		expectExecError(t, pool, `UPDATE system_config SET deleted_media_retention_days=-1 WHERE id=1`)
		expectExecError(t, pool, `UPDATE system_config SET db_backup_interval_hours=0 WHERE id=1`)
		expectExecError(t, pool, `DELETE FROM system_config WHERE id=1`)
		expectExecError(t, pool, `TRUNCATE system_config`)
		expectExecError(t, pool, `DELETE FROM maintenance_state WHERE id=1`)
		if _, err := pool.Exec(ctx, `UPDATE change_feed_state SET last_position=5 WHERE id=1`); err != nil {
			t.Fatalf("advance change feed position: %v", err)
		}
		expectExecError(t, pool, `UPDATE change_feed_state SET last_position=4 WHERE id=1`)
		expectExecError(t, pool, `DELETE FROM change_feed_state WHERE id=1`)
		expectExecError(t, pool, `TRUNCATE change_feed_state`)
		if _, err := pool.Exec(ctx, `UPDATE system_config SET default_timezone='UTC', deleted_media_retention_days=0 WHERE id=1`); err != nil {
			t.Fatalf("valid config update: %v", err)
		}
	})

	t.Run("profile definitions require exact recipes and certified activation", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		valid := testProfileParameters(t)
		profileID := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,'capability-check',1,'draft',ARRAY['image/jpeg'],'nmcp-media',1,$2::jsonb)`, profileID, valid); err != nil {
			t.Fatalf("insert valid candidate draft: %v", err)
		}
		expectExecError(t, pool, `UPDATE profiles SET status='active' WHERE id=$1`, profileID)
		if _, err := pool.Exec(ctx, testJPEGCertificationSQL); err != nil {
			t.Fatalf("certify JPEG fixture capability: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, profileID); err != nil {
			t.Fatalf("activate certified profile: %v", err)
		}

		customID := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,'family-wall',1,'draft',ARRAY['image/jpeg'],'nmcp-media',1,$2::jsonb)`, customID, valid); err != nil {
			t.Fatalf("custom key did not reuse profile validation: %v", err)
		}

		insert := `INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,$2,1,'draft',$3,$4,$5,$6::jsonb)`
		expectBothReject := func(key string, mimeTypes []string, processorName string, schemaVersion int, parameters []byte) {
			t.Helper()
			definition := profile.Definition{
				ID: newUUIDv4(t), Key: key, Version: 1, InputMIMETypes: mimeTypes,
				Processor: processorName, ParametersSchemaVersion: schemaVersion, Parameters: parameters,
			}
			if err := profile.ValidateDraft(definition); err == nil {
				t.Fatalf("Go validator accepted %s", key)
			}
			expectExecError(t, pool, insert, definition.ID, key, mimeTypes, processorName, schemaVersion, parameters)
		}
		expectBothReject("bad-wildcard", []string{"image/*"}, "nmcp-media", 1, testProfileParametersForRecipe(t, "image/*", "image/jpeg"))
		expectBothReject("bad-unknown-mime", []string{"image/x-unknown"}, "nmcp-media", 1, testProfileParametersForRecipe(t, "image/x-unknown", "image/jpeg"))
		expectBothReject("bad-processor", []string{"image/jpeg"}, "unknown", 1, valid)
		expectBothReject("bad-schema", []string{"image/jpeg"}, "nmcp-media", 2, valid)

		var parameters map[string]any
		if err := json.Unmarshal(valid, &parameters); err != nil {
			t.Fatal(err)
		}
		parameters["unexpected"] = true
		expectBothReject("bad-top-field", []string{"image/jpeg"}, "nmcp-media", 1, mustJSON(t, parameters))

		parameters = decodeJSONMap(t, valid)
		parameters["recipes"] = map[string]any{}
		expectBothReject("bad-missing", []string{"image/jpeg"}, "nmcp-media", 1, mustJSON(t, parameters))

		parameters = decodeJSONMap(t, valid)
		recipes := parameters["recipes"].(map[string]any)
		recipes["image/png"] = recipes["image/jpeg"]
		expectBothReject("bad-extra", []string{"image/jpeg"}, "nmcp-media", 1, mustJSON(t, parameters))

		parameters = decodeJSONMap(t, valid)
		recipe := parameters["recipes"].(map[string]any)["image/jpeg"].(map[string]any)
		recipe["still_output"].(map[string]any)["quality"] = 0
		expectBothReject("bad-quality", []string{"image/jpeg"}, "nmcp-media", 1, mustJSON(t, parameters))

		parameters = decodeJSONMap(t, valid)
		parameters["recipes"].(map[string]any)["image/jpeg"].(map[string]any)["source_mode"] = "video"
		expectBothReject("bad-capability-pair", []string{"image/jpeg"}, "nmcp-media", 1, mustJSON(t, parameters))

		for name, mutate := range map[string]func(map[string]any){
			"null-evidence": func(value map[string]any) { value["evidence_status"] = nil },
			"null-upscale": func(value map[string]any) {
				value["recipes"].(map[string]any)["image/jpeg"].(map[string]any)["allow_upscale"] = nil
			},
			"null-crop": func(value map[string]any) {
				value["recipes"].(map[string]any)["image/jpeg"].(map[string]any)["crop"] = nil
			},
			"null-quality": func(value map[string]any) {
				value["recipes"].(map[string]any)["image/jpeg"].(map[string]any)["still_output"].(map[string]any)["quality"] = nil
			},
		} {
			parameters := decodeJSONMap(t, valid)
			mutate(parameters)
			expectBothReject("bad-"+name, []string{"image/jpeg"}, "nmcp-media", 1, mustJSON(t, parameters))
		}

		gif := testProfileParametersForRecipe(t, "image/gif", "image/gif")
		parameters = decodeJSONMap(t, gif)
		parameters["recipes"].(map[string]any)["image/gif"].(map[string]any)["animation_timing"] = "discard"
		expectBothReject("bad-animation-combination", []string{"image/gif"}, "nmcp-media", 1, mustJSON(t, parameters))

		video := testProfileParametersForRecipe(t, "video/mp4", "video/mp4")
		parameters = decodeJSONMap(t, video)
		parameters["recipes"].(map[string]any)["video/mp4"].(map[string]any)["audio"] = "none"
		expectBothReject("bad-video-combination", []string{"video/mp4"}, "nmcp-media", 1, mustJSON(t, parameters))
		expectBothReject("bad-case-variant-field", []string{"image/jpeg"}, "nmcp-media", 1, replaceJSONOnce(t, valid, `"quality":60`, `"quality":60,"Quality":60`))
		validObject := decodeJSONMap(t, valid)
		validRecipes := mustJSON(t, validObject["recipes"])
		duplicateRecipes := []byte(`{"evidence_status":"provisional-unverified","recipes":` + string(validRecipes) + `,"recipes":{}}`)
		expectBothReject("bad-duplicate-recipes-last-empty", []string{"image/jpeg"}, "nmcp-media", 1, duplicateRecipes)
		for _, test := range []struct {
			key        string
			parameters []byte
		}{
			{key: "bad-discarded-nul", parameters: replaceJSONOnce(t, valid, `"evidence_status":"provisional-unverified"`, `"evidence_status":"\u0000","evidence_status":"provisional-unverified"`)},
			{key: "bad-discarded-surrogate", parameters: replaceJSONOnce(t, valid, `"evidence_status":"provisional-unverified"`, `"evidence_status":"\uD800","evidence_status":"provisional-unverified"`)},
			{key: "bad-discarded-numeric-overflow", parameters: replaceJSONOnce(t, valid, `"quality":60`, `"quality":1e1000000,"quality":60`)},
			{key: "bad-discarded-invalid-utf8", parameters: replaceJSONBytesOnce(t, valid, []byte(`"evidence_status":"provisional-unverified"`), append(append([]byte(`"evidence_status":"`), 0xff), []byte(`","evidence_status":"provisional-unverified"`)...))},
		} {
			expectBothReject(test.key, []string{"image/jpeg"}, "nmcp-media", 1, test.parameters)
		}

		for _, test := range []struct {
			key, mimeType string
			parameters    []byte
		}{
			{key: "bad-still-decimal-bit-depth", mimeType: "image/jpeg", parameters: replaceJSONOnce(t, valid, `"bit_depth":8`, `"bit_depth":8.0`)},
			{key: "bad-animation-decimal-bit-depth", mimeType: "image/gif", parameters: replaceJSONOnce(t, gif, `"animation_output":{"format":"animated-webp","quality":80,"bit_depth":8}`, `"animation_output":{"format":"animated-webp","quality":80,"bit_depth":8.0}`)},
			{key: "bad-video-decimal-bit-depth", mimeType: "video/mp4", parameters: replaceJSONOnce(t, video, `"bit_depth":10`, `"bit_depth":10.0`)},
			{key: "bad-video-decimal-audio-bitrate", mimeType: "video/mp4", parameters: replaceJSONOnce(t, video, `"audio_bitrate_kbps":128`, `"audio_bitrate_kbps":128.0`)},
			{key: "bad-overflow-numeric-exponent", mimeType: "video/mp4", parameters: replaceJSONOnce(t, video, `"crf":32`, `"crf":0e1073741824`)},
		} {
			expectBothReject(test.key, []string{test.mimeType}, "nmcp-media", 1, test.parameters)
		}

		exponentParameters := replaceJSONOnce(t, valid, `"bit_depth":8`, `"bit_depth":8e0`)
		exponentDefinition := profile.Definition{
			ID: newUUIDv4(t), Key: "jsonb-exponent", Version: 1,
			InputMIMETypes: []string{"image/jpeg"}, Processor: "nmcp-media",
			ParametersSchemaVersion: 1, Parameters: exponentParameters,
		}
		if err := profile.ValidateDraft(exponentDefinition); err != nil {
			t.Fatalf("Go rejected integer exponent accepted by jsonb: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,$2,$3,'draft',$4,$5,$6,$7::jsonb)`, exponentDefinition.ID, exponentDefinition.Key,
			exponentDefinition.Version, exponentDefinition.InputMIMETypes, exponentDefinition.Processor,
			exponentDefinition.ParametersSchemaVersion, exponentDefinition.Parameters); err != nil {
			t.Fatalf("PostgreSQL rejected jsonb-canonical integer exponent: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, exponentDefinition.ID); err != nil {
			t.Fatalf("activate jsonb-canonical integer exponent profile: %v", err)
		}

		boundaryExponentParameters := replaceJSONOnce(t, video, `"crf":32`, `"crf":0e1073741823`)
		boundaryExponentDefinition := profile.Definition{
			ID: newUUIDv4(t), Key: "max-numeric-exponent", Version: 1,
			InputMIMETypes: []string{"video/mp4"}, Processor: "nmcp-media",
			ParametersSchemaVersion: 1, Parameters: boundaryExponentParameters,
		}
		if err := profile.ValidateDraft(boundaryExponentDefinition); err != nil {
			t.Fatalf("Go rejected maximum PostgreSQL numeric exponent: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,$2,$3,'draft',$4,$5,$6,$7::jsonb)`, boundaryExponentDefinition.ID, boundaryExponentDefinition.Key,
			boundaryExponentDefinition.Version, boundaryExponentDefinition.InputMIMETypes, boundaryExponentDefinition.Processor,
			boundaryExponentDefinition.ParametersSchemaVersion, boundaryExponentDefinition.Parameters); err != nil {
			t.Fatalf("PostgreSQL rejected maximum numeric exponent: %v", err)
		}

		maxVersionDefinition := profile.Definition{
			ID: newUUIDv4(t), Key: "max-version", Version: math.MaxInt32,
			InputMIMETypes: []string{"image/jpeg"}, Processor: "nmcp-media",
			ParametersSchemaVersion: 1, Parameters: valid,
		}
		if err := profile.ValidateDraft(maxVersionDefinition); err != nil {
			t.Fatalf("Go rejected PostgreSQL integer maximum version: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,$2,$3,'draft',$4,$5,$6,$7::jsonb)`, maxVersionDefinition.ID, maxVersionDefinition.Key,
			maxVersionDefinition.Version, maxVersionDefinition.InputMIMETypes, maxVersionDefinition.Processor,
			maxVersionDefinition.ParametersSchemaVersion, maxVersionDefinition.Parameters); err != nil {
			t.Fatalf("PostgreSQL rejected maximum integer version: %v", err)
		}
		overflowVersionDefinition := maxVersionDefinition
		overflowVersionDefinition.ID = newUUIDv4(t)
		overflowVersionDefinition.Key = "overflow-version"
		overflowVersionDefinition.Version = math.MaxInt32 + 1
		if err := profile.ValidateDraft(overflowVersionDefinition); err == nil {
			t.Fatal("Go accepted version beyond PostgreSQL integer range")
		}
		expectExecError(t, pool, `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,$2,$3,'draft',$4,$5,$6,$7::jsonb)`, overflowVersionDefinition.ID, overflowVersionDefinition.Key,
			int64(overflowVersionDefinition.Version), overflowVersionDefinition.InputMIMETypes, overflowVersionDefinition.Processor,
			overflowVersionDefinition.ParametersSchemaVersion, overflowVersionDefinition.Parameters)
	})

	t.Run("profile certification is an immutable option envelope", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		profileID := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `
			INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES ($1,'envelope',1,'draft',ARRAY['image/jpeg'],'nmcp-media',1,$2::jsonb)`, profileID, testProfileParameters(t)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO profile_processor_certifications (
				id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
				max_long_edge,minimum_setting,maximum_setting,evidence
			) VALUES ($1,'nmcp-media',1,'image/jpeg','still','still-avif',640,1,50,'deliberately narrow test envelope')`, newUUIDv4(t)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO profile_processor_certifications (
				id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
				max_long_edge,minimum_setting,maximum_setting,evidence
			) VALUES ($1,'nmcp-media',1,'image/jpeg','still','still-avif',4096,61,100,'second independent option envelope')`, newUUIDv4(t)); err != nil {
			t.Fatalf("append second immutable envelope: %v", err)
		}
		expectExecError(t, pool, `UPDATE profiles SET status='active' WHERE id=$1`, profileID)
		expectExecError(t, pool, `UPDATE profile_processor_certifications SET max_long_edge=4096 WHERE input_mime_type='image/jpeg'`)
		expectExecError(t, pool, `DELETE FROM profile_processor_certifications WHERE input_mime_type='image/jpeg'`)
		expectExecError(t, pool, `TRUNCATE profile_processor_certifications`)
		expectExecError(t, pool, `
			INSERT INTO profile_processor_certifications (
				id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
				max_long_edge,minimum_setting,maximum_setting,evidence
			) VALUES ($1,'nmcp-media',1,'image/jpeg','still','video-av1',4096,0,63,'wrong output kind')`, newUUIDv4(t))
		expectExecError(t, pool, `
			INSERT INTO profile_processor_certifications (
				id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
				max_long_edge,minimum_setting,maximum_setting,evidence
			) VALUES ($1,'nmcp-media',1,'image/jpeg','still','still-avif',4096,1,100,'   ')`, newUUIDv4(t))
		expectExecError(t, pool, `
			INSERT INTO profile_processor_certifications (
				id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
				max_long_edge,minimum_setting,maximum_setting,evidence
			) VALUES ($1,'nmcp-media',1,'image/jpeg','still','still-avif',4096,1,100,' provisional-unverified ')`, newUUIDv4(t))
		expectExecError(t, pool, `
			INSERT INTO profile_processor_certifications (
				id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
				max_long_edge,minimum_setting,maximum_setting,evidence
			) VALUES ($1,'nmcp-media',1,'image/jpeg','still','still-avif',4096,1,100,E'\t\n')`, newUUIDv4(t))
		expectExecError(t, pool, `
			INSERT INTO profile_processor_certifications (
				id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
				max_long_edge,minimum_setting,maximum_setting,evidence
			) VALUES ($1,'nmcp-media',1,'image/jpeg','still','still-avif',4096,1,100,E'\nprovisional-unverified\t')`, newUUIDv4(t))
		expectExecError(t, pool, `
			INSERT INTO profile_processor_certifications (
				id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
				max_long_edge,minimum_setting,maximum_setting,evidence
			) VALUES ($1,'nmcp-media',1,'image/jpeg','still','still-avif',4096,1,100,E'\x0B')`, newUUIDv4(t))
	})

	t.Run("animation and video activation require recipe-specific certifications", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		insertProfile := func(key, mimeType string) string {
			t.Helper()
			id := newUUIDv4(t)
			if _, err := pool.Exec(ctx, `
				INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
				VALUES ($1,$2,1,'draft',ARRAY[$3],'nmcp-media',1,$4::jsonb)`,
				id, key, mimeType, testProfileParametersForRecipe(t, mimeType, mimeType)); err != nil {
				t.Fatalf("insert %s profile: %v", mimeType, err)
			}
			return id
		}
		certify := func(mimeType, sourceMode, outputKind string, minimum, maximum int) {
			t.Helper()
			if _, err := pool.Exec(ctx, `
				INSERT INTO profile_processor_certifications (
					id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
					max_long_edge,minimum_setting,maximum_setting,evidence
				) VALUES ($1,'nmcp-media',1,$2,$3,$4,4096,$5,$6,'isolated recipe-specific PostgreSQL fixture')`,
				newUUIDv4(t), mimeType, sourceMode, outputKind, minimum, maximum); err != nil {
				t.Fatalf("certify %s/%s: %v", mimeType, outputKind, err)
			}
		}

		gifID := insertProfile("gif-certification", "image/gif")
		expectExecError(t, pool, `UPDATE profiles SET status='active' WHERE id=$1`, gifID)
		certify("image/gif", "probe-animation", "still-avif", 1, 100)
		expectExecError(t, pool, `UPDATE profiles SET status='active' WHERE id=$1`, gifID)
		certify("image/gif", "probe-animation", "animation-webp", 1, 100)
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, gifID); err != nil {
			t.Fatalf("activate fully certified animation profile: %v", err)
		}

		videoID := insertProfile("video-certification", "video/mp4")
		expectExecError(t, pool, `UPDATE profiles SET status='active' WHERE id=$1`, videoID)
		certify("video/mp4", "video", "video-av1", 0, 63)
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, videoID); err != nil {
			t.Fatalf("activate certified video profile: %v", err)
		}
	})

	t.Run("media original uniqueness and checks", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		mediaOne := newUUIDv4(t)
		mediaTwo := newUUIDv4(t)
		insertMedia(t, pool, mediaOne)
		insertMedia(t, pool, mediaTwo)

		expectExecError(t, pool, `INSERT INTO media (id,media_type,taken_at,taken_at_source) VALUES ($1,'image',now(),'unknown')`, newUUIDv4(t))
		expectExecError(t, pool, `INSERT INTO media (id,media_type,taken_at,taken_at_source,taken_at_timezone) VALUES ($1,'image',now(),'embedded_offset','UTC')`, newUUIDv4(t))
		expectExecError(t, pool, `INSERT INTO media (id,media_type,taken_at,taken_at_source,taken_at_timezone) VALUES ($1,'image',now(),'default_timezone','Not/A_Real_Zone')`, newUUIDv4(t))
		expectExecError(t, pool, `UPDATE media SET purge_after=now() WHERE id=$1`, mediaOne)
		expectExecError(t, pool, `INSERT INTO media (id,media_type,taken_at_source) VALUES ('00000000-0000-0000-0000-000000000000','image','unknown')`)

		sha := strings.Repeat("a", 64)
		if _, err := pool.Exec(ctx, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
			VALUES ($1,$2,$3,$4,'image/jpeg',1,1,1)`, newUUIDv4(t), mediaOne, sha, "originals/aa/one/original.jpg"); err != nil {
			t.Fatalf("insert original: %v", err)
		}
		expectExecError(t, pool, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
			VALUES ($1,$2,$3,$4,'image/jpeg',1)`, newUUIDv4(t), mediaTwo, sha, "originals/bb/two/original.jpg")
		expectExecError(t, pool, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
			VALUES ($1,$2,$3,$4,'IMAGE/JPEG',1,1,NULL)`, newUUIDv4(t), mediaTwo, strings.Repeat("B", 64), "/absolute")
		expectExecError(t, pool, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes,width,height)
			VALUES ($1,$2,$3,$4,'image/jpeg',1,1,NULL)`, newUUIDv4(t), mediaTwo, strings.Repeat("b", 64), "originals/bb/dimensions/original.jpg")
		expectExecError(t, pool, `UPDATE originals SET media_id=$1 WHERE media_id=$2`, mediaTwo, mediaOne)
		expectExecError(t, pool, `DELETE FROM originals WHERE media_id=$1`, mediaOne)
		expectExecError(t, pool, `TRUNCATE originals CASCADE`)

		if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at=now(), purge_after=now() WHERE id=$1`, mediaOne); err != nil {
			t.Fatalf("logically delete media: %v", err)
		}
		expectExecError(t, pool, `
			INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
			VALUES ($1,$2,$3,$4,'image/jpeg',1)`, newUUIDv4(t), mediaTwo, sha, "originals/bb/two/original.jpg")

		concurrentMediaOne := newUUIDv4(t)
		concurrentMediaTwo := newUUIDv4(t)
		insertMedia(t, pool, concurrentMediaOne)
		insertMedia(t, pool, concurrentMediaTwo)
		start := make(chan struct{})
		errorsByInsert := make(chan error, 2)
		originalIDs := []string{newUUIDv4(t), newUUIDv4(t)}
		for index, id := range []string{concurrentMediaOne, concurrentMediaTwo} {
			go func(index int, id string) {
				<-start
				_, err := pool.Exec(ctx, `INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes) VALUES ($1,$2,$3,$4,'image/jpeg',1)`,
					originalIDs[index], id, strings.Repeat("9", 64), fmt.Sprintf("originals/99/concurrent-%d/original.jpg", index))
				errorsByInsert <- err
			}(index, id)
		}
		close(start)
		assertOneConcurrentWinner(t, errorsByInsert)
	})

	t.Run("profile activation is monotonic and immutable", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		profileOne := insertDraftProfile(t, pool, "test-standard", 1)
		profileTwo := insertDraftProfile(t, pool, "test-standard", 2)
		profileThree := insertDraftProfile(t, pool, "test-standard", 3)

		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, profileOne); err != nil {
			t.Fatalf("activate v1: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, profileThree); err != nil {
			t.Fatalf("activate v3: %v", err)
		}
		expectExecError(t, pool, `UPDATE profiles SET status='active' WHERE id=$1`, profileTwo)
		changedParameters := decodeJSONMap(t, testProfileParameters(t))
		changedParameters["recipes"].(map[string]any)["image/jpeg"].(map[string]any)["still_output"].(map[string]any)["quality"] = 61
		expectExecError(t, pool, `UPDATE profiles SET parameters=$2::jsonb WHERE id=$1`, profileThree, mustJSON(t, changedParameters))
		expectExecError(t, pool, `UPDATE profiles SET status='draft' WHERE id=$1`, profileOne)
		expectExecError(t, pool, `INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters,activated_at) VALUES ($1,'bad',1,'active',ARRAY['image/jpeg'],'nmcp-media',1,$2::jsonb,now())`, newUUIDv4(t), testProfileParameters(t))
		expectExecError(t, pool, `INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters) VALUES ($1,'bad',1,'draft',ARRAY['image/jpeg','image/jpeg'],'nmcp-media',1,$2::jsonb)`, newUUIDv4(t), testProfileParameters(t))

		var activeVersion int
		if err := pool.QueryRow(ctx, `SELECT version FROM profiles WHERE key='test-standard' AND status='active'`).Scan(&activeVersion); err != nil {
			t.Fatalf("read active profile: %v", err)
		}
		if activeVersion != 3 {
			t.Fatalf("active version = %d, want 3", activeVersion)
		}
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='retired' WHERE id=$1`, profileThree); err != nil {
			t.Fatalf("retire v3 before shadow regression: %v", err)
		}
		expectExecError(t, pool, `DELETE FROM profiles WHERE id=$1`, profileThree)
		expectExecError(t, pool, `TRUNCATE profiles CASCADE`)

		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire temp-shadow connection: %v", err)
		}
		defer conn.Release()
		var schemaName string
		if err := conn.QueryRow(ctx, `SELECT current_schema()`).Scan(&schemaName); err != nil {
			t.Fatalf("read isolated schema name: %v", err)
		}
		if _, err := conn.Exec(ctx, `CREATE TEMP TABLE profiles (id uuid, key text, version integer, status text, activated_at timestamptz)`); err != nil {
			t.Fatalf("create shadow profiles table: %v", err)
		}
		realProfiles := pgx.Identifier{schemaName, "profiles"}.Sanitize()
		if _, err := conn.Exec(ctx, `UPDATE `+realProfiles+` SET status='active' WHERE id=$1`, profileTwo); err == nil {
			t.Fatal("temporary profiles shadow bypassed monotonic activation")
		}

		concurrentTwo := insertDraftProfile(t, pool, "concurrent", 2)
		concurrentThree := insertDraftProfile(t, pool, "concurrent", 3)
		highTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin high activation: %v", err)
		}
		if _, err := highTx.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, concurrentThree); err != nil {
			_ = highTx.Rollback(ctx)
			t.Fatalf("stage high activation: %v", err)
		}
		lowResult := make(chan error, 1)
		go func() {
			_, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, concurrentTwo)
			lowResult <- err
		}()
		if err := highTx.Commit(ctx); err != nil {
			t.Fatalf("commit high activation: %v", err)
		}
		if err := awaitResult(t, lowResult); err == nil {
			t.Fatal("lower concurrent activation unexpectedly succeeded after higher version")
		}

		orderedTwo := insertDraftProfile(t, pool, "ordered", 2)
		orderedThree := insertDraftProfile(t, pool, "ordered", 3)
		lowTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin low activation: %v", err)
		}
		if _, err := lowTx.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, orderedTwo); err != nil {
			_ = lowTx.Rollback(ctx)
			t.Fatalf("stage low activation: %v", err)
		}
		highResult := make(chan error, 1)
		go func() {
			_, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, orderedThree)
			highResult <- err
		}()
		if err := lowTx.Commit(ctx); err != nil {
			t.Fatalf("commit low activation: %v", err)
		}
		if err := awaitResult(t, highResult); err != nil {
			t.Fatalf("higher concurrent activation after lower commit: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT version FROM profiles WHERE key='ordered' AND status='active'`).Scan(&activeVersion); err != nil || activeVersion != 3 {
			t.Fatalf("ordered concurrent active version = %d, err=%v", activeVersion, err)
		}
	})

	t.Run("concurrent current-rendition uniqueness", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		profileID := insertDraftProfile(t, pool, "rendition-standard", 1)
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, profileID); err != nil {
			t.Fatalf("activate profile: %v", err)
		}
		mediaID := newUUIDv4(t)
		insertMedia(t, pool, mediaID)
		originalID := insertOriginal(t, pool, mediaID, "8", "concurrent")
		targetOne := insertPendingTransform(t, pool, mediaID, originalID, profileID)
		targetTwo := insertPendingTransform(t, pool, mediaID, originalID, profileID)

		start := make(chan struct{})
		results := make(chan error, 2)
		renditionIDs := []string{newUUIDv4(t), newUUIDv4(t)}
		for index, targetID := range []string{targetOne, targetTwo} {
			go func(index int, targetID string) {
				<-start
				tx, err := pool.Begin(ctx)
				if err == nil {
					_, err = tx.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, targetID)
				}
				if err == nil {
					_, err = tx.Exec(ctx, `INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256) VALUES ($1,$2,$3,'ignored',true,$4,'image/avif',1,$5)`,
						renditionIDs[index], mediaID, targetID, fmt.Sprintf("renditions/88/concurrent/target-%d/output.avif", index), strings.Repeat(fmt.Sprintf("%x", index+6), 64))
				}
				if err == nil {
					err = tx.Commit(ctx)
				} else if tx != nil {
					_ = tx.Rollback(ctx)
				}
				results <- err
			}(index, targetID)
		}
		close(start)
		assertOneConcurrentWinner(t, results)
		var currentCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM renditions WHERE media_id=$1 AND profile_key='rendition-standard' AND is_current`, mediaID).Scan(&currentCount); err != nil {
			t.Fatalf("count current renditions: %v", err)
		}
		if currentCount != 1 {
			t.Fatalf("current rendition count = %d, want 1", currentCount)
		}
	})

	t.Run("job target rendition and purge history invariants", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		profileID := insertDraftProfile(t, pool, "history-standard", 1)
		if _, err := pool.Exec(ctx, `UPDATE profiles SET status='active' WHERE id=$1`, profileID); err != nil {
			t.Fatalf("activate profile: %v", err)
		}

		mediaID := newUUIDv4(t)
		otherMediaID := newUUIDv4(t)
		insertMedia(t, pool, mediaID)
		insertMedia(t, pool, otherMediaID)
		originalID := insertOriginal(t, pool, mediaID, "c", "first")
		otherOriginalID := insertOriginal(t, pool, otherMediaID, "d", "second")
		expectExecError(t, pool, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, newUUIDv4(t), otherOriginalID, mediaID)
		expectExecError(t, pool, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,$3,'queued',3)`, newUUIDv4(t), originalID, mediaID)

		expectTxCommitError(t, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, newUUIDv4(t), originalID, mediaID)
			return err
		})

		purgeID := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, purgeID, mediaID); err != nil {
			t.Fatalf("insert purge job: %v", err)
		}
		expectTxCommitError(t, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, newUUIDv4(t), purgeID, profileID)
			return err
		})
		expectExecError(t, pool, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, newUUIDv4(t), mediaID)
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='cancelled', finished_at=now(), cancelled_at=now(), cancel_reason='media_restored' WHERE id=$1`, purgeID); err != nil {
			t.Fatalf("cancel purge: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, newUUIDv4(t), mediaID); err != nil {
			t.Fatalf("new purge after cancellation: %v", err)
		}

		lifecycleJobID := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts) VALUES ($1,'purge',$2,'queued',3)`, lifecycleJobID, otherMediaID); err != nil {
			t.Fatalf("insert lifecycle purge: %v", err)
		}
		expectExecError(t, pool, `INSERT INTO jobs (id,type,media_id_snapshot,status,max_attempts,attempts,started_at,finished_at) VALUES ($1,'purge',$2,'succeeded',3,1,now(),now())`, newUUIDv4(t), otherMediaID)
		leaseOne := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running', attempts=1, lease_token=$2, lease_expires_at=now()+interval '1 minute', started_at=now() WHERE id=$1`, lifecycleJobID, leaseOne); err != nil {
			t.Fatalf("claim lifecycle purge: %v", err)
		}
		expectExecError(t, pool, `UPDATE jobs SET status='cancelled', started_at=NULL, lease_token=NULL, lease_expires_at=NULL, finished_at=now(), cancelled_at=now(), cancel_reason='media_restored' WHERE id=$1`, lifecycleJobID)
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='queued', lease_token=NULL, lease_expires_at=NULL WHERE id=$1`, lifecycleJobID); err != nil {
			t.Fatalf("backoff lifecycle purge: %v", err)
		}
		expectExecError(t, pool, `UPDATE jobs SET status='cancelled', finished_at=now(), cancelled_at=now(), cancel_reason='media_restored' WHERE id=$1`, lifecycleJobID)
		expectExecError(t, pool, `UPDATE jobs SET attempts=0 WHERE id=$1`, lifecycleJobID)
		expectExecError(t, pool, `UPDATE jobs SET max_attempts=2 WHERE id=$1`, lifecycleJobID)
		leaseTwo := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running', attempts=2, lease_token=$2, lease_expires_at=now()+interval '1 minute' WHERE id=$1`, lifecycleJobID, leaseTwo); err != nil {
			t.Fatalf("reclaim lifecycle purge: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET status='succeeded', lease_token=NULL, lease_expires_at=NULL, finished_at=now() WHERE id=$1`, lifecycleJobID); err != nil {
			t.Fatalf("finish lifecycle purge: %v", err)
		}
		expectExecError(t, pool, `UPDATE jobs SET status='queued', finished_at=NULL WHERE id=$1`, lifecycleJobID)
		expectExecError(t, pool, `DELETE FROM jobs WHERE id=$1`, lifecycleJobID)

		jobID := newUUIDv4(t)
		targetID := newUUIDv4(t)
		renditionID := newUUIDv4(t)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin publication: %v", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profileID)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE jobs SET status='running', attempts=1, lease_token=$2, lease_expires_at=now()+interval '1 minute', started_at=now() WHERE id=$1`, jobID, newUUIDv4(t))
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, targetID)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256,width,height) VALUES ($1,$2,$3,'ignored',true,$4,'image/avif',10,$5,1,1)`, renditionID, mediaID, targetID, "renditions/cc/one/target/output.avif", strings.Repeat("e", 64))
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE jobs SET status='succeeded', lease_token=NULL, lease_expires_at=NULL, finished_at=now() WHERE id=$1`, jobID)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("publication statements: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit publication: %v", err)
		}

		var profileKey string
		if err := pool.QueryRow(ctx, `SELECT profile_key FROM renditions WHERE id=$1`, renditionID).Scan(&profileKey); err != nil || profileKey != "history-standard" {
			t.Fatalf("derived rendition profile key = %q, err=%v", profileKey, err)
		}
		expectExecError(t, pool, `INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256) VALUES ($1,$2,$3,'standard',false,$4,'image/avif',1,$5)`, newUUIDv4(t), otherMediaID, targetID, "renditions/dd/bad/target/output.avif", strings.Repeat("f", 64))
		expectExecError(t, pool, `UPDATE job_targets SET status='failed', error_code='late', error_message='late edit' WHERE id=$1`, targetID)
		expectExecError(t, pool, `DELETE FROM job_targets WHERE id=$1`, targetID)

		dimensionTarget := insertPendingTransform(t, pool, mediaID, originalID, profileID)
		expectTxCommitError(t, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='succeeded' WHERE id=$1`, dimensionTarget); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO renditions (id,media_id,job_target_id,profile_key,is_current,relative_path,mime_type,size_bytes,sha256,width,height) VALUES ($1,$2,$3,'ignored',false,$4,'image/avif',1,$5,1,NULL)`,
				newUUIDv4(t), mediaID, dimensionTarget, "renditions/cc/one/dimension/output.avif", strings.Repeat("7", 64))
			return err
		})

		secondProfileID := insertDraftProfile(t, pool, "history-thumbnail", 1)
		deleteJobID := newUUIDv4(t)
		deleteTargets := []string{newUUIDv4(t), newUUIDv4(t)}
		deleteTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin delete-race fixture: %v", err)
		}
		if _, err = deleteTx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, deleteJobID, originalID, mediaID); err == nil {
			_, err = deleteTx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$3,$4,'pending'),($2,$3,$5,'pending')`, deleteTargets[0], deleteTargets[1], deleteJobID, profileID, secondProfileID)
		}
		if err != nil {
			_ = deleteTx.Rollback(ctx)
			t.Fatalf("create delete-race fixture: %v", err)
		}
		if err := deleteTx.Commit(ctx); err != nil {
			t.Fatalf("commit delete-race fixture: %v", err)
		}
		deleteStart := make(chan struct{})
		deleteResults := make(chan error, 2)
		for _, deleteTarget := range deleteTargets {
			go func(target string) {
				<-deleteStart
				_, err := pool.Exec(ctx, `DELETE FROM job_targets WHERE id=$1`, target)
				deleteResults <- err
			}(deleteTarget)
		}
		close(deleteStart)
		assertConcurrentFailures(t, deleteResults, 2)
		expectExecError(t, pool, `DELETE FROM jobs WHERE id=$1`, deleteJobID)
		expectExecError(t, pool, `UPDATE jobs SET original_id=NULL WHERE id=$1`, deleteJobID)
		expectExecError(t, pool, `TRUNCATE job_targets CASCADE`)
		expectExecError(t, pool, `TRUNCATE jobs CASCADE`)

		if _, err := pool.Exec(ctx, `DELETE FROM media WHERE id=$1`, mediaID); err != nil {
			t.Fatalf("physically purge media: %v", err)
		}
		var originalReference *string
		var targetCount, renditionCount int
		if err := pool.QueryRow(ctx, `SELECT original_id::text FROM jobs WHERE id=$1`, jobID).Scan(&originalReference); err != nil {
			t.Fatalf("read preserved job: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_targets WHERE id=$1`, targetID).Scan(&targetCount); err != nil {
			t.Fatalf("count preserved target: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM renditions WHERE id=$1`, renditionID).Scan(&renditionCount); err != nil {
			t.Fatalf("count cascaded rendition: %v", err)
		}
		if originalReference != nil || targetCount != 1 || renditionCount != 0 {
			t.Fatalf("purge history = original %v target %d rendition %d", originalReference, targetCount, renditionCount)
		}
	})

	t.Run("durable state is constrained and immutable", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		mediaID := newUUIDv4(t)
		requestHash := strings.Repeat("1", 64)
		if _, err := pool.Exec(ctx, `INSERT INTO idempotency_requests (scope,key,request_hash,http_status,response_body,media_id_snapshot) VALUES ('POST /media','request-1',$1,201,'{}',$2)`, requestHash, mediaID); err != nil {
			t.Fatalf("insert idempotency row: %v", err)
		}
		expectExecError(t, pool, `UPDATE idempotency_requests SET http_status=409 WHERE scope='POST /media' AND key='request-1'`)
		expectExecError(t, pool, `INSERT INTO idempotency_requests (scope,key,request_hash,http_status,response_body) VALUES ('POST /other','x',$1,201,'{}')`, requestHash)
		start := make(chan struct{})
		idempotencyResults := make(chan error, 2)
		idempotencyMediaIDs := []string{newUUIDv4(t), newUUIDv4(t)}
		for index := 0; index < 2; index++ {
			go func(index int) {
				<-start
				_, err := pool.Exec(ctx, `INSERT INTO idempotency_requests (scope,key,request_hash,http_status,response_body,media_id_snapshot) VALUES ('POST /media','concurrent',$1,201,$2,$3)`,
					strings.Repeat(fmt.Sprintf("%x", index+3), 64), fmt.Sprintf(`{"winner":%d}`, index), idempotencyMediaIDs[index])
				idempotencyResults <- err
			}(index)
		}
		close(start)
		assertOneConcurrentWinner(t, idempotencyResults)

		eventID := newUUIDv4(t)
		if _, err := pool.Exec(ctx, `INSERT INTO change_events (id,position,event_type,reason,media_id,payload) VALUES ($1,1,'media_upsert','upload',$2,'{}')`, eventID, mediaID); err != nil {
			t.Fatalf("insert change event: %v", err)
		}
		expectExecError(t, pool, `INSERT INTO change_events (id,position,event_type,reason,media_id,payload) VALUES ($1,2,'media_deleted','logical_delete',$2,'{}')`, newUUIDv4(t), mediaID)
		expectExecError(t, pool, `INSERT INTO change_events (id,position,event_type,reason,media_id,payload) VALUES ($1,2,'media_upsert','upload',$2,NULL)`, newUUIDv4(t), mediaID)
		expectExecError(t, pool, `DELETE FROM change_events WHERE id=$1`, eventID)

		expectExecError(t, pool, `INSERT INTO backup_runs (id,status,config_snapshot,finished_at) VALUES ($1,'succeeded','{}',now())`, newUUIDv4(t))
		if _, err := pool.Exec(ctx, `INSERT INTO backup_runs (id,status,config_snapshot,finished_at,final_relative_path,size_bytes,sha256,manifest,postgres_version,tool_version) VALUES ($1,'succeeded','{}',now(),'backups/run/dump',1,$2,'{}','17','pg_dump 17')`, newUUIDv4(t), strings.Repeat("2", 64)); err != nil {
			t.Fatalf("insert succeeded backup: %v", err)
		}
		expectExecError(t, pool, `UPDATE maintenance_state SET mode='maintenance' WHERE id=1`)
		if _, err := pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance', reason='restore', owner='admin', entered_at=now() WHERE id=1`); err != nil {
			t.Fatalf("enter maintenance: %v", err)
		}
		expectExecError(t, pool, `INSERT INTO reconciliation_reports (id,scope,findings,repair_disposition,quarantine_paths) VALUES ($1,'all','{}','{}',ARRAY['../escape'])`, newUUIDv4(t))
	})

	t.Run("required indexes are present and usable", func(t *testing.T) {
		pool := migratedIntegrationPool(t, databaseURL)
		ctx := context.Background()
		type indexExpectation struct {
			name      string
			columns   string
			predicate []string
		}
		required := []indexExpectation{
			{name: "media_list_order_idx", columns: "(taken_at DESC NULLS LAST, id DESC)"},
			{name: "media_due_purge_idx", columns: "(purge_after, id)", predicate: []string{"deleted_at IS NOT NULL", "purge_after IS NOT NULL"}},
			{name: "profiles_one_active_key_idx", columns: "(key)", predicate: []string{"status = 'active'::text"}},
			{name: "jobs_dequeue_idx", columns: "(available_at, created_at, id)", predicate: []string{"status = 'queued'::text"}},
			{name: "jobs_list_idx", columns: "(created_at DESC, id DESC)"},
			{name: "jobs_status_list_idx", columns: "(status, created_at DESC, id DESC)"},
			{name: "jobs_media_list_idx", columns: "(media_id_snapshot, created_at DESC, id DESC)"},
			{name: "jobs_one_active_purge_idx", columns: "(media_id_snapshot)", predicate: []string{"type = 'purge'::text", "status = ANY", "'queued'::text", "'running'::text", "'failed'::text"}},
			{name: "job_targets_job_id_idx", columns: "(job_id)"},
			{name: "job_targets_profile_id_idx", columns: "(profile_id)"},
			{name: "renditions_media_id_idx", columns: "(media_id)"},
			{name: "renditions_job_target_id_idx", columns: "(job_target_id)"},
			{name: "renditions_one_current_key_idx", columns: "(media_id, profile_key)", predicate: []string{"is_current"}},
			{name: "renditions_cleanup_idx", columns: "(purge_after, media_id, id)", predicate: []string{"NOT is_current", "purge_after IS NOT NULL"}},
			{name: "backup_runs_succeeded_idx", columns: "(finished_at DESC, id DESC)", predicate: []string{"status = 'succeeded'::text"}},
			{name: "admin_batches_resume_idx", columns: "(status, updated_at, id)"},
		}
		for _, expected := range required {
			var definition, predicate string
			if err := pool.QueryRow(ctx, `
				SELECT pg_get_indexdef(i.indexrelid), COALESCE(pg_get_expr(i.indpred, i.indrelid), '')
				FROM pg_index AS i
				JOIN pg_class AS c ON c.oid=i.indexrelid
				JOIN pg_namespace AS n ON n.oid=c.relnamespace
				WHERE n.nspname=current_schema() AND c.relname=$1`, expected.name).Scan(&definition, &predicate); err != nil {
				t.Errorf("required index %s: %v", expected.name, err)
				continue
			}
			if !strings.Contains(definition, expected.columns) {
				t.Errorf("index %s definition = %q, want columns/order %q", expected.name, definition, expected.columns)
			}
			for _, fragment := range expected.predicate {
				if !strings.Contains(predicate, fragment) {
					t.Errorf("index %s predicate = %q, want fragment %q", expected.name, predicate, fragment)
				}
			}
		}
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire explain connection: %v", err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SET enable_seqscan=off`); err != nil {
			t.Fatalf("disable sequential scans for index capability evidence: %v", err)
		}
		rows, err := conn.Query(ctx, `EXPLAIN (FORMAT JSON) SELECT id FROM jobs WHERE status='queued' AND available_at <= now() ORDER BY available_at,created_at,id LIMIT 1`)
		if err != nil {
			t.Fatalf("explain dequeue query: %v", err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan explain: %v", err)
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("explain rows: %v", err)
		}
		if len(lines) == 0 {
			t.Fatal("EXPLAIN returned no plan")
		}
		if plan := strings.Join(lines, "\n"); !strings.Contains(plan, "jobs_dequeue_idx") {
			t.Fatalf("dequeue plan does not use jobs_dequeue_idx with sequential scans disabled: %s", plan)
		}
	})
}

func migratedIntegrationPool(t *testing.T, databaseURL string) *pgxpool.Pool {
	t.Helper()
	pool := integrationPool(t, databaseURL)
	migrator, err := NewMigrator(pool)
	if err != nil {
		t.Fatalf("NewMigrator(): %v", err)
	}
	if err := migrator.Up(context.Background()); err != nil {
		t.Fatalf("apply initial schema: %v", err)
	}
	return pool
}

func newUUIDv4(t *testing.T) string {
	t.Helper()
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		t.Fatalf("generate UUID: %v", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(value)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexValue[0:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:32])
}

func expectExecError(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err == nil {
		t.Fatalf("statement unexpectedly succeeded: %s", sql)
	}
}

func expectTxCommitError(t *testing.T, pool *pgxpool.Pool, run func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin expected-failure transaction: %v", err)
	}
	if err := run(tx); err != nil {
		_ = tx.Rollback(ctx)
		return
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("transaction commit unexpectedly succeeded")
	}
}

func insertMedia(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO media (id,media_type,taken_at_source) VALUES ($1,'image','unknown')`, id); err != nil {
		t.Fatalf("insert media %s: %v", id, err)
	}
}

func insertOriginal(t *testing.T, pool *pgxpool.Pool, mediaID, digestNibble, suffix string) string {
	t.Helper()
	id := newUUIDv4(t)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO originals (id,media_id,sha256,relative_path,mime_type,size_bytes)
		VALUES ($1,$2,$3,$4,'image/jpeg',1)`, id, mediaID, strings.Repeat(digestNibble, 64), "originals/aa/"+suffix+"/original.jpg"); err != nil {
		t.Fatalf("insert original: %v", err)
	}
	return id
}

func insertDraftProfile(t *testing.T, pool *pgxpool.Pool, key string, version int) string {
	t.Helper()
	id := newUUIDv4(t)
	if _, err := pool.Exec(context.Background(), testJPEGCertificationSQL+` ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("certify test profile capability: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO profiles (id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
		VALUES ($1,$2,$3,'draft',ARRAY['image/jpeg'],'nmcp-media',1,$4::jsonb)`, id, key, version, testProfileParameters(t)); err != nil {
		t.Fatalf("insert draft profile %s/%d: %v", key, version, err)
	}
	return id
}

func testProfileParameters(t *testing.T) []byte {
	t.Helper()
	return testProfileParametersForRecipe(t, "image/jpeg", "image/jpeg")
}

func testProfileParametersForRecipe(t *testing.T, recipeKey, sourceMIMEType string) []byte {
	t.Helper()
	parameters := profile.StandardV1Parameters()
	recipe, ok := parameters.Recipes[sourceMIMEType]
	if !ok {
		t.Fatalf("missing standard recipe fixture for %s", sourceMIMEType)
	}
	parameters.Recipes = map[string]profile.Recipe{recipeKey: recipe}
	return mustJSON(t, parameters)
}

func replaceJSONOnce(t *testing.T, value []byte, old, replacement string) []byte {
	t.Helper()
	if strings.Count(string(value), old) != 1 {
		t.Fatalf("JSON fixture contains %q %d times, want exactly once: %s", old, strings.Count(string(value), old), value)
	}
	return []byte(strings.Replace(string(value), old, replacement, 1))
}

func replaceJSONBytesOnce(t *testing.T, value, old, replacement []byte) []byte {
	t.Helper()
	if bytes.Count(value, old) != 1 {
		t.Fatalf("JSON fixture contains %q %d times, want exactly once", old, bytes.Count(value, old))
	}
	return bytes.Replace(value, old, replacement, 1)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func decodeJSONMap(t *testing.T, value []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(value, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func assertBundledProfileSeeds(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT id::text,key,version,status,input_mime_types,processor,
		       parameters_schema_version,parameters,activated_at,retired_at
		FROM profiles ORDER BY key`)
	if err != nil {
		t.Fatalf("query bundled profiles: %v", err)
	}
	defer rows.Close()
	actual := make(map[string]profile.Definition)
	for rows.Next() {
		var definition profile.Definition
		var status string
		var activatedAt, retiredAt *time.Time
		if err := rows.Scan(
			&definition.ID, &definition.Key, &definition.Version, &status,
			&definition.InputMIMETypes, &definition.Processor,
			&definition.ParametersSchemaVersion, &definition.Parameters,
			&activatedAt, &retiredAt,
		); err != nil {
			t.Fatalf("scan bundled profile: %v", err)
		}
		if status != "draft" || activatedAt != nil || retiredAt != nil {
			t.Fatalf("bundled profile %s lifecycle = %s, %v, %v", definition.Key, status, activatedAt, retiredAt)
		}
		if err := profile.ValidateDraft(definition); err != nil {
			t.Fatalf("database seed %s diverges from Go validator: %v", definition.Key, err)
		}
		actual[definition.Key] = definition
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range profile.BundledDefinitions() {
		got, ok := actual[expected.Key]
		if !ok {
			t.Fatalf("missing bundled profile %s", expected.Key)
		}
		var gotParameters, wantParameters any
		if err := json.Unmarshal(got.Parameters, &gotParameters); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(expected.Parameters, &wantParameters); err != nil {
			t.Fatal(err)
		}
		got.Parameters, expected.Parameters = nil, nil
		if !reflect.DeepEqual(got, expected) || !reflect.DeepEqual(gotParameters, wantParameters) {
			t.Fatalf("bundled profile %s differs from Go definition", expected.Key)
		}
	}
	var capabilityCount, certificationCount int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM profile_processor_capabilities),(SELECT count(*) FROM profile_processor_certifications)`).Scan(&capabilityCount, &certificationCount); err != nil {
		t.Fatal(err)
	}
	if capabilityCount != 17 || certificationCount != 0 {
		t.Fatalf("capability seed counts = %d candidates, %d certifications", capabilityCount, certificationCount)
	}
	expectExecError(t, pool, `UPDATE profiles SET status='active' WHERE key='standard' AND version=1`)
	expectExecError(t, pool, `INSERT INTO profile_processor_capabilities (processor,parameters_schema_version,input_mime_type,source_mode,evidence) VALUES ('nmcp-media',1,'image/x-unknown','still','fabricated')`)
	expectExecError(t, pool, `INSERT INTO profile_processor_capabilities (processor,parameters_schema_version,input_mime_type,source_mode,evidence) VALUES ('nmcp-media',1,'image/jpeg','video','fabricated')`)
	expectExecError(t, pool, `UPDATE profile_processor_capabilities SET evidence='changed' WHERE input_mime_type='image/jpeg'`)
	expectExecError(t, pool, `DELETE FROM profile_processor_capabilities WHERE input_mime_type='image/jpeg'`)
	expectExecError(t, pool, `TRUNCATE profile_processor_capabilities`)
}

const testJPEGCertificationSQL = `
	INSERT INTO profile_processor_certifications (
		id,processor,parameters_schema_version,input_mime_type,source_mode,output_kind,
		max_long_edge,minimum_setting,maximum_setting,evidence
	) VALUES ('70000000-0000-4000-8000-000000000001','nmcp-media',1,'image/jpeg','still','still-avif',4096,1,100,'isolated PostgreSQL test fixture')`

func insertPendingTransform(t *testing.T, pool *pgxpool.Pool, mediaID, originalID, profileID string) string {
	t.Helper()
	ctx := context.Background()
	jobID := newUUIDv4(t)
	targetID := newUUIDv4(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transform fixture: %v", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,max_attempts) VALUES ($1,'transform',$2,$3,'queued',3)`, jobID, originalID, mediaID); err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO job_targets (id,job_id,profile_id,status) VALUES ($1,$2,$3,'pending')`, targetID, jobID, profileID)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("create transform fixture: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit transform fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='running', attempts=1, lease_token=$2, lease_expires_at=now()+interval '1 minute', started_at=now() WHERE id=$1`, jobID, newUUIDv4(t)); err != nil {
		t.Fatalf("claim transform fixture: %v", err)
	}
	return targetID
}

func assertOneConcurrentWinner(t *testing.T, results <-chan error) {
	t.Helper()
	successes := 0
	failures := 0
	for index := 0; index < 2; index++ {
		if err := awaitResult(t, results); err != nil {
			failures++
		} else {
			successes++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent results = %d success, %d failure; want one each", successes, failures)
	}
}

func assertConcurrentFailures(t *testing.T, results <-chan error, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		if err := awaitResult(t, results); err == nil {
			t.Fatalf("concurrent operation %d unexpectedly succeeded", index)
		}
	}
}

func awaitResult(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for concurrent database operation")
		return nil
	}
}
