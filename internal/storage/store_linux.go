//go:build linux

package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type systemOperations struct {
	openat    func(int, string, int, uint32) (int, error)
	mkdirat   func(int, string, uint32) error
	read      func(int, []byte) (int, error)
	write     func(int, []byte) (int, error)
	fsync     func(int) error
	renameat2 func(int, string, int, string, uint) error
	unlinkat  func(int, string, int) error
	fstatat   func(int, string, *unix.Stat_t, int) error
}

var linuxOperations = systemOperations{
	openat:    unix.Openat,
	mkdirat:   unix.Mkdirat,
	read:      unix.Read,
	write:     unix.Write,
	fsync:     unix.Fsync,
	renameat2: unix.Renameat2,
	unlinkat:  unix.Unlinkat,
	fstatat:   unix.Fstatat,
}

type Options struct {
	Faults FaultInjector
}

type Store struct {
	rootFD int
	faults FaultInjector
	ops    systemOperations
	mu     sync.RWMutex
	closed bool
}

func Open(root string, options Options) (*Store, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrInvalidKey
	}
	fd, err := openRootWithoutSymlinks(root)
	if err != nil {
		return nil, classifyError("open root", err)
	}
	return &Store{rootFD: fd, faults: options.Faults, ops: linuxOperations}, nil
}

func openRootWithoutSymlinks(root string) (int, error) {
	currentFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
		if component == "" {
			continue
		}
		nextFD, openErr := unix.Openat(currentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(currentFD)
		if openErr != nil {
			return -1, openErr
		}
		currentFD = nextFD
	}
	return currentFD, nil
}

func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	return unix.Close(store.rootFD)
}

func (store *Store) BeginOriginal(ctx context.Context, key OriginalKey, attempt AttemptID) (*Temp, error) {
	if _, err := ParseOriginalKey(key.String()); err != nil || !attempt.uuidV4.valid() {
		return nil, ErrInvalidKey
	}
	return store.begin(ctx, key.String(), attempt)
}

// BeginOriginalUpload stages an original before its content-detected extension
// is known. Publish accepts only a closed OriginalExtension and constructs the
// final key from the OriginalID retained here.
func (store *Store) BeginOriginalUpload(ctx context.Context, originalID OriginalID, attempt AttemptID) (*OriginalUpload, error) {
	if !originalID.uuidV4.valid() || !attempt.uuidV4.valid() {
		return nil, ErrInvalidKey
	}
	id := originalID.String()
	directories := []string{"originals", id[0:2], id}
	stagingKey := strings.Join(append(append([]string(nil), directories...), "original"), "/")
	temporary, err := store.beginAt(ctx, stagingKey, directories, "original", "", attempt)
	if err != nil {
		return nil, err
	}
	return &OriginalUpload{temporary: temporary, originalID: originalID}, nil
}

func (store *Store) BeginRendition(ctx context.Context, key RenditionKey, attempt AttemptID) (*Temp, error) {
	if _, err := ParseRenditionKey(key.String()); err != nil || !attempt.uuidV4.valid() {
		return nil, ErrInvalidKey
	}
	return store.begin(ctx, key.String(), attempt)
}

type Temp struct {
	store            *Store
	ctx              context.Context
	parentFD         int
	fileFD           int
	key              string
	tempName         string
	finalName        string
	cleanupParentFD  int
	cleanupDirName   string
	finished         bool
	published        bool
	active           bool
	stateChanged     chan struct{}
	sealed           bool
	sealedInfo       ObjectInfo
	sealedValidation validationSignature
	writeFailed      error
	mu               sync.Mutex
}

type OriginalUpload struct {
	temporary  *Temp
	originalID OriginalID
}

func (upload *OriginalUpload) Write(value []byte) (int, error) {
	return upload.temporary.Write(value)
}

// UseReadOnlyFile calls use with a separately opened read-only descriptor.
// The descriptor has an independent offset and is closed before this method
// returns or propagates a panic. Storage operations are serialized with the
// callback without holding an internal mutex. The callback must not call back
// into this upload; such calls wait for the callback to return.
func (upload *OriginalUpload) UseReadOnlyFile(use func(*os.File) error) error {
	return upload.temporary.useReadOnlyFile(use)
}

func (upload *OriginalUpload) Seal(ctx context.Context, validation Validation) (ObjectInfo, error) {
	return upload.temporary.seal(ctx, validation)
}

func (upload *OriginalUpload) PublishSealed(ctx context.Context, extension OriginalExtension) (OriginalKey, ObjectInfo, error) {
	key, err := NewOriginalKey(upload.originalID, extension)
	if err != nil {
		return OriginalKey{}, ObjectInfo{}, err
	}
	_, finalName := splitKey(key.String())
	info, err := upload.temporary.publishSealed(ctx, finalName, key.String())
	return key, info, err
}

