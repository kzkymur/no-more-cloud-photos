package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type database interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type PostgresRepository struct{ db database }

func NewPostgresRepository(db database) (*PostgresRepository, error) {
	if db == nil {
		return nil, errors.New("reconciliation database is required")
	}
	return &PostgresRepository{db: db}, nil
}

func (repository *PostgresRepository) DatabaseNow(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := repository.db.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read reconciliation database clock: %w", err)
	}
	return now.UTC(), nil
}

func (repository *PostgresRepository) CaptureSnapshot(ctx context.Context) (SnapshotCapture, error) {
	tx, err := repository.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return SnapshotCapture{}, fmt.Errorf("begin reconciliation snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var capture SnapshotCapture
	snapshot := &capture.Snapshot
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp(),clock_timestamp()`).Scan(&snapshot.StartedAt, &snapshot.CutoffAt); err != nil {
		return capture, fmt.Errorf("capture reconciliation cutoff: %w", err)
	}
	capture.DatabaseReferencesError = readSnapshotSource(ctx, tx, func(source pgx.Tx) error {
		rows, queryErr := source.Query(ctx, `SELECT * FROM nmcp_read_check_database_references($1)`, snapshot.CutoffAt)
		if queryErr != nil {
			return fmt.Errorf("read reconciliation database references: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			var row Reference
			if scanErr := rows.Scan(&row.Kind, &row.SubjectID, &row.MediaID, &row.OriginalID, &row.JobID, &row.TargetID, &row.RelativeKey, &row.ExpectedState, &row.SizeBytes, &row.SHA256, &row.IsCurrent, &row.ProvenanceValid, &row.CurrentValid, &row.DueAt); scanErr != nil {
				return fmt.Errorf("scan reconciliation database reference: %w", scanErr)
			}
			snapshot.References = append(snapshot.References, row)
		}
		return rows.Err()
	})
	capture.AttemptOwnersError = readSnapshotSource(ctx, tx, func(source pgx.Tx) error {
		rows, queryErr := source.Query(ctx, `SELECT * FROM nmcp_read_check_attempt_owners()`)
		if queryErr != nil {
			return fmt.Errorf("read reconciliation attempt owners: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			var owner AttemptOwner
			if scanErr := rows.Scan(&owner.AttemptID, &owner.Kind, &owner.Coverage, &owner.OriginalID, &owner.JobID, &owner.TempRelativeKey, &owner.EventType, &owner.LeaseExpiresAt, &owner.EventOccurredAt); scanErr != nil {
				return fmt.Errorf("scan reconciliation attempt owner: %w", scanErr)
			}
			snapshot.Attempts = append(snapshot.Attempts, owner)
		}
		return rows.Err()
	})
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&snapshot.EndedAt); err != nil {
		return capture, fmt.Errorf("capture reconciliation snapshot end: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return capture, fmt.Errorf("commit reconciliation snapshot: %w", err)
	}
	snapshot.StartedAt = snapshot.StartedAt.UTC()
	snapshot.CutoffAt = snapshot.CutoffAt.UTC()
	snapshot.EndedAt = snapshot.EndedAt.UTC()
	return capture, nil
}

func readSnapshotSource(ctx context.Context, tx pgx.Tx, read func(pgx.Tx) error) error {
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin reconciliation source savepoint: %w", err)
	}
	if err := read(savepoint); err != nil {
		if rollbackErr := savepoint.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		return err
	}
	if err := savepoint.Commit(ctx); err != nil {
		return fmt.Errorf("release reconciliation source savepoint: %w", err)
	}
	return nil
}

func (repository *PostgresRepository) BeginReport(ctx context.Context, reportID string, scope Scope, snapshot Snapshot, scanStarted time.Time) error {
	_, err := repository.db.Exec(ctx, `SELECT nmcp_begin_check_report($1,$2,1::smallint,$3,$4,$5,$6)`,
		reportID, string(scope), snapshot.StartedAt, snapshot.CutoffAt, snapshot.EndedAt, scanStarted)
	if err != nil {
		return fmt.Errorf("begin reconciliation report: %w", err)
	}
	return nil
}

func (repository *PostgresRepository) CompleteSource(ctx context.Context, reportID, source string, errorCode *string) error {
	_, err := repository.db.Exec(ctx, `SELECT nmcp_complete_check_source($1,$2,$3)`, reportID, source, errorCode)
	if err != nil {
		return fmt.Errorf("complete reconciliation source %s: %w", source, err)
	}
	return nil
}

func (repository *PostgresRepository) ReportOutcome(ctx context.Context, reportID string) (ReportOutcome, error) {
	var outcome ReportOutcome
	err := repository.db.QueryRow(ctx, `SELECT * FROM nmcp_read_check_report_outcome($1)`, reportID).Scan(
		&outcome.DatabaseResult, &outcome.DatabaseError,
		&outcome.AttemptResult, &outcome.AttemptError,
		&outcome.StorageResult, &outcome.StorageError,
		&outcome.FindingCount, &outcome.Sealed, &outcome.SealedFindingCount, &outcome.ScanEndedAt,
	)
	if err != nil {
		return ReportOutcome{}, fmt.Errorf("read reconciliation report outcome: %w", err)
	}
	return outcome, nil
}

func (repository *PostgresRepository) AppendFinding(ctx context.Context, reportID string, finding Finding) (int64, error) {
	var ordinal int64
	err := repository.db.QueryRow(ctx, `SELECT nmcp_append_check_finding(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULL,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		finding.ID, reportID, finding.Kind, finding.Reason, finding.SubjectType,
		finding.SubjectID, finding.MediaID, finding.JobID, finding.TargetID, finding.AttemptID,
		finding.RelativeKey, finding.ExpectedState, finding.ExpectedSize, finding.ExpectedSHA,
		finding.ObservedType, finding.ObservedSize, finding.ObservedSHA,
		finding.ObservedMTime, finding.ObservedCTime, finding.ObservedAt).Scan(&ordinal)
	if err != nil {
		return 0, fmt.Errorf("append reconciliation finding: %w", err)
	}
	return ordinal, nil
}

func (repository *PostgresRepository) SealReport(ctx context.Context, reportID string, count int64, scanEnded time.Time) error {
	_, err := repository.db.Exec(ctx, `SELECT nmcp_seal_check_report($1,$2,$3)`, reportID, count, scanEnded)
	if err != nil {
		return fmt.Errorf("seal reconciliation report: %w", err)
	}
	return nil
}

func (repository *PostgresRepository) FinalizeReport(ctx context.Context, reportID string, count int64, scanEnded time.Time) error {
	tx, err := repository.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin reconciliation report finalization: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT nmcp_complete_check_source($1,'storage_scan',NULL)`, reportID); err != nil {
		return fmt.Errorf("complete reconciliation storage source: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT nmcp_seal_check_report($1,$2,$3)`, reportID, count, scanEnded); err != nil {
		return fmt.Errorf("seal reconciliation report: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit reconciliation report finalization: %w", err)
	}
	return nil
}
