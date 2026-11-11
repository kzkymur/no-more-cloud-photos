package admin

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
)

type options struct {
	command          string
	json             bool
	dry              bool
	yes              bool
	name             string
	value            string
	file             string
	id               string
	profile          string
	mediaID          string
	all              bool
	jobID            string
	status           string
	jobType          string
	additional       int
	batchSize        int
	resume           string
	scope            string
	report           string
	kind             string
	operationID      string
	offlineSource    bool
	offlineAuthority string
	cutover          bool
	ifDue            bool
	definitionData   []byte
}

func parse(args []string) (options, error) {
	var o options
	if len(args) == 0 {
		return o, ErrValidation
	}
	o.command = args[0]
	start := 1
	switch args[0] {
	case "config", "profile", "jobs", "maintenance", "backup":
		if len(args) < 2 {
			return o, ErrValidation
		}
		o.command += " " + args[1]
		start = 2
	}
	flags := flag.NewFlagSet(o.command, flag.ContinueOnError)
	// flag errors may echo arbitrary argv. Emit only our sanitized error.
	flags.SetOutput(io.Discard)
	flags.BoolVar(&o.json, "json", false, "write one JSON value")
	switch o.command {
	case "config show", "maintenance status", "backup list":
	case "backup run":
		flags.BoolVar(&o.ifDue, "if-due", false, "run only when current database configuration is due")
	case "config set":
		flags.StringVar(&o.name, "name", "", "typed setting name")
		flags.StringVar(&o.value, "value", "", "non-secret setting value")
	case "profile create":
		flags.StringVar(&o.file, "file", "", "profile JSON file")
	case "profile activate", "profile retire":
		flags.StringVar(&o.id, "id", "", "profile UUIDv4")
	case "regenerate":
		flags.StringVar(&o.profile, "profile", "", "profile key")
		flags.StringVar(&o.mediaID, "media-id", "", "Media UUIDv4")
		flags.BoolVar(&o.all, "all", false, "all eligible media")
	case "jobs retry":
		flags.StringVar(&o.jobID, "job-id", "", "Job UUIDv4")
		flags.StringVar(&o.status, "status", "", "failed")
		flags.StringVar(&o.jobType, "type", "", "transform or purge")
		flags.IntVar(&o.additional, "additional-attempts", 3, "positive allowance")
	case "check":
		flags.StringVar(&o.scope, "scope", "all", "files, db or all")
	case "repair":
		flags.StringVar(&o.report, "report", "", "saved report UUIDv4")
	case "cleanup":
		flags.StringVar(&o.kind, "kind", "all", "renditions, media, backups or all")
	case "maintenance resume":
	case "restore":
		flags.StringVar(&o.id, "backup-id", "", "backup UUIDv4")
		flags.StringVar(&o.operationID, "operation-id", "", "validated restore operation UUIDv4")
		flags.BoolVar(&o.cutover, "cutover", false, "explicit operator cutover; never resumes maintenance")
		flags.BoolVar(&o.offlineSource, "offline-source", false, "explicit root/operator recovery without source SQL")
		flags.StringVar(&o.offlineAuthority, "offline-authority", "", "root-private external offline authorization file")
	default:
		return o, ErrValidation
	}
	if o.command == "regenerate" || o.command == "jobs retry" {
		flags.IntVar(&o.batchSize, "batch-size", 100, "page size 1..1000")
		flags.StringVar(&o.resume, "resume", "", "persisted batch UUIDv4")
	}
	switch o.command {
	case "regenerate", "jobs retry", "repair", "cleanup":
		flags.BoolVar(&o.dry, "dry-run", false, "preview without effects")
		flags.BoolVar(&o.yes, "yes", false, "confirm mutation")
	case "maintenance resume", "restore":
		flags.BoolVar(&o.yes, "yes", false, "confirm mutation")
	}
	if err := flags.Parse(args[start:]); err != nil || flags.NArg() != 0 {
		return o, ErrValidation
	}
	switch o.command {
	case "config set":
		if _, err := ConfigValue(o.name, o.value); err != nil {
			return o, err
		}
	case "profile create":
		if o.file == "" {
			return o, ErrValidation
		}
		input, err := os.Open(o.file)
		if err != nil {
			return o, ErrValidation
		}
		o.definitionData, err = io.ReadAll(io.LimitReader(input, (2<<20)+1))
		_ = input.Close()
		if err != nil {
			return o, ErrValidation
		}
		if _, err := DecodeProfile(o.definitionData); err != nil {
			return o, err
		}
	case "restore":
		if o.offlineAuthority != "" && !o.offlineSource {
			return o, ErrValidation
		}
		if o.cutover {
			if !uuidPattern.MatchString(o.operationID) || o.id != "" {
				return o, ErrValidation
			}
		} else if !uuidPattern.MatchString(o.id) || o.operationID != "" {
			return o, ErrValidation
		}
	case "profile activate", "profile retire":
		if !uuidPattern.MatchString(o.id) {
			return o, ErrValidation
		}
	case "regenerate", "jobs retry":
		if o.batchSize < 1 || o.batchSize > 1000 || o.resume != "" && !uuidPattern.MatchString(o.resume) || o.status != "" && o.status != "failed" {
			return o, ErrValidation
		}
		if err := o.batchRequest().Validate(); err != nil {
			return o, err
		}
	case "check":
		if o.scope != "all" && o.scope != "db" && o.scope != "files" {
			return o, ErrValidation
		}
	case "repair":
		if !uuidPattern.MatchString(o.report) {
			return o, ErrValidation
		}
	case "cleanup":
		if o.kind != "all" && o.kind != "backups" && o.kind != "media" && o.kind != "renditions" {
			return o, ErrValidation
		}
	}
	if !o.dry && !o.yes {
		switch o.command {
		case "regenerate":
			if o.all {
				return o, ErrConfirmation
			}
		case "jobs retry":
			if o.status == "failed" {
				return o, ErrConfirmation
			}
		case "repair", "cleanup", "maintenance resume", "restore":
			return o, ErrConfirmation
		}
	}
	return o, nil
}

