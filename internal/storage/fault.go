package storage

import (
	"context"
	"errors"
)

var ErrInvalidDatabaseBoundary = errors.New("invalid storage database boundary")

type Boundary string

const (
	BoundaryDirectoryCreate                    Boundary = "directory_create"
	BoundaryNewDirectorySync                   Boundary = "new_directory_sync"
	BoundaryParentDirectorySync                Boundary = "parent_directory_sync"
	BoundaryTempCreate                         Boundary = "temp_create"
	BoundaryWrite                              Boundary = "write"
	BoundaryFileSync                           Boundary = "file_sync"
	BoundaryValidation                         Boundary = "validation"
	BoundaryReadOnlyProbe                      Boundary = "read_only_probe"
	BoundaryRename                             Boundary = "rename"
	BoundaryFinalDirectorySync                 Boundary = "final_directory_sync"
	BoundaryBeforeDBCommit                     Boundary = "before_db_commit"
	BoundaryAfterDBCommit                      Boundary = "after_db_commit"
	BoundaryDelete                             Boundary = "delete"
	BoundaryDeleteDirectorySync                Boundary = "delete_directory_sync"
	BoundaryUploadDirectoryDelete              Boundary = "upload_directory_delete"
	BoundaryUploadDirectorySync                Boundary = "upload_directory_sync"
	BoundaryQuarantineDirectoryCreate          Boundary = "quarantine_directory_create"
	BoundaryQuarantineDirectorySync            Boundary = "quarantine_directory_sync"
	BoundaryQuarantineRename                   Boundary = "quarantine_rename"
	BoundaryQuarantineSourceDirectorySync      Boundary = "quarantine_source_directory_sync"
	BoundaryQuarantineDestinationDirectorySync Boundary = "quarantine_destination_directory_sync"
)

type Phase string

const (
	Before Phase = "before"
	After  Phase = "after"
)

type FaultEvent struct {
	Boundary Boundary
	Phase    Phase
	Key      string
	Depth    int
}

type FaultInjector interface {
	Inject(context.Context, FaultEvent) error
}

type FaultInjectorFunc func(context.Context, FaultEvent) error

func (function FaultInjectorFunc) Inject(ctx context.Context, event FaultEvent) error {
	return function(ctx, event)
}

func Inject(ctx context.Context, injector FaultInjector, event FaultEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if injector == nil {
		return nil
	}
	if err := injector.Inject(ctx, event); err != nil {
		return err
	}
	return ctx.Err()
}
