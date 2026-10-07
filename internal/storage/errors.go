package storage

import (
	"errors"
	"fmt"
)

var (
	ErrSymlink          = errors.New("storage symlink refused")
	ErrCollision        = errors.New("storage object collision")
	ErrReadOnly         = errors.New("storage is read-only")
	ErrValidation       = errors.New("storage validation failed")
	ErrDurability       = errors.New("storage durability operation failed")
	ErrOutcomeUncertain = errors.New("storage outcome is uncertain")
	ErrUnexpectedType   = errors.New("unexpected storage object type")
	ErrClosed           = errors.New("storage is closed")
)

type PublishError struct {
	Published bool
	Uncertain bool
	operation string
	cause     error
}

func (err *PublishError) Error() string {
	return fmt.Sprintf("storage publish %s failed", err.operation)
}

func (err *PublishError) Unwrap() error { return err.cause }

type QuarantineError struct {
	Moved     bool
	Uncertain bool
	operation string
	cause     error
}

func (err *QuarantineError) Error() string {
	return fmt.Sprintf("storage quarantine %s failed", err.operation)
}

func (err *QuarantineError) Unwrap() error { return err.cause }
