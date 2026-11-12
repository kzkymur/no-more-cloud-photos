// Package failpoint provides deterministic process-crash points for the
// end-to-end crash/restart lane.
//
// Only binaries built with -tags nmcp_failpoints can fire a point. In every
// other build Hit is an empty, inlinable function: the package then carries no
// point names, reads no environment and references no process exit, and the
// linker discards it entirely. scripts/check-release-no-failpoints gates
// release binaries on that property.
//
// This file must stay free of string literals: point names live only in the
// tagged implementation.
package failpoint

// Point identifies one named crash boundary.
type Point uint8

// Each constant documents its exact boundary; docs/development.md lists the
// dotted names accepted by NMCP_FAILPOINT in tagged builds.
const (
	pointNone Point = iota

	// UploadBeforeOriginalPublish fires after the upload temporary file is
	// complete (written, fsynced, sealed and probed) and the acceptance
	// transaction holds its locks, before the original is renamed into place.
	UploadBeforeOriginalPublish
	// UploadAfterOriginalPublish fires after the original's rename and final
	// directory fsync, before the acceptance transaction commits.
	UploadAfterOriginalPublish
	// UploadAfterDBCommit fires after the 201 acceptance transaction commits,
	// before the HTTP response is written.
	UploadAfterDBCommit

	// RenditionBeforeFilePublish fires after the rendition temporary file is
	// produced and the publication transaction holds its locks, before the
	// file is fsynced, validated and renamed into place.
	RenditionBeforeFilePublish
	// RenditionAfterFilePublish fires after the rendition rename and final
	// directory fsync, before the rendition row transaction commits.
	RenditionAfterFilePublish
	// RenditionAfterDBCommit fires after the rendition row, target and
	// (for the last target) Job transaction commits, before the executor moves
	// on to the next target or returns the Job result to the Worker loop.
	RenditionAfterDBCommit

	// PurgeAfterStart fires after the purge lease and file manifest commit,
	// before any file deletion.
	PurgeAfterStart
	// PurgeAfterFileDelete fires after one manifest file was deleted (unlink
	// and directory fsync) or found missing, before its progress row commits.
	PurgeAfterFileDelete
	// PurgeAfterFileStep fires after one file's progress row commits, before
	// the next file step.
	PurgeAfterFileStep
	// PurgeBeforeFinalize fires after every manifest file is done, before the
	// Media deletion, tombstone and Job success transaction.
	PurgeBeforeFinalize
	// PurgeAfterFinalize fires after the Media deletion, tombstone and Job
	// success transaction commits, before the executor returns.
	PurgeAfterFinalize

	pointLimit
)