func (o options) batchRequest() BatchRequest {
	operation := "regenerate"
	all := o.all
	if o.command == "jobs retry" {
		operation, all = "jobs_retry", o.status == "failed"
	}
	return BatchRequest{Operation: operation, ProfileKey: o.profile, MediaID: o.mediaID, JobID: o.jobID,
		JobType: o.jobType, All: all, AdditionalAttempts: o.additional}
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parse(args)
	if err != nil {
		fmt.Fprintln(stderr, "usage: nmcp-admin config|profile|regenerate|jobs|check|repair|cleanup|maintenance [options]")
		return finish(stdout, stderr, o.json, nil, err)
	}

	// Offline recovery must not establish a source connection or run source
	// migration status before the explicit operator boundary can fence it.
	if o.command == "restore" && o.offlineSource {
		if os.Geteuid() != 0 {
			return finish(stdout, stderr, o.json, nil, ErrValidation)
		}
		settings, settingsErr := restoreSettings()
		settings.OfflineSource = true
		settings.OfflineAuthorityFile = o.offlineAuthority
		if settings.OfflineAuthorityFile == "" {
			settings.OfflineAuthorityFile = os.Getenv("NMCP_RESTORE_OFFLINE_AUTHORITY")
		}
		var result any
		if settingsErr == nil {
			if o.cutover {
				result, settingsErr = New(nil).RestoreCutover(ctx, o.operationID, settings)
			} else {
				result, settingsErr = New(nil).Restore(ctx, o.id, settings)
			}
		}
		return finish(stdout, stderr, o.json, result, settingsErr)
	}
	cfg, err := config.LoadAdmin()
	if err != nil {
		return finish(stdout, stderr, o.json, nil, err)
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return finish(stdout, stderr, o.json, nil, err)
	}
	defer pool.Close()
	migrator, err := database.NewMigrator(pool)
	if err != nil {
		return finish(stdout, stderr, o.json, nil, err)
	}
	status, err := migrator.Status(ctx)
	if err != nil || !status.Ready() {
		if err == nil {
			err = ErrConflict
		}
		return finish(stdout, stderr, o.json, nil, err)
	}
	service := New(pool)
	var result any
	switch o.command {
	case "restore":
		var settings RestoreSettings
		settings, err = restoreSettings()
		if err == nil {
			if o.cutover {
				result, err = service.RestoreCutover(ctx, o.operationID, settings)
			} else {
				result, err = service.Restore(ctx, o.id, settings)
			}
		}
	case "cleanup":
		if o.kind == "backups" {
			root := os.Getenv("NMCP_BACKUP_ROOT")
			if root == "" {
				err = ErrValidation
			} else {
				result, err = service.CleanupBackups(ctx, BackupSettings{Root: root}, o.dry)
			}
		} else {
			result, err = service.Operational(ctx, o)
		}
	case "backup list":
		result, err = service.BackupList(ctx)
	case "backup run":
		var settings BackupSettings
		settings, err = backupSettings()
		if err == nil {
			result, err = service.Backup(ctx, settings, o.ifDue)
		}
	case "config show":
		result, err = service.ConfigShow(ctx)
	case "config set":
		result, err = service.ConfigSet(ctx, o.name, o.value)
	case "profile create":
		definition, decodeErr := DecodeProfile(o.definitionData)
		err = decodeErr
		if err == nil {
			err = service.ProfileCreate(ctx, definition)
			result = map[string]any{"id": definition.ID, "status": "draft"}
		}
	case "profile activate", "profile retire":
		err = service.ProfileTransition(ctx, o.id, o.command == "profile activate")
		result = map[string]any{"id": o.id, "command": o.command}
	case "regenerate", "jobs retry":
		batch, beginErr := service.BeginBatch(ctx, o.batchRequest(), o.resume, o.dry)
		err = beginErr
		if err == nil && !o.dry {
			// Token survives terminal interruption and does not violate the
			// single stdout JSON contract. Resume does not need active profile.
			fmt.Fprintln(stderr, "resume token:", batch.Token)
			for batch.Status == "running" {
				page, pageErr := service.BatchPage(ctx, batch.Token, o.batchSize)
				err = pageErr
				if page.Token != "" {
					batch = page
				}
				if err != nil {
					break
				}
			}
		}
		if err == nil && batch.Failed > 0 && batch.Status != "running" {
			err = ErrPartial
		}
		if err == nil && batch.Total == 0 {
			err = ErrConflict
		}
		result = batch
	default:
		result, err = service.Operational(ctx, o)
	}
	return finish(stdout, stderr, o.json, result, classified(err))
}

func finish(stdout, stderr io.Writer, jsonOutput bool, result any, err error) int {
	code := 0
	message := "success"
	if err != nil {
		switch {
		case errors.Is(err, ErrValidation):
			code, message = 2, ErrValidation.Error()
		case errors.Is(err, ErrConfirmation):
			code, message = 3, ErrConfirmation.Error()
		case errors.Is(err, ErrConflict):
			code, message = 4, ErrConflict.Error()
		case errors.Is(err, ErrPartial):
			code, message = 5, ErrPartial.Error()
		default:
			code, message = 1, "administrative operation failed"
		}
		_ = json.NewEncoder(stderr).Encode(map[string]any{"level": "ERROR", "message": message, "exit_code": code})
	}
	if jsonOutput {
		if result == nil || err != nil {
			result = map[string]any{"result": result, "exit_code": code, "message": message}
		}
		if encodeErr := json.NewEncoder(stdout).Encode(result); encodeErr != nil {
			return 1
		}
	} else if result != nil {
		encoded, encodeErr := json.MarshalIndent(result, "", "  ")
		if encodeErr != nil {
			return 1
		}
		if _, writeErr := fmt.Fprintln(stdout, string(encoded)); writeErr != nil {
			return 1
		}
	}
	return code
}
