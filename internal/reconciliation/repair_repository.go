package reconciliation

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RepairPostgresRepository struct{ pool *pgxpool.Pool }

func NewRepairPostgresRepository(pool *pgxpool.Pool) (*RepairPostgresRepository, error) {
	if pool == nil {
		return nil, errors.New("repair database is required")
	}
	return &RepairPostgresRepository{pool: pool}, nil
}

func (repository *RepairPostgresRepository) ReadFindings(ctx context.Context, reportID string) ([]RepairFinding, error) {
	result := make([]RepairFinding, 0)
	var after int64
	for {
		rows, err := repository.pool.Query(ctx, `SELECT * FROM nmcp_read_repairable_findings($1,$2,256)`, reportID, after)
		if err != nil {
			return nil, fmt.Errorf("read repairable findings: %w", err)
		}
		count := 0
		for rows.Next() {
			var finding RepairFinding
			var sourceType string
			var sha string
			var observedMTime, observedCTime *time.Time
			if err := rows.Scan(&finding.Ordinal, &finding.FindingID, &sourceType, &finding.SubjectID,
				&finding.MediaID, &finding.JobID, &finding.TargetID, &finding.StorageAttemptID,
				&finding.SourceRelativeKey, &finding.ExpectedSize, &sha, &observedMTime, &observedCTime); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan repairable finding: %w", err)
			}
			decoded, err := hex.DecodeString(sha)
			if err != nil || len(decoded) != len(finding.ExpectedSHA256) {
				rows.Close()
				return nil, ErrRepairAdmission
			}
			copy(finding.ExpectedSHA256[:], decoded)
			finding.SourceType = RepairSourceType(sourceType)
			if observedMTime != nil {
				finding.ObservedMTime = observedMTime.UTC()
			}
			if observedCTime != nil {
				finding.ObservedCTime = observedCTime.UTC()
			}
			result = append(result, finding)
			after = finding.Ordinal
			count++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if count < 256 {
			break
		}
	}
	if len(result) == 0 {
		return nil, ErrRepairAdmission
	}
	return result, nil
}

func (repository *RepairPostgresRepository) ReadRunByAuthorization(ctx context.Context, authorizationID string) (RepairRun, bool, error) {
	row := repository.pool.QueryRow(ctx, `SELECT * FROM nmcp_read_repair_run_by_authorization($1)`, authorizationID)
	run, err := scanRepairRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return RepairRun{}, false, nil
	}
	return run, err == nil, err
}

func (repository *RepairPostgresRepository) ReadRun(ctx context.Context, runID string) (RepairRun, error) {
	return scanRepairRun(repository.pool.QueryRow(ctx, `SELECT * FROM nmcp_read_repair_run_outcome($1)`, runID))
}

type repairRow interface{ Scan(...any) error }

func scanRepairRun(row repairRow) (RepairRun, error) {
	var run RepairRun
	err := row.Scan(&run.RunID, &run.ReportID, &run.AuthorizationID, &run.Actor, &run.StartedAt,
		&run.PlanID, &run.PlanVersion, &run.PlanItemCount, &run.PlanDigest, &run.Result,
		&run.Quarantined, &run.Stale, &run.Resolved, &run.Failed, &run.Unknown, &run.CompletedAt)
	return run, err
}

func (repository *RepairPostgresRepository) BeginRun(ctx context.Context, runID string, request RepairPlanRequest) error {
	var gotRun, report, authorization, actor string
	var created bool
	err := repository.pool.QueryRow(ctx, `SELECT * FROM nmcp_begin_repair_run_v2($1,$2,$3,$4)`,
		runID, request.ReportID, request.AuthorizationID, request.Actor).Scan(&gotRun, &report, &authorization, &actor, &created)
	if err != nil {
		return fmt.Errorf("begin repair run: %w", err)
	}
	if gotRun != runID || report != request.ReportID || authorization != request.AuthorizationID || actor != request.Actor {
		return ErrRepairConflict
	}
	return nil
}

func (repository *RepairPostgresRepository) PrepareItem(ctx context.Context, runID, itemID, findingID, quarantineID string) error {
	var gotItem, destination string
	var ordinal int64
	var created bool
	err := repository.pool.QueryRow(ctx, `SELECT * FROM nmcp_prepare_repair_manifest_item_v2($1,$2,$3,$4)`,
		itemID, runID, findingID, quarantineID).Scan(&gotItem, &ordinal, &destination, &created)
	if err != nil {
		return fmt.Errorf("prepare repair manifest item: %w", err)
	}
	if gotItem != itemID || destination != ".quarantine/"+quarantineID {
		return ErrRepairConflict
	}
	return nil
}

