package admin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

// These PostgreSQL integration tests exercise the real transaction, audit,
// lock, trigger, and validator path. Codec work goes through the runtime seam;
// the production path (newProductionCertifyRuntime) drives the real helpers.

func certifyIntegrationDraft(t *testing.T, s *Service, key string, mimes []string) profile.Definition {
	t.Helper()
	definition := certifyDraft(t, profile.ThumbnailV1(), mimes, nil)
	definition.Key, definition.Version = key, 1
	if err := s.ProfileCreate(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	return definition
}

func certifyIntegrationSpec(t *testing.T, profileID string, fixtures map[string][]string, helperVariant ...string) CertifySpec {
	t.Helper()
	env := newCertifyTestEnv(t, fixtures, helperVariant...)
	env.spec.ProfileID = profileID
	spec, err := DecodeCertifySpec(env.encode(t))
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func certifyCount(t *testing.T, s *Service, query string, args ...any) int {
	t.Helper()
	var count int
	if err := s.Pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func certifyRowCount(t *testing.T, s *Service) int {
	return certifyCount(t, s, `SELECT count(*)::int FROM profile_processor_certifications`)
}

func certifyAuditCount(t *testing.T, s *Service, outcome, profileID string) int {
	return certifyCount(t, s, `SELECT count(*)::int FROM admin_audit WHERE command='profile_certify' AND outcome=$1 AND $2::uuid=ANY(affected_ids)`, outcome, profileID)
}

func TestAdminIntegrationProfileCertifyThenActivate(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	s.newCertifyRuntime = func(*certifyWorkspace) (certifyRuntime, error) { return &fakeCertifyRuntime{}, nil }
	definition := certifyIntegrationDraft(t, s, "certify-success", []string{"image/jpeg", "image/png"})
	if err := s.ProfileTransition(ctx, definition.ID, true); !errors.Is(err, ErrValidation) {
		t.Fatalf("uncertified activation not refused: %v", err)
	}
	result, err := s.ProfileCertify(ctx, certifyIntegrationSpec(t, definition.ID, map[string][]string{"image/jpeg": {"a"}, "image/png": {"b"}}))
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	if !result.ActivationVerified || result.ProfileStatus != "draft" || len(result.Certifications) != 2 || certifyRowCount(t, s) != 2 {
		t.Fatalf("result %+v", result)
	}
	if certifyAuditCount(t, s, "succeeded", definition.ID) != 1 {
		t.Fatal("success audit absent")
	}
	var status string
	if err := s.Pool.QueryRow(ctx, `SELECT status FROM profiles WHERE id=$1`, definition.ID).Scan(&status); err != nil || status != "draft" {
		t.Fatal("certify must not activate")
	}
	rows, err := s.Pool.Query(ctx, `SELECT input_mime_type,source_mode,output_kind,max_long_edge,minimum_setting,maximum_setting,evidence
		FROM profile_processor_certifications ORDER BY input_mime_type`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var mime, mode, kind, evidence string
		var edge, minimum, maximum int
		if err := rows.Scan(&mime, &mode, &kind, &edge, &minimum, &maximum, &evidence); err != nil {
			t.Fatal(err)
		}
		var receipt certifyReceipt
		if mode != "still" || kind != "still-avif" || edge != 640 || minimum != 50 || maximum != 50 ||
			json.Unmarshal([]byte(evidence), &receipt) != nil || receipt.Requirement.MIMEType != mime || receipt.Profile.ID != definition.ID {
			t.Fatalf("row %s %s %s %d %d %d", mime, mode, kind, edge, minimum, maximum)
		}
	}
	rows.Close()
	if _, err := s.Pool.Exec(ctx, `UPDATE profile_processor_certifications SET evidence='x'`); err == nil {
		t.Fatal("certification row is mutable")
	}
	if err := s.ProfileTransition(ctx, definition.ID, true); err != nil {
		t.Fatalf("activation after certification: %v", err)
	}

	// Re-certifying the now active profile is refused and inserts nothing.
	again := certifyIntegrationSpec(t, definition.ID, map[string][]string{"image/jpeg": {"a"}, "image/png": {"b"}})
	if _, err := s.ProfileCertify(ctx, again); !errors.Is(err, ErrConflict) || certifyRowCount(t, s) != 2 {
		t.Fatalf("non-draft certify: %v", err)
	}
	if certifyAuditCount(t, s, "failed", definition.ID) != 1 {
		t.Fatal("refused certification not audited")
	}
	if _, err := s.ProfileCertify(ctx, certifyIntegrationSpec(t, newID(), map[string][]string{"image/jpeg": {"a"}})); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown profile: %v", err)
	}
}

func certifyInsertRow(t *testing.T, s *Service, mime, evidence string) string {
	t.Helper()
	id := newID()
	if _, err := s.Pool.Exec(context.Background(), `INSERT INTO profile_processor_certifications(id,processor,parameters_schema_version,input_mime_type,
		source_mode,output_kind,max_long_edge,minimum_setting,maximum_setting,evidence) VALUES($1,'nmcp-media',1,$2,'still','still-avif',640,50,50,$3)`, id, mime, evidence); err != nil {
		t.Error(err)
	}
	return id
}

func certifyMIMECount(t *testing.T, s *Service, mime string) int {
	return certifyCount(t, s, `SELECT count(*)::int FROM profile_processor_certifications WHERE input_mime_type=$1`, mime)
}

func TestAdminIntegrationProfileCertifyAppendOnlyHelperIdentity(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	s.newCertifyRuntime = func(*certifyWorkspace) (certifyRuntime, error) { return &fakeCertifyRuntime{}, nil }
	first := certifyIntegrationDraft(t, s, "certify-first", []string{"image/jpeg"})
	if _, err := s.ProfileCertify(ctx, certifyIntegrationSpec(t, first.ID, map[string][]string{"image/jpeg": {"a"}})); err != nil {
		t.Fatal(err)
	}
	var originalID, originalEvidence string
	if err := s.Pool.QueryRow(ctx, `SELECT id::text,evidence FROM profile_processor_certifications`).Scan(&originalID, &originalEvidence); err != nil {
		t.Fatal(err)
	}

	// Identical helper/ICC digests for an existing requirement: exit 4, nothing
	// inserted, even though the png requirement is new.
	second := certifyIntegrationDraft(t, s, "certify-second", []string{"image/jpeg", "image/png"})
	if _, err := s.ProfileCertify(ctx, certifyIntegrationSpec(t, second.ID, map[string][]string{"image/jpeg": {"a"}, "image/png": {"b"}})); !errors.Is(err, ErrConflict) || certifyRowCount(t, s) != 1 {
		t.Fatalf("identical-helper duplicate accepted: %v", err)
	}
	if certifyAuditCount(t, s, "failed", second.ID) != 1 {
		t.Fatal("duplicate refusal not audited")
	}

	// A changed helper digest appends new evidence; the old row is untouched.
	result, err := s.ProfileCertify(ctx, certifyIntegrationSpec(t, second.ID, map[string][]string{"image/jpeg": {"a"}, "image/png": {"b"}}, "-upgraded"))
	if err != nil || len(result.Certifications) != 2 || certifyRowCount(t, s) != 3 || certifyMIMECount(t, s, "image/jpeg") != 2 {
		t.Fatalf("changed helper not appended: %v", err)
	}
	var stillEvidence string
	if err := s.Pool.QueryRow(ctx, `SELECT evidence FROM profile_processor_certifications WHERE id=$1`, originalID).Scan(&stillEvidence); err != nil || stillEvidence != originalEvidence {
		t.Fatal("original certification row changed or removed")
	}
	if err := s.ProfileTransition(ctx, second.ID, true); err != nil {
		t.Fatalf("activation after appended certification: %v", err)
	}
	// The upgraded digests are now themselves identical for these requirements.
	third := certifyIntegrationDraft(t, s, "certify-third", []string{"image/png"})
	if _, err := s.ProfileCertify(ctx, certifyIntegrationSpec(t, third.ID, map[string][]string{"image/png": {"b"}}, "-upgraded")); !errors.Is(err, ErrConflict) || certifyRowCount(t, s) != 3 {
		t.Fatalf("identical upgraded duplicate accepted: %v", err)
	}

	// Foreign or malformed evidence on an existing row is "not identical": the
	// new row is appended and the existing rows are neither errors nor mutated.
	foreign := certifyInsertRow(t, s, "image/heic", "operator note: certified manually")
	malformed := certifyInsertRow(t, s, "image/heic", originalEvidence[:len(originalEvidence)-1])
	heic := certifyIntegrationDraft(t, s, "certify-heic", []string{"image/heic"})
	if _, err := s.ProfileCertify(ctx, certifyIntegrationSpec(t, heic.ID, map[string][]string{"image/heic": {"h"}})); err != nil || certifyMIMECount(t, s, "image/heic") != 3 {
		t.Fatalf("foreign evidence blocked append: %v", err)
	}
	if certifyCount(t, s, `SELECT count(*)::int FROM profile_processor_certifications WHERE id IN ($1,$2)`, foreign, malformed) != 2 {
		t.Fatal("foreign rows changed")
	}

	// Race: a row with identical digests appears after the read-only precheck,
	// while transforms run. The locked transaction refuses and inserts nothing.
	var receipt certifyReceipt
	if err := json.Unmarshal([]byte(originalEvidence), &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Requirement.MIMEType = "image/bmp"
	racing, _ := json.Marshal(receipt)
	race := certifyIntegrationDraft(t, s, "certify-race", []string{"image/dng", "image/bmp"})
	s.newCertifyRuntime = func(*certifyWorkspace) (certifyRuntime, error) {
		inserted := false
		return &fakeCertifyRuntime{afterTransform: func(certifyRun) {
			if !inserted {
				inserted = true
				certifyInsertRow(t, s, "image/bmp", string(racing))
			}
		}}, nil
	}
	before := certifyRowCount(t, s)
	if _, err := s.ProfileCertify(ctx, certifyIntegrationSpec(t, race.ID, map[string][]string{"image/dng": {"d"}, "image/bmp": {"b"}})); !errors.Is(err, ErrConflict) {
		t.Fatalf("racing identical certification accepted: %v", err)
	}
	if certifyRowCount(t, s) != before+1 || certifyMIMECount(t, s, "image/dng") != 0 || certifyMIMECount(t, s, "image/bmp") != 1 {
		t.Fatal("partial rows inserted")
	}
	if certifyAuditCount(t, s, "failed", race.ID) != 1 {
		t.Fatal("transaction refusal not audited")
	}
}

func TestAdminIntegrationProfileCertifyPartialFailureInsertsNothing(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	definition := certifyIntegrationDraft(t, s, "certify-partial", []string{"image/jpeg", "image/png"})
	s.newCertifyRuntime = func(*certifyWorkspace) (certifyRuntime, error) {
		return &fakeCertifyRuntime{failTransform: func(run certifyRun) error {
			if run.Fixture.mimeType == "image/png" {
				return errors.New("helper process failure")
			}
			return nil
		}}, nil
	}
	spec := certifyIntegrationSpec(t, definition.ID, map[string][]string{"image/jpeg": {"a"}, "image/png": {"b"}})
	if _, err := s.ProfileCertify(ctx, spec); err == nil || errors.Is(err, ErrValidation) || errors.Is(err, ErrConflict) {
		t.Fatalf("operational failure classification: %v", err)
	}
	if certifyRowCount(t, s) != 0 || certifyAuditCount(t, s, "failed", definition.ID) != 1 {
		t.Fatal("partial failure inserted rows or skipped audit")
	}
	// A missing required MIME fixture is validation and inserts nothing.
	s.newCertifyRuntime = func(*certifyWorkspace) (certifyRuntime, error) { return &fakeCertifyRuntime{}, nil }
	if _, err := s.ProfileCertify(ctx, certifyIntegrationSpec(t, definition.ID, map[string][]string{"image/jpeg": {"a"}})); !errors.Is(err, ErrValidation) || certifyRowCount(t, s) != 0 {
		t.Fatalf("missing MIME fixture: %v", err)
	}
	if err := s.ProfileTransition(ctx, definition.ID, true); !errors.Is(err, ErrValidation) {
		t.Fatal("activation succeeded without certification")
	}
	// Maintenance mode refuses the mutation and inserts nothing.
	if _, err := s.Pool.Exec(ctx, `UPDATE maintenance_state SET mode='maintenance',reason='test',owner='test',entered_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProfileCertify(ctx, spec); !errors.Is(err, ErrConflict) || certifyRowCount(t, s) != 0 {
		t.Fatalf("maintenance certify: %v", err)
	}
}

// certifyValidatorWith evaluates nmcp_profile_definition_valid(...,true) for a
// definition with exactly the given certification rows present, inside a
// transaction that is always rolled back (rows are otherwise immutable).
func certifyValidatorWith(t *testing.T, s *Service, definition profile.Definition, rows []certifyRequirement) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	var existing int
	if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM profile_processor_certifications`).Scan(&existing); err != nil || existing != 0 {
		t.Fatalf("parity schema is not empty: %d %v", existing, err)
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `INSERT INTO profile_processor_certifications(id,processor,parameters_schema_version,input_mime_type,
			source_mode,output_kind,max_long_edge,minimum_setting,maximum_setting,evidence) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'parity')`,
			newID(), definition.Processor, definition.ParametersSchemaVersion, r.MIMEType, string(r.SourceMode), string(r.OutputKind),
			r.MaxEdge, r.MinSetting, r.MaxSetting); err != nil {
			t.Fatalf("insert %+v: %v", r, err)
		}
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT nmcp_profile_definition_valid($1::text[],$2,$3,$4::jsonb,true)`,
		definition.InputMIMETypes, definition.Processor, definition.ParametersSchemaVersion, string(definition.Parameters)).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	return valid
}

func certifyHasRequirement(requirements []certifyRequirement, mime string, kind profile.OutputKind) bool {
	for _, r := range requirements {
		if r.MIMEType == mime && r.OutputKind == kind {
			return true
		}
	}
	return false
}

// TestAdminIntegrationProfileCertifyRequirementParity proves, on the current
// embedded migrations, that the Go-derived certification set is necessary and
// sufficient for the SQL activation validator.
func TestAdminIntegrationProfileCertifyRequirementParity(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	var version int
	if err := s.Pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version < 24 {
		t.Fatalf("parity requires migration 24 or later: %d %v", version, err)
	}
	cases := map[string]profile.Definition{}
	// The migration-seeded bundled drafts, read back exactly as certify loads them.
	for _, id := range []string{profile.StandardV1ID, profile.ThumbnailV1ID} {
		loaded, err := s.loadCertifyProfile(ctx, s.Pool, id, "")
		if err != nil || loaded.status != "draft" {
			t.Fatalf("bundled draft %s: %v", id, err)
		}
		cases["bundled "+loaded.definition.Key] = loaded.definition
	}
	// Synthetic recipes: every valid source_mode x frame_policy, with
	// non-bundled edges/settings so the derived values (not constants) are used.
	adjusted := func(base profile.Definition, mime string, edge, setting int) profile.Definition {
		return certifyDraft(t, base, []string{mime}, func(r map[string]profile.Recipe) {
			recipe := r[mime]
			recipe.MaxLongEdge = edge
			if recipe.StillOutput != nil {
				output := *recipe.StillOutput
				output.Quality = setting
				recipe.StillOutput = &output
			}
			if recipe.AnimationOutput != nil {
				output := *recipe.AnimationOutput
				output.Quality = setting + 1
				recipe.AnimationOutput = &output
			}
			if recipe.VideoOutput != nil {
				output := *recipe.VideoOutput
				output.CRF = setting % 64
				recipe.VideoOutput = &output
			}
			r[mime] = recipe
		})
	}
	for _, c := range []struct {
		name string
		base profile.Definition
		mime string
	}{
		{"still/first", profile.ThumbnailV1(), "image/heic"},
		{"probe-animation/first gif", profile.ThumbnailV1(), "image/gif"},
		{"probe-animation/first webp", profile.ThumbnailV1(), "image/webp"},
		{"probe-animation/all gif", profile.StandardV1(), "image/gif"},
		{"probe-animation/all webp", profile.StandardV1(), "image/webp"},
		{"video/first", profile.ThumbnailV1(), "video/quicktime"},
		{"video/all", profile.StandardV1(), "video/mp4"},
	} {
		cases["synthetic "+c.name] = adjusted(c.base, c.mime, 1234, 37)
	}
	cases["synthetic mixed"] = certifyDraft(t, profile.StandardV1(), []string{"image/png", "image/gif", "image/webp", "video/mp4"}, nil)

	for name, definition := range cases {
		t.Run(name, func(t *testing.T) {
			// Validator parity is independent of the processors' static envelope,
			// so synthetic edges/settings beyond it are still compared.
			requirements, _, err := certifyWorkerRequirements(definition)
			if err != nil {
				t.Fatalf("derivation refused: %v", err)
			}
			if len(requirements) == 0 {
				t.Fatal("no requirements derived")
			}
			if certifyValidatorWith(t, s, definition, nil) {
				t.Fatal("validator true without certifications")
			}
			if !certifyValidatorWith(t, s, definition, requirements) {
				t.Fatal("Go-derived certification set is not sufficient")
			}
			for omit := range requirements {
				partial := append(append([]certifyRequirement{}, requirements[:omit]...), requirements[omit+1:]...)
				if certifyValidatorWith(t, s, definition, partial) {
					t.Fatalf("requirement %+v is not necessary", requirements[omit])
				}
			}
		})
	}

	standard, _, _ := certificationRequirements(cases["bundled standard"])
	thumbnail, _, _ := certificationRequirements(cases["bundled thumbnail"])
	if certifyHasRequirement(standard, "image/gif", profile.OutputStillAVIF) || !certifyHasRequirement(standard, "image/gif", profile.OutputAnimationWebP) {
		t.Fatal("standard GIF must need only animation-webp")
	}
	if !certifyHasRequirement(thumbnail, "image/gif", profile.OutputStillAVIF) {
		t.Fatal("thumbnail GIF must still need still-avif")
	}
	if !certifyHasRequirement(standard, "image/webp", profile.OutputStillAVIF) || !certifyHasRequirement(standard, "image/webp", profile.OutputAnimationWebP) {
		t.Fatal("standard WebP must need still-avif and animation-webp")
	}
	if len(standard) != 18 || len(thumbnail) != 17 {
		t.Fatalf("bundled requirement counts standard=%d thumbnail=%d", len(standard), len(thumbnail))
	}
}

func TestAdminIntegrationMissingAnimationCertificationBlocksActivation(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	definition := certifyDraft(t, profile.StandardV1(), []string{"image/webp"}, nil)
	definition.Key, definition.Version = "parity-webp", 1
	if err := s.ProfileCreate(ctx, definition); err != nil {
		t.Fatal(err)
	}
	requirements, _, err := certificationRequirements(definition)
	if err != nil || len(requirements) != 2 {
		t.Fatalf("webp standard requirements: %v %d", err, len(requirements))
	}
	insert := func(kind profile.OutputKind) {
		for _, r := range requirements {
			if r.OutputKind == kind {
				if _, err := s.Pool.Exec(ctx, `INSERT INTO profile_processor_certifications(id,processor,parameters_schema_version,input_mime_type,
					source_mode,output_kind,max_long_edge,minimum_setting,maximum_setting,evidence) VALUES($1,'nmcp-media',1,$2,$3,$4,$5,$6,$7,'parity')`,
					newID(), r.MIMEType, string(r.SourceMode), string(r.OutputKind), r.MaxEdge, r.MinSetting, r.MaxSetting); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	insert(profile.OutputStillAVIF)
	if err := s.ProfileTransition(ctx, definition.ID, true); !errors.Is(err, ErrValidation) {
		t.Fatalf("activation without animation-webp certification: %v", err)
	}
	insert(profile.OutputAnimationWebP)
	if err := s.ProfileTransition(ctx, definition.ID, true); err != nil {
		t.Fatalf("activation with both certifications: %v", err)
	}
}
