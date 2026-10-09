//go:build linux

package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// ObservationType is the no-follow type observed at a storage-root-relative
// namespace entry. Values intentionally match the reconciliation evidence
// vocabulary.
type ObservationType string

const (
	ObservationRegular   ObservationType = "regular"
	ObservationDirectory ObservationType = "directory"
	ObservationSymlink   ObservationType = "symlink"
	ObservationFIFO      ObservationType = "fifo"
	ObservationSocket    ObservationType = "socket"
	ObservationDevice    ObservationType = "device"
	ObservationOther     ObservationType = "other"
)

// Observation is an immutable point-in-time description produced beneath the
// Store's pinned root. A regular observation is Stable only when the opened
// descriptor and the namespace leaf retain identical identity and metadata
// across hashing. Unstable observations deliberately omit SHA256.
type Observation struct {
	RelativeKey string
	Type        ObservationType
	Size        *int64
	SHA256      *[sha256.Size]byte
	ModifiedAt  *time.Time
	ChangedAt   *time.Time
	ObservedAt  time.Time
	Stable      bool
	identity    unix.Stat_t
}

// Scan walks the pinned storage root descriptor-relatively and without
// following symlinks. It performs no filesystem mutation. Results are sorted
// by raw relative-key bytes so creation order never affects reconciliation.
func (store *Store) Scan(ctx context.Context) ([]Observation, error) {
	if ctx == nil {
		return nil, ErrValidation
	}
	store.mu.RLock()
	if store.closed {
		store.mu.RUnlock()
		return nil, ErrClosed
	}
	rootFD, err := store.ops.openat(store.rootFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	store.mu.RUnlock()
	if err != nil {
		return nil, classifyError("duplicate storage root for scan", err)
	}
	unix.CloseOnExec(rootFD)
	defer unix.Close(rootFD)
	var rootBefore unix.Stat_t
	if err := store.ops.fstat(rootFD, &rootBefore); err != nil {
		return nil, classifyError("stat storage scan root", err)
	}

	observations := make([]Observation, 0)
	if err := store.scanDirectory(ctx, rootFD, "", &observations); err != nil {
		return nil, err
	}
	sort.Slice(observations, func(i, j int) bool {
		return observations[i].RelativeKey < observations[j].RelativeKey
	})
	// Reverse lexical order validates leaves before their recorded ancestor
	// directories. A namespace change triggered while a leaf is checked must
	// therefore change a directory identity that has not yet been accepted.
	for index := len(observations) - 1; index >= 0; index-- {
		if err := store.revalidateScanObservation(ctx, rootFD, &observations[index]); err != nil {
			if errors.Is(err, ErrUnstableScan) {
				markObservationsUnstable(observations)
				return observations, ErrUnstableScan
			}
			return nil, err
		}
	}
	var rootAfter unix.Stat_t
	if err := store.ops.fstat(rootFD, &rootAfter); err != nil {
		return nil, classifyError("revalidate storage scan root", err)
	}
	if !sameScanIdentity(rootBefore, rootAfter) {
		markObservationsUnstable(observations)
		return observations, ErrUnstableScan
	}
	for _, observation := range observations {
		if !observation.Stable {
			return observations, ErrUnstableScan
		}
	}
	return observations, nil
}

func (store *Store) revalidateScanObservation(ctx context.Context, rootFD int, observation *Observation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parts := strings.Split(observation.RelativeKey, "/")
	parentFD, err := store.ops.openat(rootFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return classifyError("open storage scan root for revalidation", err)
	}
	defer func() { _ = unix.Close(parentFD) }()
	for _, component := range parts[:len(parts)-1] {
		nextFD, openErr := store.ops.openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			if errors.Is(openErr, unix.ENOENT) || errors.Is(openErr, unix.ENOTDIR) || errors.Is(openErr, unix.ELOOP) {
				return ErrUnstableScan
			}
			return classifyError("open storage scan revalidation directory", openErr)
		}
		_ = unix.Close(parentFD)
		parentFD = nextFD
	}
	var current unix.Stat_t
	if err := store.ops.fstatat(parentFD, parts[len(parts)-1], &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ErrUnstableScan
		}
		return classifyError("revalidate storage scan entry", err)
	}
	if !sameScanIdentity(observation.identity, current) {
		return ErrUnstableScan
	}
	return nil
}

