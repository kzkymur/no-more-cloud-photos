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
	write     func(int, []byte) (int, error)
	fsync     func(int) error
	renameat2 func(int, string, int, string, uint) error
	unlinkat  func(int, string, int) error
	fstatat   func(int, string, *unix.Stat_t, int) error
}

var linuxOperations = systemOperations{
	openat:    unix.Openat,
	mkdirat:   unix.Mkdirat,
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

func (store *Store) BeginRendition(ctx context.Context, key RenditionKey, attempt AttemptID) (*Temp, error) {
	if _, err := ParseRenditionKey(key.String()); err != nil || !attempt.uuidV4.valid() {
		return nil, ErrInvalidKey
	}
	return store.begin(ctx, key.String(), attempt)
}

type Temp struct {
	store       *Store
	ctx         context.Context
	parentFD    int
	fileFD      int
	key         string
	tempName    string
	finalName   string
	finished    bool
	published   bool
	writeFailed error
	mu          sync.Mutex
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

type DeleteResult struct {
	Missing bool
}

func (store *Store) begin(ctx context.Context, key string, attempt AttemptID) (*Temp, error) {
	store.mu.RLock()
	if store.closed {
		store.mu.RUnlock()
		return nil, ErrClosed
	}
	directories, finalName := splitKey(key)
	parentFD, err := unix.Dup(store.rootFD)
	store.mu.RUnlock()
	if err != nil {
		return nil, classifyError("duplicate root", err)
	}
	for depth, directory := range directories {
		nextFD, openErr := store.ops.openat(parentFD, directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil && errors.Is(openErr, unix.ENOENT) {
			if err = store.inject(ctx, BoundaryDirectoryCreate, Before, key, depth); err == nil {
				err = store.ops.mkdirat(parentFD, directory, 0o700)
			}
			created := err == nil
			if errors.Is(err, unix.EEXIST) {
				// Another trusted publisher won the same directory creation race.
				// Open and sync both sides of that new entry ourselves before use.
				err = nil
			}
			if err == nil && created {
				err = store.inject(ctx, BoundaryDirectoryCreate, After, key, depth)
			}
			if err == nil {
				nextFD, err = store.ops.openat(parentFD, directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		} else {
			err = openErr
		}
		// Sync every traversed component. This is intentionally redundant for
		// old directories so a retry repairs a prior crash/failure after mkdir.
		if err == nil {
			if syncErr := store.syncBoundary(ctx, nextFD, BoundaryNewDirectorySync, key, depth); syncErr != nil {
				err = errors.Join(ErrDurability, classifyError("sync key directory", syncErr))
			}
		}
		if err == nil {
			if syncErr := store.syncBoundary(ctx, parentFD, BoundaryParentDirectorySync, key, depth); syncErr != nil {
				err = errors.Join(ErrDurability, classifyError("sync key directory parent", syncErr))
			}
		}
		if err != nil {
			_ = unix.Close(parentFD)
			if nextFD >= 0 {
				_ = unix.Close(nextFD)
			}
			if errors.Is(err, ErrDurability) {
				return nil, err
			}
			return nil, classifyError("open key directory", err)
		}
		_ = unix.Close(parentFD)
		parentFD = nextFD
	}
	tempName := "." + finalName + "." + attempt.String() + ".tmp"
	if err := store.inject(ctx, BoundaryTempCreate, Before, key, len(directories)); err != nil {
		_ = unix.Close(parentFD)
		return nil, err
	}
	fileFD, err := store.ops.openat(parentFD, tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err == nil {
		err = store.inject(ctx, BoundaryTempCreate, After, key, len(directories))
	}
	if err != nil {
		if fileFD >= 0 {
			_ = unix.Close(fileFD)
		}
		_ = unix.Close(parentFD)
		return nil, classifyError("create temporary object", err)
	}
	return &Temp{store: store, ctx: ctx, parentFD: parentFD, fileFD: fileFD, key: key, tempName: tempName, finalName: finalName}, nil
}

func (temp *Temp) Write(value []byte) (written int, returnErr error) {
	temp.mu.Lock()
	defer temp.mu.Unlock()
	defer func() {
		if returnErr != nil {
			temp.writeFailed = returnErr
		}
	}()
	if temp.finished {
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
// descriptor and closes it before returning. Publish cannot run concurrently,
// so no writable descriptor supplied by this API survives publication.
func (temp *Temp) UseWritableFile(use func(*os.File) error) (returnErr error) {
	temp.mu.Lock()
	defer temp.mu.Unlock()
	if temp.finished {
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

func (temp *Temp) Publish(ctx context.Context, validation Validation) (ObjectInfo, error) {
	temp.mu.Lock()
	defer temp.mu.Unlock()
	if temp.finished {
		return ObjectInfo{}, ErrClosed
	}
	if temp.writeFailed != nil {
		return ObjectInfo{}, errors.Join(ErrValidation, temp.writeFailed)
	}
	if err := temp.store.syncBoundary(ctx, temp.fileFD, BoundaryFileSync, temp.key, 0); err != nil {
		return ObjectInfo{}, &PublishError{operation: "file sync", cause: errors.Join(ErrDurability, classifyError("sync temporary object", err))}
	}
	info, err := temp.inspect(ctx, validation)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := temp.store.inject(ctx, BoundaryRename, Before, temp.key, 0); err != nil {
		return ObjectInfo{}, err
	}
	err = temp.store.ops.renameat2(temp.parentFD, temp.tempName, temp.parentFD, temp.finalName, unix.RENAME_NOREPLACE)
	if err != nil {
		return ObjectInfo{}, classifyError("publish object", err)
	}
	temp.published = true
	if err := temp.store.inject(ctx, BoundaryRename, After, temp.key, 0); err != nil {
		temp.finish()
		return ObjectInfo{}, &PublishError{Published: true, Uncertain: true, operation: "after rename", cause: errors.Join(ErrOutcomeUncertain, err)}
	}
	if err := temp.store.inject(ctx, BoundaryFinalDirectorySync, Before, temp.key, 0); err != nil {
		temp.finish()
		return ObjectInfo{}, &PublishError{Published: true, Uncertain: true, operation: "final directory sync", cause: errors.Join(ErrOutcomeUncertain, ErrDurability, err)}
	}
	if err := temp.store.ops.fsync(temp.parentFD); err != nil {
		temp.finish()
		return ObjectInfo{}, &PublishError{Published: true, Uncertain: true, operation: "final directory sync", cause: errors.Join(ErrOutcomeUncertain, ErrDurability, err)}
	}
	if err := temp.store.inject(ctx, BoundaryFinalDirectorySync, After, temp.key, 0); err != nil {
		temp.finish()
		return ObjectInfo{}, &PublishError{Published: true, operation: "after final directory sync", cause: err}
	}
	temp.finish()
	return info, nil
}

func (temp *Temp) inspect(ctx context.Context, validation Validation) (ObjectInfo, error) {
	if err := temp.store.inject(ctx, BoundaryValidation, Before, temp.key, 0); err != nil {
		return ObjectInfo{}, err
	}
	readFD, err := temp.store.ops.openat(temp.parentFD, temp.tempName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ObjectInfo{}, classifyError("open object for validation", err)
	}
	file := os.NewFile(uintptr(readFD), "storage-validation")
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return ObjectInfo{}, errors.Join(ErrValidation, err)
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
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return ObjectInfo{}, errors.Join(ErrValidation, err)
		}
		if err := validation.Validate(ctx, file); err != nil {
			return ObjectInfo{}, errors.Join(ErrValidation, err)
		}
	}
	if err := temp.store.inject(ctx, BoundaryValidation, After, temp.key, 0); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Size: size, SHA256: digest}, nil
}

func (temp *Temp) Abort(ctx context.Context) error {
	temp.mu.Lock()
	defer temp.mu.Unlock()
	if temp.finished {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := temp.store.ops.unlinkat(temp.parentFD, temp.tempName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return classifyError("abort temporary object", err)
	}
	// Once the namespace was (or may already have been) mutated, cancellation
	// cannot safely skip the directory sync or descriptor cleanup.
	if err := temp.store.syncBoundary(context.WithoutCancel(ctx), temp.parentFD, BoundaryDeleteDirectorySync, temp.key, 0); err != nil {
		temp.finish()
		return errors.Join(ErrOutcomeUncertain, ErrDurability, err)
	}
	temp.finish()
	return nil
}

func (temp *Temp) finish() {
	if temp.finished {
		return
	}
	temp.finished = true
	_ = unix.Close(temp.fileFD)
	_ = unix.Close(temp.parentFD)
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

func (store *Store) inject(ctx context.Context, boundary Boundary, phase Phase, key string, depth int) error {
	return Inject(ctx, store.faults, FaultEvent{Boundary: boundary, Phase: phase, Key: key, Depth: depth})
}

func (store *Store) syncBoundary(ctx context.Context, fd int, boundary Boundary, key string, depth int) error {
	if err := store.inject(ctx, boundary, Before, key, depth); err != nil {
		return err
	}
	if err := store.ops.fsync(fd); err != nil {
		return err
	}
	return store.inject(ctx, boundary, After, key, depth)
}

func (store *Store) syncDeleteDirectory(ctx context.Context, fd int, key string) (bool, error) {
	if err := store.inject(ctx, BoundaryDeleteDirectorySync, Before, key, 0); err != nil {
		return false, err
	}
	if err := store.ops.fsync(fd); err != nil {
		return false, err
	}
	if err := store.inject(ctx, BoundaryDeleteDirectorySync, After, key, 0); err != nil {
		return true, err
	}
	return true, nil
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