func (upload *OriginalUpload) Publish(ctx context.Context, extension OriginalExtension, validation Validation) (OriginalKey, ObjectInfo, error) {
	key, err := NewOriginalKey(upload.originalID, extension)
	if err != nil {
		return OriginalKey{}, ObjectInfo{}, err
	}
	if _, err := upload.Seal(ctx, validation); err != nil {
		return key, ObjectInfo{}, err
	}
	return upload.PublishSealed(ctx, extension)
}

func (upload *OriginalUpload) Abort(ctx context.Context) error {
	return upload.temporary.Abort(ctx)
}

type Validation struct {
	ExpectedSize   int64
	ExpectedSHA256 *[sha256.Size]byte
	Validate       func(context.Context, *os.File) error
}

type ObjectInfo struct {
	Size   int64
	SHA256 [sha256.Size]byte
}

type validationSignature struct {
	expectedSize int64
	hasSHA256    bool
	sha256       [sha256.Size]byte
	hasValidate  bool
}

func signatureOf(validation Validation) validationSignature {
	signature := validationSignature{expectedSize: validation.ExpectedSize}
	if validation.ExpectedSHA256 != nil {
		signature.hasSHA256 = true
		signature.sha256 = *validation.ExpectedSHA256
	}
	if validation.Validate != nil {
		signature.hasValidate = true
	}
	return signature
}

type DeleteResult struct {
	Missing bool
}

func (store *Store) begin(ctx context.Context, key string, attempt AttemptID) (*Temp, error) {
	directories, finalName := splitKey(key)
	return store.beginAt(ctx, key, directories, finalName, finalName, attempt)
}