func (repository *RepairPostgresRepository) PlanDigest(ctx context.Context, runID string) (string, error) {
	var digest string
	err := repository.pool.QueryRow(ctx, `SELECT nmcp_read_repair_plan_digest($1)`, runID).Scan(&digest)
	return digest, err
}

func (repository *RepairPostgresRepository) SealPlan(ctx context.Context, planID, runID string, count int64, digest string) error {
	var gotPlan, gotDigest string
	var gotCount int64
	var created bool
	err := repository.pool.QueryRow(ctx, `SELECT * FROM nmcp_seal_repair_plan($1,$2,1::smallint,$3,$4)`,
		planID, runID, count, digest).Scan(&gotPlan, &gotCount, &gotDigest, &created)
	if err != nil {
		return fmt.Errorf("seal repair plan: %w", err)
	}
	if gotPlan != planID || gotCount != count || gotDigest != digest {
		return ErrRepairConflict
	}
	return nil
}

func (repository *RepairPostgresRepository) ReadManifest(ctx context.Context, runID string) ([]RepairManifestItem, error) {
	items := make([]RepairManifestItem, 0)
	var after int64
	for {
		rows, err := repository.pool.Query(ctx, `SELECT * FROM nmcp_read_repair_manifest($1,$2,256)`, runID, after)
		if err != nil {
			return nil, fmt.Errorf("read repair manifest: %w", err)
		}
		count := 0
		for rows.Next() {
			var item RepairManifestItem
			var sourceType, sha string
			if err := rows.Scan(&item.Ordinal, &item.ItemID, &item.FindingID, &item.QuarantineID,
				&sourceType, &item.SubjectID, &item.MediaID, &item.JobID, &item.TargetID, &item.StorageAttemptID,
				&item.SourceRelativeKey, &item.ExpectedSize, &sha, &item.ObservedMTime, &item.ObservedCTime,
				&item.OwnerSnapshot, &item.DestinationRelativeKey,
				&item.EffectiveOutcome, &item.ActiveAttemptID, &item.LatestAttemptNumber); err != nil {
				rows.Close()
				return nil, err
			}
			decoded, err := hex.DecodeString(sha)
			if err != nil || len(decoded) != 32 {
				rows.Close()
				return nil, ErrRepairAdmission
			}
			copy(item.ExpectedSHA256[:], decoded)
			item.SourceType = RepairSourceType(sourceType)
			item.RunID = runID
			items = append(items, item)
			after = item.Ordinal
			count++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if count < 256 {
			break
		}
	}
	return items, nil
}

func (repository *RepairPostgresRepository) Preflight(ctx context.Context, reportID, findingID, previewQuarantineID string) (RepairAuthority, error) {
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RepairAuthority{}, err
	}
	defer tx.Rollback(context.Background())
	authority, err := readRepairAuthority(ctx, tx, `SELECT * FROM nmcp_preflight_repair_finding($1,$2,$3)`, reportID, findingID, previewQuarantineID)
	if err != nil {
		return RepairAuthority{}, err
	}
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return RepairAuthority{}, err
	}
	return authority, nil
}

func (repository *RepairPostgresRepository) BeginAttempt(ctx context.Context, attemptID, itemID, preparedEventID string) (int, error) {
	var number int
	err := repository.pool.QueryRow(ctx, `SELECT nmcp_begin_repair_attempt($1,$2,$3)`, attemptID, itemID, preparedEventID).Scan(&number)
	return number, err
}

type RepairApplySession struct {
	tx        pgx.Tx
	authority RepairAuthority
}

func (repository *RepairPostgresRepository) BeginApply(ctx context.Context, runID, itemID, attemptID string) (*RepairApplySession, error) {
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	authority, err := readRepairAuthority(ctx, tx, `SELECT * FROM nmcp_lock_repair_item_apply($1,$2,$3)`, runID, itemID, attemptID)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	return &RepairApplySession{tx: tx, authority: authority}, nil
}

type repairQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readRepairAuthority(ctx context.Context, queryer repairQueryer, query string, arguments ...any) (RepairAuthority, error) {
	rows, err := queryer.Query(ctx, query, arguments...)
	if err != nil {
		return RepairAuthority{}, err
	}
	defer rows.Close()
	var authority RepairAuthority
	for rows.Next() {
		var row RepairAuthority
		var sourceType string
		var sha string
		var owner RepairOwner
		var reason *string
		if err := rows.Scan(&row.Decision, &reason, &sourceType, &row.Finding.SubjectID, &row.Finding.MediaID,
			&row.Finding.JobID, &row.Finding.TargetID, &row.Finding.StorageAttemptID, &row.Finding.SourceRelativeKey,
			&row.Finding.ExpectedSize, &sha, &row.Destination, &owner.AttemptID, &owner.Kind, &owner.Coverage,
			&owner.EventType, &owner.LeaseExpiresAt, &owner.EventOccurredAt); err != nil {
			return RepairAuthority{}, err
		}
		if reason != nil {
			row.Reason = *reason
		}
		decoded, err := hex.DecodeString(sha)
		if err != nil || len(decoded) != 32 {
			return RepairAuthority{}, ErrRepairAdmission
		}
		copy(row.Finding.ExpectedSHA256[:], decoded)
		row.Finding.SourceType = RepairSourceType(sourceType)
		if authority.Decision == "" {
			authority = row
		}
		if owner.AttemptID != nil {
			authority.Owners = append(authority.Owners, owner)
		}
	}
	return authority, rows.Err()
}

