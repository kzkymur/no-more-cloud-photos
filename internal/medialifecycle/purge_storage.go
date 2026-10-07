package medialifecycle

import (
	"context"
	"errors"

	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func StoragePurgeFileAction(store *storage.Store) PurgeFileAction {
	return func(ctx context.Context, file PurgeFile) (PurgeFileDisposition, error) {
		if store == nil || file.SizeBytes < 0 {
			return "", storage.ErrValidation
		}
		expectation := storage.DeleteExpectation{ExpectedSize: file.SizeBytes}
		var result storage.DeleteResult
		var err error
		switch file.Kind {
		case PurgeFileOriginal:
			key, parseErr := storage.ParseOriginalKey(file.RelativePath)
			if parseErr != nil || key.OriginalID().String() != file.ObjectID {
				return "", storage.ErrValidation
			}
			result, err = store.DeleteOriginal(ctx, key, expectation)
		case PurgeFileRendition:
			key, parseErr := storage.ParseRenditionKey(file.RelativePath)
			if parseErr != nil || key.RenditionID().String() != file.ObjectID {
				return "", storage.ErrValidation
			}
			result, err = store.DeleteRendition(ctx, key, expectation)
		default:
			return "", storage.ErrValidation
		}
		if err != nil {
			return "", classifyPurgeStorageError(err)
		}
		if result.Missing {
			return PurgeFileMissing, nil
		}
		return PurgeFileDeleted, nil
	}
}

func classifyPurgeStorageError(err error) error {
	classified := errors.New("purge storage operation failed")
	if errors.Is(err, context.Canceled) {
		classified = errors.Join(classified, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		classified = errors.Join(classified, context.DeadlineExceeded)
	}
	for _, stable := range []error{
		storage.ErrNoSpace,
		storage.ErrQuota,
		storage.ErrPermission,
		storage.ErrReadOnly,
		storage.ErrOutcomeUncertain,
		storage.ErrDurability,
		storage.ErrValidation,
		storage.ErrSymlink,
		storage.ErrUnexpectedType,
	} {
		if errors.Is(err, stable) {
			classified = errors.Join(classified, stable)
		}
	}
	return classified
}