func (store *Store) beginAt(ctx context.Context, key string, directories []string, tempBase, finalName string, attempt AttemptID) (*Temp, error) {
	store.mu.RLock()
	if store.closed {
		store.mu.RUnlock()
		return nil, ErrClosed
	}
	parentFD, err := unix.Dup(store.rootFD)
	store.mu.RUnlock()
	if err != nil {
		return nil, classifyError("duplicate root", err)
	}
	cleanupParentFD := -1
	cleanupDirName := ""
	for depth, directory := range directories {
		created := false
		nextFD, openErr := store.ops.openat(parentFD, directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil && errors.Is(openErr, unix.ENOENT) {
			if err = store.inject(ctx, BoundaryDirectoryCreate, Before, key, depth); err == nil {
				err = store.ops.mkdirat(parentFD, directory, 0o700)
			}
			created = err == nil
			if errors.Is(err, unix.EEXIST) {
				// Another trusted publisher won the same directory creation race.
				// Open and sync both sides of that new entry ourselves before use.
				err = nil
			}
			if err == nil {
				nextFD, err = store.ops.openat(parentFD, directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
			if err == nil && created && depth == len(directories)-1 && finalName == "" {
				cleanupParentFD, err = unix.Dup(parentFD)
				if err != nil {
					cleanupErr := store.removeCreatedUploadDirectory(context.WithoutCancel(ctx), nextFD, parentFD, directory, key)
					_ = unix.Close(nextFD)
					_ = unix.Close(parentFD)
					return nil, errors.Join(classifyError("pin upload directory parent", err), cleanupErr)
				}
				unix.CloseOnExec(cleanupParentFD)
				cleanupDirName = directory
			}
			if err == nil && created {
				err = store.inject(ctx, BoundaryDirectoryCreate, After, key, depth)
			}
		} else {
			err = openErr
		}
		// Sync every traversed component. This is intentionally redundant for
		// old directories so a retry repairs a prior crash/failure after mkdir.
		if err == nil {
			if durable, syncErr := store.syncBoundary(ctx, nextFD, BoundaryNewDirectorySync, key, depth, "sync key directory"); syncErr != nil {
				if durable {
					err = syncErr
				} else {
					err = errors.Join(ErrDurability, syncErr)
				}
			}
		}
		if err == nil {
			if durable, syncErr := store.syncBoundary(ctx, parentFD, BoundaryParentDirectorySync, key, depth, "sync key directory parent"); syncErr != nil {
				if durable {
					err = syncErr
				} else {
					err = errors.Join(ErrDurability, syncErr)
				}
			}
		}
		if err != nil {
			var cleanupErr error
			if cleanupParentFD >= 0 {
				cleanupErr = store.cleanupFailedBegin(context.WithoutCancel(ctx), nextFD, -1, "", cleanupParentFD, cleanupDirName, key)
			}
			_ = unix.Close(parentFD)
			if cleanupParentFD >= 0 {
				_ = unix.Close(cleanupParentFD)
			}
			if nextFD >= 0 {
				_ = unix.Close(nextFD)
			}
			if errors.Is(err, ErrDurability) {
				return nil, errors.Join(err, cleanupErr)
			}
			return nil, errors.Join(classifyError("open key directory", err), cleanupErr)
		}
		_ = unix.Close(parentFD)
		parentFD = nextFD
	}
	tempName := "." + tempBase + "." + attempt.String() + ".tmp"
	if err := store.inject(ctx, BoundaryTempCreate, Before, key, len(directories)); err != nil {
		cleanupErr := store.cleanupFailedBegin(context.WithoutCancel(ctx), parentFD, -1, "", cleanupParentFD, cleanupDirName, key)
		_ = unix.Close(parentFD)
		if cleanupParentFD >= 0 {
			_ = unix.Close(cleanupParentFD)
		}
		return nil, errors.Join(err, cleanupErr)
	}
	fileFD, err := store.ops.openat(parentFD, tempName, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err == nil {
		err = store.inject(ctx, BoundaryTempCreate, After, key, len(directories))
		if err == nil {
			err = ctx.Err()
		}
	}
	if err != nil {
		cleanupErr := store.cleanupFailedBegin(context.WithoutCancel(ctx), parentFD, fileFD, tempName, cleanupParentFD, cleanupDirName, key)
		if fileFD >= 0 {
			_ = unix.Close(fileFD)
		}
		_ = unix.Close(parentFD)
		if cleanupParentFD >= 0 {
			_ = unix.Close(cleanupParentFD)
		}
		return nil, errors.Join(classifyError("create temporary object", err), cleanupErr)
	}
	return &Temp{
		store: store, ctx: ctx, parentFD: parentFD, fileFD: fileFD, key: key,
		tempName: tempName, finalName: finalName, cleanupParentFD: cleanupParentFD,
		cleanupDirName: cleanupDirName, stateChanged: make(chan struct{}),
	}, nil
}

func (store *Store) cleanupFailedBegin(ctx context.Context, uploadFD, fileFD int, tempName string, cleanupParentFD int, cleanupDirName, key string) error {
	var cleanupErr error
	if fileFD >= 0 {
		cleanupErr = store.removeOwnedTemp(ctx, uploadFD, fileFD, tempName, key)
	}
	if cleanupParentFD >= 0 && uploadFD >= 0 {
		cleanupErr = errors.Join(cleanupErr, store.removeCreatedUploadDirectory(ctx, uploadFD, cleanupParentFD, cleanupDirName, key))
	}
	return cleanupErr
}

func (store *Store) removeOwnedTemp(ctx context.Context, parentFD, fileFD int, tempName, key string) error {
	var pinned, leaf unix.Stat_t
	if err := unix.Fstat(fileFD, &pinned); err != nil {
		return classifyError("stat pinned temporary object for cleanup", err)
	}
	if err := store.ops.fstatat(parentFD, tempName, &leaf, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return classifyError("stat temporary object for cleanup", err)
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFREG || leaf.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrUnexpectedType
	}
	if pinned.Dev != leaf.Dev || pinned.Ino != leaf.Ino {
		return errors.Join(ErrValidation, errors.New("temporary object identity changed during cleanup"))
	}
	if err := store.ops.unlinkat(parentFD, tempName, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return errors.Join(ErrOutcomeUncertain, ErrDurability, classifyError("clean failed temporary object", err))
	}
	durable, err := store.syncDeleteDirectory(ctx, parentFD, key)
	if err == nil || durable {
		return err
	}
	return errors.Join(ErrOutcomeUncertain, ErrDurability, err)
}

func (temp *Temp) Write(value []byte) (written int, returnErr error) {
	if err := temp.startOperation(temp.ctx); err != nil {
		return 0, err
	}
	defer temp.endOperation()
	defer func() {
		if returnErr != nil {
			temp.mu.Lock()
			temp.writeFailed = returnErr
			temp.mu.Unlock()
		}
	}()
	temp.mu.Lock()
	sealed := temp.sealed
	temp.mu.Unlock()
	if sealed {
		return 0, ErrClosed
	}
	for written < len(value) {
		if err := temp.store.inject(temp.ctx, BoundaryWrite, Before, temp.key, written); err != nil {
			return written, err
		}
		count, err := temp.store.ops.write(temp.fileFD, value[written:])
		if count < 0 || count > len(value)-written {
			return written, io.ErrShortWrite
		}
		written += count
		if injected := temp.store.inject(temp.ctx, BoundaryWrite, After, temp.key, written); injected != nil && err == nil {
			err = injected
		}
		if err != nil {
			return written, classifyError("write temporary object", err)
		}
		if count == 0 {
			return written, io.ErrNoProgress
		}
	}
	return written, nil
}

// UseWritableFile calls a trusted streaming/codec operation with a duplicate
// descriptor and closes it before returning. Storage operations cannot run
// concurrently, so no writable descriptor supplied by this API survives seal.
// The callback must not call back into this Temp.
func (temp *Temp) UseWritableFile(use func(*os.File) error) (returnErr error) {
	if err := temp.startOperation(temp.ctx); err != nil {
		return err
	}
	defer temp.endOperation()
	temp.mu.Lock()
	sealed := temp.sealed
	temp.mu.Unlock()
	if sealed {
		return ErrClosed
	}
	fd, err := unix.Dup(temp.fileFD)
	if err != nil {
		return classifyError("duplicate temporary object", err)
	}
	unix.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), "storage-output")
	returned := false
	defer func() {
		closeErr := file.Close()
		temp.mu.Lock()
		defer temp.mu.Unlock()
		if !returned {
			temp.writeFailed = errors.New("writable callback did not return")
		} else if returnErr != nil {
			temp.writeFailed = returnErr
		} else if closeErr != nil {
			temp.writeFailed = closeErr
			returnErr = closeErr
		}
	}()
	returnErr = use(file)
	returned = true
	return returnErr
}

func (temp *Temp) useReadOnlyFile(use func(*os.File) error) (returnErr error) {
	if err := temp.startOperation(temp.ctx); err != nil {
		return err
	}
	defer temp.endOperation()
	if err := temp.store.inject(temp.ctx, BoundaryReadOnlyProbe, Before, temp.key, 0); err != nil {
		return err
	}
	fd, err := openPinnedReadOnly(temp.fileFD)
	if err != nil {
		return classifyError("open pinned temporary object for reading", err)
	}
	file := os.NewFile(uintptr(fd), "storage-original-input")
	defer func() {
		if closeErr := file.Close(); returnErr == nil && closeErr != nil {
			returnErr = classifyError("close temporary object reader", closeErr)
		}
	}()
	returnErr = use(file)
	if returnErr == nil {
		returnErr = temp.store.inject(temp.ctx, BoundaryReadOnlyProbe, After, temp.key, 0)
	}
	return returnErr
}

func (temp *Temp) Publish(ctx context.Context, validation Validation) (ObjectInfo, error) {
	if _, err := temp.seal(ctx, validation); err != nil {
		return ObjectInfo{}, err
	}
	return temp.publishSealed(ctx, temp.finalName, temp.key)
}

func (temp *Temp) seal(ctx context.Context, validation Validation) (ObjectInfo, error) {
	if err := temp.startOperation(ctx); err != nil {
		return ObjectInfo{}, err
	}
	defer temp.endOperation()
	signature := signatureOf(validation)
	temp.mu.Lock()
	if temp.sealed {
		info := temp.sealedInfo
		compatible := !temp.sealedValidation.hasValidate && validation.Validate == nil && temp.sealedValidation == signature
		temp.mu.Unlock()
		if !compatible {
			return ObjectInfo{}, errors.Join(ErrValidation, errors.New("upload already sealed with different validation"))
		}
		return info, nil
	}
	writeFailed := temp.writeFailed
	temp.mu.Unlock()
	if writeFailed != nil {
		return ObjectInfo{}, errors.Join(ErrValidation, writeFailed)
	}
	if durable, err := temp.store.syncBoundary(ctx, temp.fileFD, BoundaryFileSync, temp.key, 0, "sync temporary object"); err != nil {
		if !durable {
			err = errors.Join(ErrDurability, err)
		}
		return ObjectInfo{}, &PublishError{operation: "file sync", cause: err}
	}
	info, err := temp.inspectPinned(ctx, temp.key, validation)
	if err != nil {
		return ObjectInfo{}, err
	}
	temp.mu.Lock()
	temp.sealed = true
	temp.sealedInfo = info
	temp.sealedValidation = signature
	temp.mu.Unlock()
	return info, nil
}

func (temp *Temp) publishSealed(ctx context.Context, finalName, key string) (ObjectInfo, error) {
	if err := temp.startOperation(ctx); err != nil {
		return ObjectInfo{}, err
	}
	defer temp.endOperation()
	temp.mu.Lock()
	sealed, info := temp.sealed, temp.sealedInfo
	temp.mu.Unlock()
	if !sealed {
		return ObjectInfo{}, errors.Join(ErrValidation, errors.New("upload is not sealed"))
	}
	if err := temp.store.inject(ctx, BoundaryRename, Before, key, 0); err != nil {
		return ObjectInfo{}, err
	}
	if err := temp.verifyTempIdentity(); err != nil {
		return ObjectInfo{}, err
	}
	err := temp.store.ops.renameat2(temp.parentFD, temp.tempName, temp.parentFD, finalName, unix.RENAME_NOREPLACE)
	if err != nil {
		return ObjectInfo{}, classifyError("publish object", err)
	}
	temp.published = true
	if err := temp.store.inject(ctx, BoundaryRename, After, key, 0); err != nil {
		temp.finish()
		return ObjectInfo{}, &PublishError{Published: true, Uncertain: true, operation: "after rename", cause: errors.Join(ErrOutcomeUncertain, err)}
	}
	if durable, err := temp.store.syncBoundary(ctx, temp.parentFD, BoundaryFinalDirectorySync, key, 0, "sync final object directory"); err != nil {
		temp.finish()
		if durable {
			return ObjectInfo{}, &PublishError{Published: true, operation: "after final directory sync", cause: err}
		}
		return ObjectInfo{}, &PublishError{Published: true, Uncertain: true, operation: "final directory sync", cause: errors.Join(ErrOutcomeUncertain, ErrDurability, err)}
	}
	temp.finish()
	return info, nil
}

func (temp *Temp) inspectPinned(ctx context.Context, key string, validation Validation) (ObjectInfo, error) {
	if err := temp.store.inject(ctx, BoundaryValidation, Before, key, 0); err != nil {
		return ObjectInfo{}, err
	}
	offset, err := unix.Seek(temp.fileFD, 0, io.SeekCurrent)
	if err != nil {
		return ObjectInfo{}, errors.Join(ErrValidation, classifyError("get object offset for validation", err))
	}
	defer func() { _, _ = unix.Seek(temp.fileFD, offset, io.SeekStart) }()
	if _, err := unix.Seek(temp.fileFD, 0, io.SeekStart); err != nil {
		return ObjectInfo{}, errors.Join(ErrValidation, classifyError("seek object for validation", err))
	}
	hash := sha256.New()
	var size int64
	buffer := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return ObjectInfo{}, err
		}
		count, readErr := temp.store.ops.read(temp.fileFD, buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
			size += int64(count)
		}
		if err := ctx.Err(); err != nil {
			return ObjectInfo{}, err
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if errors.Is(readErr, unix.EINTR) {
			continue
		}
		if readErr != nil {
			return ObjectInfo{}, errors.Join(ErrValidation, readErr)
		}
		if count == 0 {
			break
		}
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	if validation.ExpectedSize >= 0 && size != validation.ExpectedSize {
		return ObjectInfo{}, ErrValidation
	}
	if validation.ExpectedSHA256 != nil && digest != *validation.ExpectedSHA256 {
		return ObjectInfo{}, ErrValidation
	}
	if validation.Validate != nil {
		readFD, err := openPinnedReadOnly(temp.fileFD)
		if err != nil {
			return ObjectInfo{}, errors.Join(ErrValidation, classifyError("open pinned object for validation", err))
		}
		file := os.NewFile(uintptr(readFD), "storage-validation")
		validateErr, closeErr := func() (returnErr, closeErr error) {
			defer func() { closeErr = file.Close() }()
			returnErr = validation.Validate(ctx, file)
			return returnErr, nil
		}()
		if validateErr != nil {
			return ObjectInfo{}, errors.Join(ErrValidation, validateErr)
		}
		if closeErr != nil {
			return ObjectInfo{}, errors.Join(ErrValidation, closeErr)
		}
		if err := ctx.Err(); err != nil {
			return ObjectInfo{}, errors.Join(ErrValidation, err)
		}
	}
	if err := temp.store.inject(ctx, BoundaryValidation, After, key, 0); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Size: size, SHA256: digest}, nil
}

func (temp *Temp) Abort(ctx context.Context) error {
	if err := temp.startOperation(ctx); err != nil {
		if errors.Is(err, ErrClosed) {
			return nil
		}
		return err
	}
	defer temp.endOperation()
	var leafStat unix.Stat_t
	statErr := temp.store.ops.fstatat(temp.parentFD, temp.tempName, &leafStat, unix.AT_SYMLINK_NOFOLLOW)
	if statErr == nil {
		if err := temp.verifyTempIdentity(); err != nil {
			temp.finish()
			return err
		}
	} else if !errors.Is(statErr, unix.ENOENT) {
		return classifyError("stat temporary object for abort", statErr)
	}
	if err := temp.store.ops.unlinkat(temp.parentFD, temp.tempName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return classifyError("abort temporary object", err)
	}
	// Once the namespace was (or may already have been) mutated, cancellation
	// cannot safely skip the directory sync or descriptor cleanup.
	durable, err := temp.store.syncDeleteDirectory(context.WithoutCancel(ctx), temp.parentFD, temp.key)
	if err != nil {
		temp.finish()
		if durable {
			return err
		}
		return errors.Join(ErrOutcomeUncertain, ErrDurability, err)
	}
	if err := temp.removeCreatedUploadDirectory(context.WithoutCancel(ctx)); err != nil {
		temp.finish()
		return err
	}
	temp.finish()
	return nil
}

func (temp *Temp) finish() {
	temp.mu.Lock()
	if temp.finished {
		temp.mu.Unlock()
		return
	}
	temp.finished = true
	cleanupParentFD := temp.cleanupParentFD
	temp.cleanupParentFD = -1
	temp.mu.Unlock()
	_ = unix.Close(temp.fileFD)
	_ = unix.Close(temp.parentFD)
	if cleanupParentFD >= 0 {
		_ = unix.Close(cleanupParentFD)
	}
}

func (temp *Temp) startOperation(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		temp.mu.Lock()
		if temp.finished {
			temp.mu.Unlock()
			return ErrClosed
		}
		if !temp.active {
			temp.active = true
			temp.mu.Unlock()
			return nil
		}
		changed := temp.stateChanged
		temp.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (temp *Temp) endOperation() {
	temp.mu.Lock()
	temp.active = false
	close(temp.stateChanged)
	temp.stateChanged = make(chan struct{})
	temp.mu.Unlock()
}

func openPinnedReadOnly(fd int) (int, error) {
	return unix.Open(fmt.Sprintf("/proc/self/fd/%d", fd), unix.O_RDONLY|unix.O_CLOEXEC, 0)
}

func (temp *Temp) verifyTempIdentity() error {
	var pinned, leaf unix.Stat_t
	if err := unix.Fstat(temp.fileFD, &pinned); err != nil {
		return classifyError("stat pinned temporary object", err)
	}
	if err := temp.store.ops.fstatat(temp.parentFD, temp.tempName, &leaf, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return classifyError("stat temporary object identity", err)
	}
	if leaf.Mode&unix.S_IFMT == unix.S_IFLNK {
		return ErrSymlink
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFREG || leaf.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrUnexpectedType
	}
	if pinned.Dev != leaf.Dev || pinned.Ino != leaf.Ino {
		return errors.Join(ErrValidation, errors.New("temporary object identity changed"))
	}
	return nil
}

func (temp *Temp) removeCreatedUploadDirectory(ctx context.Context) error {
	if temp.cleanupParentFD < 0 {
		return nil
	}
	return temp.store.removeCreatedUploadDirectory(ctx, temp.parentFD, temp.cleanupParentFD, temp.cleanupDirName, temp.key)
}

func (store *Store) removeCreatedUploadDirectory(ctx context.Context, uploadFD, cleanupParentFD int, cleanupDirName, key string) error {
	var pinned, leaf unix.Stat_t
	if err := unix.Fstat(uploadFD, &pinned); err != nil {
		return classifyError("stat pinned upload directory", err)
	}
	if err := store.ops.fstatat(cleanupParentFD, cleanupDirName, &leaf, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return classifyError("stat upload directory for cleanup", err)
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFDIR || leaf.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrUnexpectedType
	}
	if pinned.Dev != leaf.Dev || pinned.Ino != leaf.Ino {
		return errors.Join(ErrValidation, errors.New("upload directory identity changed"))
	}
	if err := store.inject(ctx, BoundaryUploadDirectoryDelete, Before, key, 0); err != nil {
		return err
	}
	err := store.ops.unlinkat(cleanupParentFD, cleanupDirName, unix.AT_REMOVEDIR)
	if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
		return nil
	}
	if err != nil {
		return classifyError("remove empty upload directory", err)
	}
	var deleteErr error
	if err := store.inject(ctx, BoundaryUploadDirectoryDelete, After, key, 0); err != nil {
		deleteErr = errors.Join(ErrOutcomeUncertain, err)
	}
	durable, err := store.syncBoundary(ctx, cleanupParentFD, BoundaryUploadDirectorySync, key, 0, "sync removed upload directory parent")
	if err == nil || durable {
		return errors.Join(deleteErr, err)
	}
	return errors.Join(deleteErr, ErrOutcomeUncertain, ErrDurability, err)
}

func (store *Store) OpenOriginal(ctx context.Context, key OriginalKey) (*os.File, error) {
	if _, err := ParseOriginalKey(key.String()); err != nil {
		return nil, ErrInvalidKey
	}
	return store.openObject(ctx, key.String())
}

func (store *Store) OpenRendition(ctx context.Context, key RenditionKey) (*os.File, error) {
	if _, err := ParseRenditionKey(key.String()); err != nil {
		return nil, ErrInvalidKey
	}
	return store.openObject(ctx, key.String())
}

func (store *Store) openObject(ctx context.Context, key string) (*os.File, error) {
	parentFD, finalName, release, err := store.openParent(ctx, key)
	if err != nil {
		return nil, err
	}
	defer release()
	fd, err := store.ops.openat(parentFD, finalName, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, classifyError("open object", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		if err != nil {
			return nil, classifyError("stat object", err)
		}
		return nil, ErrUnexpectedType
	}
	return os.NewFile(uintptr(fd), "storage-object"), nil
}

func (store *Store) DeleteOriginal(ctx context.Context, key OriginalKey) (DeleteResult, error) {
	if _, err := ParseOriginalKey(key.String()); err != nil {
		return DeleteResult{}, ErrInvalidKey
	}
	return store.deleteObject(ctx, key.String())
}

func (store *Store) DeleteRendition(ctx context.Context, key RenditionKey) (DeleteResult, error) {
	if _, err := ParseRenditionKey(key.String()); err != nil {
		return DeleteResult{}, ErrInvalidKey
	}
	return store.deleteObject(ctx, key.String())
}

func (store *Store) deleteObject(ctx context.Context, key string) (DeleteResult, error) {
	parentFD, finalName, release, err := store.openParent(ctx, key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DeleteResult{Missing: true}, nil
		}
		return DeleteResult{}, err
	}
	defer release()
	var stat unix.Stat_t
	if err := store.ops.fstatat(parentFD, finalName, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			durable, syncErr := store.syncDeleteDirectory(ctx, parentFD, key)
			if syncErr != nil {
				if durable {
					return DeleteResult{Missing: true}, syncErr
				}
				return DeleteResult{}, errors.Join(ErrOutcomeUncertain, ErrDurability, syncErr)
			}
			return DeleteResult{Missing: true}, nil
		}
		return DeleteResult{}, classifyError("stat object for delete", err)
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
		return DeleteResult{}, ErrSymlink
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return DeleteResult{}, ErrUnexpectedType
	}
	if err := store.inject(ctx, BoundaryDelete, Before, key, 0); err != nil {
		return DeleteResult{}, err
	}
	if err := store.ops.unlinkat(parentFD, finalName, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			durable, syncErr := store.syncDeleteDirectory(ctx, parentFD, key)
			if syncErr != nil {
				if durable {
					return DeleteResult{Missing: true}, syncErr
				}
				return DeleteResult{}, errors.Join(ErrOutcomeUncertain, ErrDurability, syncErr)
			}
			return DeleteResult{Missing: true}, nil
		}
		return DeleteResult{}, classifyError("delete object", err)
	}
	if err := store.inject(ctx, BoundaryDelete, After, key, 0); err != nil {
		return DeleteResult{}, errors.Join(ErrOutcomeUncertain, err)
	}
	durable, err := store.syncDeleteDirectory(ctx, parentFD, key)
	if err != nil {
		if durable {
			return DeleteResult{}, err
		}
		return DeleteResult{}, errors.Join(ErrOutcomeUncertain, ErrDurability, err)
	}
	return DeleteResult{}, nil
}

