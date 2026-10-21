package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

type Configuration struct {
	DeletedMediaRetentionDays *int `json:"deleted_media_retention_days"`
	SupersededRenditionRetentionDays *int `json:"superseded_rendition_retention_days"`
	DefaultTimezone string `json:"default_timezone"`
	DBBackupIntervalHours int `json:"db_backup_interval_hours"`
	DBBackupRetentionDays int `json:"db_backup_retention_days"`
}

func ConfigValue(name, value string) (any, error) {
	switch name {
	case "default_timezone":
		if value == "" || value == "Local" || strings.HasPrefix(value, "/") || strings.Contains(value, "..") {
			return nil, ErrValidation
		}
		if _, err := time.LoadLocation(value); err != nil {
			return nil, ErrValidation
		}
		return value, nil
	case "deleted_media_retention_days", "superseded_rendition_retention_days":
		if value == "null" {
			return nil, nil
		}
	case "db_backup_interval_hours", "db_backup_retention_days":
	default:
		return nil, ErrValidation
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 0 || name == "deleted_media_retention_days" && parsed > 36500 {
		return nil, ErrValidation
	}
	if (name == "db_backup_interval_hours" || name == "db_backup_retention_days") && parsed == 0 {
		return nil, ErrValidation
	}
	return int(parsed), nil
}

func (s *Service) ConfigShow(ctx context.Context) (Configuration, error) {
	var result Configuration
	err := s.Pool.QueryRow(ctx, `SELECT deleted_media_retention_days,superseded_rendition_retention_days,
		default_timezone,db_backup_interval_hours,db_backup_retention_days FROM system_config WHERE id=1`).Scan(
		&result.DeletedMediaRetentionDays, &result.SupersededRenditionRetentionDays, &result.DefaultTimezone,
		&result.DBBackupIntervalHours, &result.DBBackupRetentionDays)
	return result, err
}

func (s *Service) ConfigSet(ctx context.Context, name, value string) (Configuration, error) {
	parsed, err := ConfigValue(name, value)
	if err != nil {
		return Configuration{}, err
	}
	err = s.mutate(ctx, "config_set", "config", map[string]any{"name": name, "value": parsed}, nil, func(tx pgx.Tx) error {
		if err := normal(ctx, tx); err != nil {
			return err
		}
		// name has passed the closed allowlist above, never arbitrary SQL.
		_, err := tx.Exec(ctx, `UPDATE system_config SET `+name+`=$1 WHERE id=1`, parsed)
		return err
	})
	if err != nil {
		return Configuration{}, err
	}
	return s.ConfigShow(ctx)
}

// ProfileDocument is the file format, deliberately distinct from internal
// Definition so JSON names are stable and unknown fields cannot be ignored.
type ProfileDocument struct {
	ID string `json:"id,omitempty"`
	Key string `json:"key"`
	Version int `json:"version"`
	InputMIMETypes []string `json:"input_mime_types"`
	Processor string `json:"processor"`
	ParametersSchemaVersion int `json:"parameters_schema_version"`
	Parameters json.RawMessage `json:"parameters"`
}

func DecodeProfile(data []byte) (profile.Definition, error) {
	if len(data) > 2<<20 {
		return profile.Definition{}, ErrValidation
	}
	var doc ProfileDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return profile.Definition{}, ErrValidation
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return profile.Definition{}, ErrValidation
	}
	if doc.ID == "" {
		doc.ID = newID()
	}
	definition := profile.Definition{ID: doc.ID, Key: doc.Key, Version: doc.Version,
		InputMIMETypes: doc.InputMIMETypes, Processor: doc.Processor,
		ParametersSchemaVersion: doc.ParametersSchemaVersion, Parameters: doc.Parameters}
	if err := profile.ValidateDraft(definition); err != nil {
		return profile.Definition{}, fmt.Errorf("%w: profile definition", ErrValidation)
	}
	return definition, nil
}

func (s *Service) ProfileCreate(ctx context.Context, definition profile.Definition) error {
	if err := profile.ValidateDraft(definition); err != nil {
		return ErrValidation
	}
	return s.mutate(ctx, "profile_create", "profile:"+definition.Key,
		map[string]any{"id": definition.ID, "key": definition.Key, "version": definition.Version}, []string{definition.ID}, func(tx pgx.Tx) error {
		if err := normal(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO profiles(id,key,version,status,input_mime_types,processor,parameters_schema_version,parameters)
			VALUES($1,$2,$3,'draft',$4,$5,$6,$7)`, definition.ID, definition.Key, definition.Version,
			definition.InputMIMETypes, definition.Processor, definition.ParametersSchemaVersion, definition.Parameters)
		return err
	})
}

func (s *Service) ProfileTransition(ctx context.Context, id string, activate bool) error {
	if !uuidPattern.MatchString(id) {
		return ErrValidation
	}
	var key string
	if err := s.Pool.QueryRow(ctx, `SELECT key FROM profiles WHERE id=$1`, id).Scan(&key); err != nil {
		if err == pgx.ErrNoRows {
			return ErrConflict
		}
		return err
	}
	command, from, to := "profile_retire", "active", "retired"
	if activate {
		command, from, to = "profile_activate", "draft", "active"
	}
	return s.mutate(ctx, command, "profile:"+key, map[string]any{"id": id}, []string{id}, func(tx pgx.Tx) error {
		if err := normal(ctx, tx); err != nil {
			return err
		}
		// Use the SAME key-scoped lock as the database lifecycle guard, but
		// never wait behind another activation (including non-CLI writers).
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(1313686352,hashtext($1))`, key).Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return ErrConflict
		}
		var status string
		var version int
		if err := tx.QueryRow(ctx, `SELECT status,version FROM profiles WHERE id=$1 FOR UPDATE NOWAIT`, id).Scan(&status, &version); err != nil {
			return err
		}
		if status != from {
			return ErrConflict
		}
		if activate {
			var greatest int
			if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM profiles WHERE key=$1 AND activated_at IS NOT NULL`, key).Scan(&greatest); err != nil {
				return err
			}
			if version <= greatest {
				return ErrConflict
			}
			// This existing DB validator consults the append-only processor
			// certification registry; do not invent or auto-certify capability.
			var valid bool
			if err := tx.QueryRow(ctx, `SELECT nmcp_profile_definition_valid(input_mime_types,processor,
				parameters_schema_version,parameters,true) FROM profiles WHERE id=$1`, id).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return ErrValidation
			}
		}
		_, err := tx.Exec(ctx, `UPDATE profiles SET status=$2 WHERE id=$1`, id, to)
		return err
	})
}
