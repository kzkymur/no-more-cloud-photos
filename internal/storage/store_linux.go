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
	"time"

	"golang.org/x/sys/unix"
)

type systemOperations struct {
	openat    func(int, string, int, uint32) (int, error)
	openat2   func(int, string, *unix.OpenHow) (int, error)
	mkdirat   func(int, string, uint32) error
	read      func(int, []byte) (int, error)
	write     func(int, []byte) (int, error)
	fsync     func(int) error
	renameat2 func(int, string, int, string, uint) error
	unlinkat  func(int, string, int) error
	fstat     func(int, *unix.Stat_t) error
	fstatat   func(int, string, *unix.Stat_t, int) error
	readFile  func(string) ([]byte, error)
}

var linuxOperations = systemOperations{
	openat:    unix.Openat,
	openat2:   unix.Openat2,
	mkdirat:   unix.Mkdirat,
	read:      unix.Read,
	write:     unix.Write,
	fsync:     unix.Fsync,
	renameat2: unix.Renameat2,
	unlinkat:  unix.Unlinkat,
	fstat:     unix.Fstat,
	fstatat:   unix.Fstatat,
	readFile:  os.ReadFile,
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

// PinnedObject retains the exact regular file validated by the Store. Readers
// are reopened from this descriptor, not from the storage namespace.
type PinnedObject struct {
	store    *Store
	file     *os.File
	identity unix.Stat_t
	size     int64
	sha256   [sha256.Size]byte
	mu       sync.Mutex
	closed   bool
}

// UseReadOnlyFile calls use with a fresh read-only descriptor whose offset is
// independent from every other reader of this pinned object.
func (object *PinnedObject) UseReadOnlyFile(use func(*os.File) error) (returnErr error) {
	if object == nil || use == nil {
		return ErrValidation
	}
	object.mu.Lock()
	defer object.mu.Unlock()
	if object.closed {
		return ErrClosed
	}
	fd, err := openPinnedReadOnly(int(object.file.Fd()))
	if err != nil {
		return classifyError("reopen pinned object", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return classifyError("stat reopened pinned object", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Dev != object.identity.Dev || stat.Ino != object.identity.Ino || stat.Size != object.identity.Size {
		_ = unix.Close(fd)
		return errors.Join(ErrValidation, errors.New("pinned object identity changed"))
	}
	file := os.NewFile(uintptr(fd), "storage-pinned-object")
	defer func() {
		if closeErr := file.Close(); returnErr == nil && closeErr != nil {
			returnErr = classifyError("close pinned object reader", closeErr)
		}
	}()
	return use(file)
}

// Verify revalidates the retained descriptor without reopening the object by
// pathname. It must run after the last processor read and before publication.
func (object *PinnedObject) Verify(ctx context.Context) error {
	if object == nil || ctx == nil {
		return ErrValidation
	}
	object.mu.Lock()
	defer object.mu.Unlock()
	if object.closed {
		return ErrClosed
	}
	_, err := object.store.verifyPinnedObject(ctx, object.file, Validation{ExpectedSize: object.size, ExpectedSHA256: &object.sha256}, &object.identity)
	return err
}

func (object *PinnedObject) Close() error {
	if object == nil {
		return nil
	}
	object.mu.Lock()
	defer object.mu.Unlock()
	if object.closed {
		return nil
	}
	object.closed = true
	return object.file.Close()
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

type DeleteExpectation struct {
	ExpectedSize int64
}

type QuarantineResult struct {
	Path    string
	Moved   bool
	Missing bool
}

type AttemptTempInfo struct {
	Size       int64
	ModifiedAt time.Time
}

func (store *Store) InspectAttemptTemp(ctx context.Context, key AttemptTempKey) (AttemptTempInfo, error) {
	if _, err := ParseAttemptTempKey(key.String()); err != nil {
		return AttemptTempInfo{}, ErrInvalidKey
	}
	parentFD, name, release, err := store.openParent(ctx, key.String())
	if err != nil {
		return AttemptTempInfo{}, err
	}
	defer release()
	fd, err := store.ops.openat(parentFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return AttemptTempInfo{}, classifyError("open attempt temporary object", err)
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := store.ops.fstat(fd, &stat); err != nil {
		return AttemptTempInfo{}, classifyError("stat attempt temporary object", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return AttemptTempInfo{}, ErrUnexpectedType
	}
	return AttemptTempInfo{Size: stat.Size, ModifiedAt: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)}, nil
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
	tempKey := strings.Join(append(append([]string(nil), directories...), tempName), "/")
	if _, err := ParseAttemptTempKey(tempKey); err != nil {
		cleanupErr := store.cleanupFailedBegin(context.WithoutCancel(ctx), parentFD, -1, "", cleanupParentFD, cleanupDirName, key)
		_ = unix.Close(parentFD)
		if cleanupParentFD >= 0 {
			_ = unix.Close(cleanupParentFD)
		}
		return nil, errors.Join(ErrInvalidKey, cleanupErr)
	}
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

// OpenOriginalPinned opens and validates the exact descriptor retained by the
// returned object. ExpectedSize and ExpectedSHA256 are mandatory so callers
// cannot accidentally expose an unbound canonical original to a processor.
func (store *Store) OpenOriginalPinned(ctx context.Context, key OriginalKey, validation Validation) (*PinnedObject, error) {
	if validation.ExpectedSize < 0 || validation.ExpectedSHA256 == nil || validation.Validate != nil {
		return nil, ErrValidation
	}
	file, err := store.OpenOriginal(ctx, key)
	if err != nil {
		return nil, err
	}
	object, err := store.validatePinnedObject(ctx, file, validation)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return object, nil
}

func (store *Store) validatePinnedObject(ctx context.Context, file *os.File, validation Validation) (*PinnedObject, error) {
	identity, err := store.verifyPinnedObject(ctx, file, validation, nil)
	if err != nil {
		return nil, err
	}
	return &PinnedObject{store: store, file: file, identity: identity, size: validation.ExpectedSize, sha256: *validation.ExpectedSHA256}, nil
}

func (store *Store) verifyPinnedObject(ctx context.Context, file *os.File, validation Validation, expectedIdentity *unix.Stat_t) (unix.Stat_t, error) {
	var before unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &before); err != nil {
		return unix.Stat_t{}, errors.Join(ErrValidation, classifyError("stat pinned object", err))
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return unix.Stat_t{}, ErrUnexpectedType
	}
	if expectedIdentity != nil && (before.Dev != expectedIdentity.Dev || before.Ino != expectedIdentity.Ino || before.Size != expectedIdentity.Size) {
		return unix.Stat_t{}, errors.Join(ErrValidation, errors.New("pinned object identity changed"))
	}
	if before.Size != validation.ExpectedSize {
		return unix.Stat_t{}, ErrValidation
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return unix.Stat_t{}, errors.Join(ErrValidation, classifyError("rewind pinned object", err))
	}
	hash := sha256.New()
	buffer := make([]byte, 128*1024)
	remaining := validation.ExpectedSize
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return unix.Stat_t{}, err
		}
		limit := int64(len(buffer))
		if remaining < limit {
			limit = remaining
		}
		count, readErr := store.ops.read(int(file.Fd()), buffer[:limit])
		if count < 0 || int64(count) > limit {
			return unix.Stat_t{}, errors.Join(ErrValidation, errors.New("invalid pinned object read count"))
		}
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
			remaining -= int64(count)
		}
		if err := ctx.Err(); err != nil {
			return unix.Stat_t{}, err
		}
		if errors.Is(readErr, unix.EINTR) {
			continue
		}
		if readErr != nil {
			return unix.Stat_t{}, errors.Join(ErrValidation, readErr)
		}
		if count == 0 {
			return unix.Stat_t{}, errors.Join(ErrValidation, io.ErrUnexpectedEOF)
		}
	}
	if err := ctx.Err(); err != nil {
		return unix.Stat_t{}, err
	}
	count, readErr := store.ops.read(int(file.Fd()), buffer[:1])
	if err := ctx.Err(); err != nil {
		return unix.Stat_t{}, err
	}
	if count < 0 || count > 1 || count != 0 {
		return unix.Stat_t{}, errors.Join(ErrValidation, errors.New("pinned object exceeds expected size"))
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, unix.EINTR) {
		return unix.Stat_t{}, errors.Join(ErrValidation, readErr)
	}
	if errors.Is(readErr, unix.EINTR) {
		return unix.Stat_t{}, errors.Join(ErrValidation, readErr)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	if digest != *validation.ExpectedSHA256 {
		return unix.Stat_t{}, ErrValidation
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return unix.Stat_t{}, errors.Join(ErrValidation, classifyError("restat pinned object", err))
	}
	if err := ctx.Err(); err != nil {
		return unix.Stat_t{}, err
	}
	if after.Mode&unix.S_IFMT != unix.S_IFREG || after.Dev != before.Dev || after.Ino != before.Ino || after.Size != before.Size {
		return unix.Stat_t{}, errors.Join(ErrValidation, errors.New("pinned object changed during validation"))
	}
	return after, nil
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

func (store *Store) DeleteOriginal(ctx context.Context, key OriginalKey, expectation DeleteExpectation) (DeleteResult, error) {
	if _, err := ParseOriginalKey(key.String()); err != nil {
		return DeleteResult{}, ErrInvalidKey
	}
	return store.deleteObject(ctx, key.String(), expectation)
}

func (store *Store) DeleteRendition(ctx context.Context, key RenditionKey, expectation DeleteExpectation) (DeleteResult, error) {
	if _, err := ParseRenditionKey(key.String()); err != nil {
		return DeleteResult{}, ErrInvalidKey
	}
	return store.deleteObject(ctx, key.String(), expectation)
}

func (store *Store) deleteObject(ctx context.Context, key string, expectation DeleteExpectation) (DeleteResult, error) {
	if expectation.ExpectedSize < 0 {
		return DeleteResult{}, ErrValidation
	}
	parentFD, finalName, release, err := store.openParent(ctx, key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DeleteResult{Missing: true}, nil
		}
		return DeleteResult{}, err
	}
	defer release()
	objectFD, err := store.ops.openat(parentFD, finalName, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
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
		return DeleteResult{}, classifyError("open object for delete", err)
	}
	defer unix.Close(objectFD)
	var pinned unix.Stat_t
	if err := store.ops.fstat(objectFD, &pinned); err != nil {
		return DeleteResult{}, classifyError("stat pinned object for delete", err)
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFREG {
		return DeleteResult{}, ErrUnexpectedType
	}
	if pinned.Size != expectation.ExpectedSize {
		return DeleteResult{}, ErrValidation
	}
	if err := store.inject(ctx, BoundaryDelete, Before, key, 0); err != nil {
		return DeleteResult{}, err
	}
	var current unix.Stat_t
	if err := store.ops.fstatat(parentFD, finalName, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
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
		return DeleteResult{}, classifyError("restat object for delete", err)
	}
	if current.Mode&unix.S_IFMT == unix.S_IFLNK {
		return DeleteResult{}, ErrSymlink
	}
	if current.Mode&unix.S_IFMT != unix.S_IFREG {
		return DeleteResult{}, ErrUnexpectedType
	}
	if current.Dev != pinned.Dev || current.Ino != pinned.Ino || current.Size != pinned.Size {
		return DeleteResult{}, ErrValidation
	}
	if err := ctx.Err(); err != nil {
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
	durableCtx := context.WithoutCancel(ctx)
	if err := store.inject(durableCtx, BoundaryDelete, After, key, 0); err != nil {
		return DeleteResult{}, errors.Join(ErrOutcomeUncertain, err)
	}
	durable, err := store.syncDeleteDirectory(durableCtx, parentFD, key)
	if err != nil {
		if durable {
			return DeleteResult{}, err
		}
		return DeleteResult{}, errors.Join(ErrOutcomeUncertain, ErrDurability, err)
	}
	return DeleteResult{}, nil
}

func (store *Store) QuarantineOriginal(ctx context.Context, source OriginalKey, expectation DeleteExpectation, destination QuarantineKey) (QuarantineResult, error) {
	if _, err := ParseOriginalKey(source.String()); err != nil {
		return QuarantineResult{}, ErrInvalidKey
	}
	return store.quarantineObject(ctx, source.String(), expectation, destination)
}

func (store *Store) QuarantineRendition(ctx context.Context, source RenditionKey, expectation DeleteExpectation, destination QuarantineKey) (QuarantineResult, error) {
	if _, err := ParseRenditionKey(source.String()); err != nil {
		return QuarantineResult{}, ErrInvalidKey
	}
	return store.quarantineObject(ctx, source.String(), expectation, destination)
}

func (store *Store) QuarantineAttemptTemp(ctx context.Context, source AttemptTempKey, expectation DeleteExpectation, destination QuarantineKey) (QuarantineResult, error) {
	if _, err := ParseAttemptTempKey(source.String()); err != nil {
		return QuarantineResult{}, ErrInvalidKey
	}
	return store.quarantineObject(ctx, source.String(), expectation, destination)
}

func (store *Store) quarantineObject(ctx context.Context, source string, expectation DeleteExpectation, destination QuarantineKey) (QuarantineResult, error) {
	destinationPath := destination.String()
	if expectation.ExpectedSize < 0 {
		return QuarantineResult{}, ErrValidation
	}
	if _, err := ParseQuarantineKey(destinationPath); err != nil {
		return QuarantineResult{}, ErrInvalidKey
	}
	sourceFD, sourceName, release, err := store.openParent(ctx, source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return QuarantineResult{Path: destinationPath, Missing: true}, nil
		}
		return QuarantineResult{}, err
	}
	defer release()
	objectFD, err := store.ops.openat(sourceFD, sourceName, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return store.recoverQuarantineMove(ctx, sourceFD, expectation, destination)
		}
		return QuarantineResult{}, classifyError("open quarantine source", err)
	}
	defer unix.Close(objectFD)
	var pinned unix.Stat_t
	if err := store.ops.fstat(objectFD, &pinned); err != nil {
		return QuarantineResult{}, classifyError("stat quarantine source", err)
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFREG {
		return QuarantineResult{}, ErrUnexpectedType
	}
	if pinned.Size != expectation.ExpectedSize {
		return QuarantineResult{}, ErrValidation
	}
	quarantineFD, err := store.openQuarantineDirectory(ctx, destinationPath)
	if err != nil {
		return QuarantineResult{}, err
	}
	defer unix.Close(quarantineFD)
	var sourceDirectory, quarantineDirectory unix.Stat_t
	if err := store.ops.fstat(sourceFD, &sourceDirectory); err != nil {
		return QuarantineResult{}, classifyError("stat quarantine source directory", err)
	}
	if err := store.ops.fstat(quarantineFD, &quarantineDirectory); err != nil {
		return QuarantineResult{}, classifyError("stat quarantine destination directory", err)
	}
	if sourceDirectory.Dev != quarantineDirectory.Dev || pinned.Dev != quarantineDirectory.Dev {
		return QuarantineResult{}, errors.Join(ErrValidation, errors.New("quarantine requires one filesystem"))
	}
	if err := store.inject(ctx, BoundaryQuarantineRename, Before, destinationPath, 0); err != nil {
		return QuarantineResult{}, err
	}
	var current unix.Stat_t
	if err := store.ops.fstatat(sourceFD, sourceName, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return store.recoverQuarantineMove(ctx, sourceFD, expectation, destination)
		}
		return QuarantineResult{}, classifyError("restat quarantine source", err)
	}
	if current.Mode&unix.S_IFMT != unix.S_IFREG || current.Dev != pinned.Dev || current.Ino != pinned.Ino || current.Size != pinned.Size {
		return QuarantineResult{}, ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return QuarantineResult{}, err
	}
	_, destinationName := splitKey(destinationPath)
	if err := store.ops.renameat2(sourceFD, sourceName, quarantineFD, destinationName, unix.RENAME_NOREPLACE); err != nil {
		return QuarantineResult{}, classifyError("rename object to quarantine", err)
	}
	durableCtx := context.WithoutCancel(ctx)
	var postErrors []error
	uncertain := false
	if err := store.inject(durableCtx, BoundaryQuarantineRename, After, destinationPath, 0); err != nil {
		postErrors = append(postErrors, err)
		uncertain = true
	}
	var moved unix.Stat_t
	if err := store.ops.fstatat(quarantineFD, destinationName, &moved, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		postErrors = append(postErrors, classifyError("validate quarantined object", err))
		uncertain = true
	} else if moved.Mode&unix.S_IFMT != unix.S_IFREG || moved.Dev != pinned.Dev || moved.Ino != pinned.Ino || moved.Size != pinned.Size {
		postErrors = append(postErrors, ErrValidation)
		uncertain = true
	}
	if syncUncertain, syncErr := store.syncQuarantineMove(durableCtx, sourceFD, quarantineFD, destinationPath); syncErr != nil {
		postErrors = append(postErrors, syncErr)
		uncertain = uncertain || syncUncertain
	}
	result := QuarantineResult{Path: destinationPath, Moved: true}
	if len(postErrors) != 0 {
		cause := errors.Join(postErrors...)
		if uncertain {
			cause = errors.Join(ErrOutcomeUncertain, cause)
		}
		return result, &QuarantineError{Moved: true, Uncertain: uncertain, operation: "durability", cause: cause}
	}
	return result, nil
}

func (store *Store) recoverQuarantineMove(ctx context.Context, sourceFD int, expectation DeleteExpectation, destination QuarantineKey) (QuarantineResult, error) {
	destinationPath := destination.String()
	quarantineFD, err := store.openQuarantineDirectory(ctx, destinationPath)
	if err != nil {
		return QuarantineResult{}, err
	}
	defer unix.Close(quarantineFD)
	_, destinationName := splitKey(destinationPath)
	moved := false
	var stat unix.Stat_t
	if err := store.ops.fstatat(quarantineFD, destinationName, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size != expectation.ExpectedSize {
			return QuarantineResult{}, ErrValidation
		}
		moved = true
	} else if !errors.Is(err, unix.ENOENT) {
		return QuarantineResult{}, classifyError("inspect quarantine retry destination", err)
	}
	durableCtx := context.WithoutCancel(ctx)
	uncertain, syncErr := store.syncQuarantineMove(durableCtx, sourceFD, quarantineFD, destinationPath)
	result := QuarantineResult{Path: destinationPath, Moved: moved, Missing: !moved}
	if syncErr == nil {
		return result, nil
	}
	cause := syncErr
	if uncertain {
		cause = errors.Join(ErrOutcomeUncertain, cause)
	}
	return result, &QuarantineError{Moved: moved, Uncertain: uncertain, operation: "retry durability", cause: cause}
}

func (store *Store) syncQuarantineMove(ctx context.Context, sourceFD, quarantineFD int, destinationPath string) (bool, error) {
	uncertain := false
	var syncErrors []error
	if durable, err := store.syncBoundary(ctx, sourceFD, BoundaryQuarantineSourceDirectorySync, destinationPath, 0, "sync quarantine source directory"); err != nil {
		if !durable {
			syncErrors = append(syncErrors, errors.Join(ErrDurability, err))
			uncertain = true
		} else {
			syncErrors = append(syncErrors, err)
		}
	}
	if durable, err := store.syncBoundary(ctx, quarantineFD, BoundaryQuarantineDestinationDirectorySync, destinationPath, 0, "sync quarantine destination directory"); err != nil {
		if !durable {
			syncErrors = append(syncErrors, errors.Join(ErrDurability, err))
			uncertain = true
		} else {
			syncErrors = append(syncErrors, err)
		}
	}
	return uncertain, errors.Join(syncErrors...)
}

func (store *Store) openQuarantineDirectory(ctx context.Context, key string) (int, error) {
	const name = ".quarantine"
	fd, err := store.ops.openat(store.rootFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	created := false
	if errors.Is(err, unix.ENOENT) {
		if err = store.inject(ctx, BoundaryQuarantineDirectoryCreate, Before, key, 0); err == nil {
			err = store.ops.mkdirat(store.rootFD, name, 0o700)
		}
		if errors.Is(err, unix.EEXIST) {
			err = nil
		} else if err == nil {
			created = true
		}
		if err == nil {
			fd, err = store.ops.openat(store.rootFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err == nil && created {
			err = store.inject(context.WithoutCancel(ctx), BoundaryQuarantineDirectoryCreate, After, key, 0)
		}
	}
	if err != nil {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		return -1, classifyError("open quarantine directory", err)
	}
	var directoryStat unix.Stat_t
	if err := store.ops.fstat(fd, &directoryStat); err != nil {
		_ = unix.Close(fd)
		return -1, classifyError("stat quarantine directory", err)
	}
	if directoryStat.Mode&unix.S_IFMT != unix.S_IFDIR || directoryStat.Mode&0o077 != 0 {
		_ = unix.Close(fd)
		return -1, ErrUnexpectedType
	}
	durableCtx := ctx
	if created {
		durableCtx = context.WithoutCancel(ctx)
	}
	if durable, syncErr := store.syncBoundary(durableCtx, fd, BoundaryQuarantineDirectorySync, key, 0, "sync quarantine directory"); syncErr != nil {
		_ = unix.Close(fd)
		if durable {
			return -1, syncErr
		}
		return -1, errors.Join(ErrDurability, syncErr)
	}
	if durable, syncErr := store.syncBoundary(durableCtx, store.rootFD, BoundaryQuarantineDirectorySync, key, 1, "sync quarantine directory parent"); syncErr != nil {
		_ = unix.Close(fd)
		if durable {
			return -1, syncErr
		}
		return -1, errors.Join(ErrDurability, syncErr)
	}
	return fd, nil
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
	case errors.Is(err, unix.ENOSPC):
		return errors.Join(ErrNoSpace, fmt.Errorf("%s failed", operation))
	case errors.Is(err, unix.EDQUOT):
		return errors.Join(ErrQuota, fmt.Errorf("%s failed", operation))
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return errors.Join(ErrPermission, fmt.Errorf("%s failed", operation))
	case errors.Is(err, unix.EROFS):
		return errors.Join(ErrReadOnly, fmt.Errorf("%s failed", operation))
	case errors.Is(err, unix.ENOENT):
		return errors.Join(os.ErrNotExist, fmt.Errorf("%s failed", operation))
	default:
		return fmt.Errorf("%s failed: %w", operation, err)
	}
}