func (store *Store) scanDirectory(ctx context.Context, directoryFD int, prefix string, observations *[]Observation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	readFD, err := store.ops.openat(directoryFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return classifyError("duplicate storage scan directory", err)
	}
	unix.CloseOnExec(readFD)
	directory := os.NewFile(uintptr(readFD), "storage-scan-directory")
	entries := make([]os.DirEntry, 0)
	for {
		if err := ctx.Err(); err != nil {
			_ = directory.Close()
			return err
		}
		batch, readErr := directory.ReadDir(256)
		entries = append(entries, batch...)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = directory.Close()
			return classifyError("enumerate storage directory", readErr)
		}
	}
	closeErr := directory.Close()
	if closeErr != nil {
		return classifyError("close storage scan directory", closeErr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") || !utf8.ValidString(name) {
			return errors.Join(ErrValidation, errors.New("storage contains an unrepresentable directory entry"))
		}
		relativeKey := name
		if prefix != "" {
			relativeKey = prefix + "/" + name
		}
		var namespaceStat unix.Stat_t
		if err := store.ops.fstatat(directoryFD, name, &namespaceStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				*observations = append(*observations, unstableObservation(relativeKey, time.Now()))
				continue
			}
			return classifyError("stat storage scan entry", err)
		}
		entryType := observationType(namespaceStat.Mode)
		if entryType == ObservationDirectory {
			*observations = append(*observations, metadataObservation(relativeKey, entryType, namespaceStat, true, time.Now()))
			directoryObservation := len(*observations) - 1
			subtreeStart := len(*observations)
			childFD, openErr := store.ops.openat(directoryFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if openErr != nil {
				if errors.Is(openErr, unix.ENOENT) || errors.Is(openErr, unix.ENOTDIR) || errors.Is(openErr, unix.ELOOP) {
					(*observations)[len(*observations)-1].Stable = false
					continue
				}
				return classifyError("open storage scan directory", openErr)
			}
			scanErr := store.scanDirectory(ctx, childFD, relativeKey, observations)
			if scanErr != nil {
				_ = unix.Close(childFD)
				return scanErr
			}
			var childAfter, leafAfter unix.Stat_t
			if statErr := store.ops.fstat(childFD, &childAfter); statErr != nil {
				_ = unix.Close(childFD)
				return classifyError("revalidate storage scan directory", statErr)
			}
			if statErr := store.ops.fstatat(directoryFD, name, &leafAfter, unix.AT_SYMLINK_NOFOLLOW); statErr != nil {
				if errors.Is(statErr, unix.ENOENT) {
					(*observations)[directoryObservation].Stable = false
					markObservationsUnstable((*observations)[subtreeStart:])
				} else {
					_ = unix.Close(childFD)
					return classifyError("revalidate storage scan directory namespace", statErr)
				}
			} else if !sameScanIdentity(namespaceStat, childAfter) || !sameScanIdentity(childAfter, leafAfter) {
				(*observations)[directoryObservation].Stable = false
				markObservationsUnstable((*observations)[subtreeStart:])
			}
			closeErr := unix.Close(childFD)
			if closeErr != nil {
				return classifyError("close storage child directory", closeErr)
			}
			continue
		}
		if entryType != ObservationRegular {
			*observations = append(*observations, metadataObservation(relativeKey, entryType, namespaceStat, true, time.Now()))
			continue
		}
		observation, inspectErr := store.inspectScanRegular(ctx, directoryFD, name, relativeKey, namespaceStat)
		if inspectErr != nil {
			return inspectErr
		}
		*observations = append(*observations, observation)
	}
	return nil
}

func markObservationsUnstable(observations []Observation) {
	for index := range observations {
		observations[index].Stable = false
		observations[index].SHA256 = nil
	}
}

func (store *Store) inspectScanRegular(ctx context.Context, parentFD int, name, relativeKey string, first unix.Stat_t) (Observation, error) {
	observedAt := time.Now()
	fd, err := store.ops.openat(parentFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) {
			return unstableObservation(relativeKey, observedAt), nil
		}
		return Observation{}, classifyError("open storage scan object", err)
	}
	defer unix.Close(fd)
	var before unix.Stat_t
	if err := store.ops.fstat(fd, &before); err != nil {
		return Observation{}, classifyError("stat storage scan object", err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG || !sameScanIdentity(first, before) || before.Nlink != 1 {
		return metadataObservation(relativeKey, ObservationRegular, before, false, observedAt), nil
	}
	hash := sha256.New()
	buffer := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return Observation{}, err
		}
		count, readErr := store.ops.read(fd, buffer)
		if count < 0 || count > len(buffer) {
			return Observation{}, errors.Join(ErrValidation, errors.New("invalid storage scan read count"))
		}
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(readErr, unix.EINTR) {
			continue
		}
		if readErr != nil {
			return Observation{}, classifyError("hash storage scan object", readErr)
		}
		if count == 0 {
			break
		}
	}
	var after, leaf unix.Stat_t
	if err := store.ops.fstat(fd, &after); err != nil {
		return Observation{}, classifyError("restat storage scan object", err)
	}
	if err := store.ops.fstatat(parentFD, name, &leaf, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return metadataObservation(relativeKey, ObservationRegular, after, false, observedAt), nil
		}
		return Observation{}, classifyError("revalidate storage scan namespace", err)
	}
	stable := sameScanIdentity(before, after) && sameScanIdentity(after, leaf) && after.Nlink == 1
	observation := metadataObservation(relativeKey, ObservationRegular, after, stable, observedAt)
	if stable {
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		observation.SHA256 = &digest
	}
	return observation, nil
}

func metadataObservation(relativeKey string, objectType ObservationType, stat unix.Stat_t, stable bool, observedAt time.Time) Observation {
	size := stat.Size
	modifiedAt := time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).UTC()
	changedAt := time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec).UTC()
	return Observation{
		RelativeKey: relativeKey, Type: objectType, Size: &size,
		ModifiedAt: &modifiedAt, ChangedAt: &changedAt,
		ObservedAt: observedAt.UTC(), Stable: stable, identity: stat,
	}
}

func unstableObservation(relativeKey string, observedAt time.Time) Observation {
	return Observation{RelativeKey: relativeKey, Type: ObservationOther, ObservedAt: observedAt.UTC(), Stable: false}
}

func sameScanIdentity(left, right unix.Stat_t) bool {
	return left.Mode == right.Mode && left.Dev == right.Dev && left.Ino == right.Ino && left.Size == right.Size &&
		left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func observationType(mode uint32) ObservationType {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return ObservationRegular
	case unix.S_IFDIR:
		return ObservationDirectory
	case unix.S_IFLNK:
		return ObservationSymlink
	case unix.S_IFIFO:
		return ObservationFIFO
	case unix.S_IFSOCK:
		return ObservationSocket
	case unix.S_IFCHR, unix.S_IFBLK:
		return ObservationDevice
	default:
		return ObservationOther
	}
}

func (observation Observation) String() string {
	return fmt.Sprintf("%s (%s)", observation.RelativeKey, observation.Type)
}