func (store *Store) openParent(ctx context.Context, key string) (int, string, func(), error) {
	store.mu.RLock()
	if store.closed {
		store.mu.RUnlock()
		return -1, "", func() {}, ErrClosed
	}
	directories, finalName := splitKey(key)
	currentFD, err := unix.Dup(store.rootFD)
	if err != nil {
		store.mu.RUnlock()
		return -1, "", func() {}, classifyError("duplicate root", err)
	}
	for _, directory := range directories {
		if err := ctx.Err(); err != nil {
			_ = unix.Close(currentFD)
			store.mu.RUnlock()
			return -1, "", func() {}, err
		}
		nextFD, err := store.ops.openat(currentFD, directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(currentFD)
		if err != nil {
			store.mu.RUnlock()
			return -1, "", func() {}, classifyError("open key directory", err)
		}
		currentFD = nextFD
	}
	release := func() {
		_ = unix.Close(currentFD)
		store.mu.RUnlock()
	}
	return currentFD, finalName, release, nil
}

func (store *Store) Probe(ctx context.Context) (returnErr error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	name := fmt.Sprintf(".nmcp-probe-%x", token[:])
	finalName := name + ".published"
	fd, err := store.ops.openat(store.rootFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return classifyError("create probe", err)
	}
	mutated := true
	defer func() {
		var cleanupErrors []error
		if err := unix.Close(fd); err != nil {
			cleanupErrors = append(cleanupErrors, classifyError("close probe", err))
		}
		removed := false
		for _, candidate := range []string{name, finalName} {
			if err := store.ops.unlinkat(store.rootFD, candidate, 0); err == nil {
				removed = true
			} else if !errors.Is(err, unix.ENOENT) {
				cleanupErrors = append(cleanupErrors, classifyError("clean probe", err))
			}
		}
		if mutated || removed {
			if err := store.ops.fsync(store.rootFD); err != nil {
				cleanupErrors = append(cleanupErrors, errors.Join(ErrDurability, classifyError("sync probe cleanup", err)))
			}
		}
		returnErr = errors.Join(append([]error{returnErr}, cleanupErrors...)...)
	}()
	if _, err := writeAll(store.ops.write, fd, []byte{0x4e}); err != nil {
		return classifyError("write probe", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.ops.fsync(fd); err != nil {
		return errors.Join(ErrDurability, classifyError("sync probe", err))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.ops.renameat2(store.rootFD, name, store.rootFD, finalName, unix.RENAME_NOREPLACE); err != nil {
		return classifyError("publish probe", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.ops.fsync(store.rootFD); err != nil {
		return errors.Join(ErrDurability, classifyError("sync probe directory", err))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.ops.unlinkat(store.rootFD, finalName, 0); err != nil {
		return classifyError("delete probe", err)
	}
	if err := store.ops.fsync(store.rootFD); err != nil {
		return errors.Join(ErrDurability, classifyError("sync probe deletion", err))
	}
	return nil
}

// DatabaseCheckpoint exposes only the two upload commit fault boundaries to
// database coordination code. General fault injection remains internal.
func (store *Store) DatabaseCheckpoint(ctx context.Context, boundary Boundary, key string) error {
	phase := Before
	if boundary == BoundaryAfterDBCommit {
		phase = After
	} else if boundary != BoundaryBeforeDBCommit {
		return ErrInvalidDatabaseBoundary
	}
	return store.inject(ctx, boundary, phase, key, 0)
}

func (store *Store) inject(ctx context.Context, boundary Boundary, phase Phase, key string, depth int) error {
	return Inject(ctx, store.faults, FaultEvent{Boundary: boundary, Phase: phase, Key: key, Depth: depth})
}

func (store *Store) syncBoundary(ctx context.Context, fd int, boundary Boundary, key string, depth int, operation string) (bool, error) {
	if err := store.inject(ctx, boundary, Before, key, depth); err != nil {
		return false, err
	}
	if err := store.ops.fsync(fd); err != nil {
		return false, classifyError(operation, err)
	}
	if err := store.inject(ctx, boundary, After, key, depth); err != nil {
		return true, err
	}
	return true, nil
}

func (store *Store) syncDeleteDirectory(ctx context.Context, fd int, key string) (bool, error) {
	return store.syncBoundary(ctx, fd, BoundaryDeleteDirectorySync, key, 0, "sync deleted object directory")
}

func splitKey(key string) ([]string, string) {
	parts := strings.Split(key, "/")
	return parts[:len(parts)-1], parts[len(parts)-1]
}

func writeAll(write func(int, []byte) (int, error), fd int, value []byte) (int, error) {
	written := 0
	for written < len(value) {
		count, err := write(fd, value[written:])
		if count < 0 || count > len(value)-written {
			return written, io.ErrShortWrite
		}
		written += count
		if err != nil {
			return written, err
		}
		if count == 0 {
			return written, io.ErrNoProgress
		}
	}
	return written, nil
}

func classifyError(operation string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, unix.ELOOP):
		return errors.Join(ErrSymlink, fmt.Errorf("%s failed", operation))
	case errors.Is(err, unix.ENOTDIR):
		return errors.Join(ErrUnexpectedType, fmt.Errorf("%s failed", operation))
	case errors.Is(err, unix.EEXIST):
		return errors.Join(ErrCollision, fmt.Errorf("%s failed", operation))
	case errors.Is(err, unix.EROFS), errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return errors.Join(ErrReadOnly, fmt.Errorf("%s failed", operation))
	case errors.Is(err, unix.ENOENT):
		return errors.Join(os.ErrNotExist, fmt.Errorf("%s failed", operation))
	default:
		return fmt.Errorf("%s failed: %w", operation, err)
	}
}