func (session *RepairApplySession) Authority() RepairAuthority { return session.authority }
func (session *RepairApplySession) Append(ctx context.Context, event RepairEvent) (int, error) {
	var sequence int
	err := session.tx.QueryRow(ctx, `SELECT nmcp_append_repair_event($1,$2,$3,$4,$5,$6,$7)`, event.ID, event.AttemptID, event.Type, event.Outcome, event.ErrorCode, event.ObservedSize, event.ObservedSHA).Scan(&sequence)
	return sequence, err
}
func (session *RepairApplySession) Commit(ctx context.Context) error { return session.tx.Commit(ctx) }
func (session *RepairApplySession) Rollback(ctx context.Context) error {
	return session.tx.Rollback(ctx)
}

func (repository *RepairPostgresRepository) ReadHistory(ctx context.Context, itemID string) ([]RepairHistoryEvent, error) {
	rows, err := repository.pool.Query(ctx, `SELECT * FROM nmcp_read_repair_attempt_history($1,0,0,1000)`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RepairHistoryEvent
	for rows.Next() {
		var event RepairHistoryEvent
		var txid any
		if err := rows.Scan(&event.AttemptNumber, &event.AttemptID, &event.StartedAt, &event.Sequence, &event.EventID, &event.EventType, &event.OutcomeCode, &event.ErrorCode, &event.ObservedSize, &event.ObservedSHA, &txid, &event.OccurredAt, &event.PhysicalEffectPossible); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func (repository *RepairPostgresRepository) ConfirmHistory(ctx context.Context, runID, itemID string) ([]RepairHistoryEvent, error) {
	conn, err := repository.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))`, runID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT * FROM nmcp_read_repair_attempt_history($1,0,0,1000)`, itemID)
	if err != nil {
		return nil, err
	}
	var result []RepairHistoryEvent
	for rows.Next() {
		var event RepairHistoryEvent
		var txid any
		if err := rows.Scan(&event.AttemptNumber, &event.AttemptID, &event.StartedAt, &event.Sequence, &event.EventID, &event.EventType, &event.OutcomeCode, &event.ErrorCode, &event.ObservedSize, &event.ObservedSHA, &txid, &event.OccurredAt, &event.PhysicalEffectPossible); err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func (repository *RepairPostgresRepository) Finish(ctx context.Context, runID string) (RepairExecutionResult, error) {
	_, finishErr := repository.pool.Exec(ctx, `SELECT nmcp_finish_repair_run($1)`, runID)
	var run RepairRun
	var err error
	if finishErr == nil {
		run, err = repository.ReadRun(ctx, runID)
	}
	if finishErr != nil || err != nil {
		operationErr := errors.Join(finishErr, err)
		confirmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		run, err = repository.confirmRunOutcome(confirmCtx, runID)
		if err != nil {
			return RepairExecutionResult{}, errors.Join(operationErr, err)
		}
		if run.Result == nil {
			return RepairExecutionResult{}, errors.Join(operationErr, ErrRepairIncomplete)
		}
	}
	if run.Result == nil {
		return RepairExecutionResult{}, ErrRepairIncomplete
	}
	return RepairExecutionResult{RunID: runID, Result: *run.Result, Manifest: derefInt64(run.PlanItemCount), Quarantined: derefInt64(run.Quarantined), Stale: derefInt64(run.Stale), Resolved: derefInt64(run.Resolved), Failed: derefInt64(run.Failed), Unknown: derefInt64(run.Unknown)}, nil
}

func (repository *RepairPostgresRepository) confirmRunOutcome(ctx context.Context, runID string) (RepairRun, error) {
	connection, err := repository.pool.Acquire(ctx)
	if err != nil {
		return RepairRun{}, err
	}
	defer connection.Release()
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RepairRun{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))`, runID); err != nil {
		return RepairRun{}, err
	}
	run, err := scanRepairRun(tx.QueryRow(ctx, `SELECT * FROM nmcp_read_repair_run_outcome($1)`, runID))
	if err != nil {
		return RepairRun{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RepairRun{}, err
	}
	return run, nil
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
