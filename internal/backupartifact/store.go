// Package backupartifact publishes immutable, checksummed backup artifacts beneath
// an existing private directory. All filesystem operations use pinned descriptors.
package backupartifact

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type Boundary string

const (
	DumpSync     Boundary = "dump-sync"
	ManifestSync Boundary = "manifest-sync"
	StageSync    Boundary = "stage-sync"
	Rename       Boundary = "rename"
	RootSync     Boundary = "root-sync"
	DeleteSync   Boundary = "delete-sync"
)

const DeleteDirSync Boundary = "delete-dir-sync"

// Options supplies a fault injection hook called before each durability boundary.
var ErrLocked = errors.New("backup root has another active operation")

type Options struct {
	// Lock coordinates production operations even if a DB lock session is lost.
	Lock  bool
	Fault func(Boundary) error
	// OwnerUID permits root to verify or delete artifacts belonging to a fixed
	// backup UID. Non-root overrides are rejected. Publication always requires
	// the expected owner to equal the effective UID; it never creates foreign-owned artifacts.
	OwnerUID *uint32
	ReadOnly bool
}
type Metadata struct {
	ID             string            `json:"id"`
	SnapshotCutoff time.Time         `json:"snapshot_cutoff"`
	Config         json.RawMessage   `json:"config"`
	ToolVersions   map[string]string `json:"tool_versions"`
	ServerVersion  string            `json:"server_version"`
}
type Manifest struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Metadata
	DumpSHA256 string `json:"dump_sha256"`
	DumpSize   int64  `json:"dump_size"`
}

// PublicationError means the rename succeeded but its durability is uncertain.
// The published directory is preserved and must be reconciled, never retried blindly.
type PublicationError struct {
	ID  string
	Err error
}

func (e *PublicationError) Error() string {
	return fmt.Sprintf("backup %s published, durability uncertain: %v", e.ID, e.Err)
}
func (e *PublicationError) Unwrap() error { return e.Err }

type Store struct {
	mu       sync.Mutex
	root     *os.File
	opts     Options
	ownerUID uint32
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Open requires an existing absolute root with no symlink path components. The
// root must be owned by the effective UID (or root-authorized OwnerUID) and
// inaccessible to group and others.
func Open(path string, opts Options) (*Store, error) {
	ownerUID := uint32(os.Geteuid())
	if opts.OwnerUID != nil {
		if os.Geteuid() != 0 {
			return nil, errors.New("owner override requires root access")
		}
		ownerUID = *opts.OwnerUID
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("backup root must be a clean absolute path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
	}
	root := os.NewFile(uintptr(fd), path)
	if err = privateDir(root, ownerUID); err != nil {
		root.Close()
		return nil, err
	}
	if opts.Lock {
		mode := unix.LOCK_EX | unix.LOCK_NB
		if opts.ReadOnly {
			mode = unix.LOCK_SH | unix.LOCK_NB
		}
		if err = unix.Flock(int(root.Fd()), mode); err != nil {
			root.Close()
			if errors.Is(err, unix.EWOULDBLOCK) {
				return nil, ErrLocked
			}
			return nil, err
		}
	}
	return &Store{root: root, opts: opts, ownerUID: ownerUID}, nil
}
func (s *Store) Close() error { s.mu.Lock(); defer s.mu.Unlock(); return s.root.Close() }
func privateDir(f *os.File, ownerUID uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != ownerUID || st.Mode&0077 != 0 {
		return errors.New("directory must be private and owned by expected UID")
	}
	return nil
}
func (s *Store) boundary(b Boundary, fn func() error) error {
	if s.opts.Fault != nil {
		if err := s.opts.Fault(b); err != nil {
			return err
		}
	}
	return fn()
}

// NewID returns a cryptographically random UUIDv4 for persistence before dumping.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func validateMetadata(m Metadata) error {
	if !uuidPattern.MatchString(m.ID) || m.SnapshotCutoff.IsZero() || len(m.Config) == 0 || !json.Valid(m.Config) || len(m.ToolVersions) == 0 || m.ServerVersion == "" {
		return errors.New("incomplete snapshot metadata")
	}
	for k, v := range m.ToolVersions {
		if k == "" || v == "" {
			return errors.New("empty tool version")
		}
	}
	return nil
}
func openDir(parent *os.File, name string, ownerUID uint32) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	if err = privateDir(f, ownerUID); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func createFile(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func openFile(dir *os.File, name string, ownerUID uint32) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	if err = checkFile(f, ownerUID); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func checkFile(f *os.File, ownerUID uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != ownerUID || st.Mode&0077 != 0 {
		return errors.New("artifact file must be private, regular, singly linked and owned by expected UID")
	}
	return nil
}
func sameEntry(parent *os.File, name string, f *os.File) error {
	var a, b unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &a); err != nil {
		return err
	}
	if err := unix.Fstatat(int(parent.Fd()), name, &b, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if a.Dev != b.Dev || a.Ino != b.Ino {
		return errors.New("artifact path changed during operation")
	}
	return nil
}
func digest(f *os.File) (string, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// Publish calls write to produce a custom-format dump, then validate to run the
// real pg_restore --list against a read-only descriptor for that pinned file.
// Callbacks must not call Store methods, close or
// retain the file. A successful return means all durability barriers completed.
func (s *Store) Publish(ctx context.Context, meta Metadata, write, validate func(context.Context, *os.File) error) (Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m Manifest
	if s.opts.ReadOnly {
		return m, errors.New("store is read-only")
	}
	if s.ownerUID != uint32(os.Geteuid()) {
		return m, errors.New("publication requires effective UID to match backup owner")
	}
	if err := privateDir(s.root, s.ownerUID); err != nil {
		return m, err
	}
	if err := validateMetadata(meta); err != nil {
		return m, err
	}
	if write == nil || validate == nil {
		return m, errors.New("dump writer and pg_restore validator are required")
	}
	if err := ctx.Err(); err != nil {
		return m, err
	}
	id := meta.ID
	var err error
	stage := ".tmp-" + id
	if err = unix.Mkdirat(int(s.root.Fd()), stage, 0700); err != nil {
		return m, err
	}
	dir, err := openDir(s.root, stage, s.ownerUID)
	if err != nil {
		return m, err
	}
	defer dir.Close()
	published := false
	defer func() {
		if !published { // Only fixed filenames in our pinned staging directory.
			unix.Unlinkat(int(dir.Fd()), "dump", 0)
			unix.Unlinkat(int(dir.Fd()), "manifest.json", 0)
			if sameEntry(s.root, stage, dir) == nil {
				unix.Unlinkat(int(s.root.Fd()), stage, unix.AT_REMOVEDIR)
			}
		}
	}()
	dump, err := createFile(dir, "dump")
	if err != nil {
		return m, err
	}
	defer dump.Close()
	if err = write(ctx, dump); err != nil {
		return m, err
	}
	if err = checkFile(dump, s.ownerUID); err != nil {
		return m, err
	}
	if err = s.boundary(DumpSync, dump.Sync); err != nil {
		return m, err
	}
	validationFile, err := openFile(dir, "dump", s.ownerUID)
	if err != nil {
		return m, err
	}
	defer validationFile.Close()
	if err = sameEntry(dir, "dump", dump); err != nil {
		return m, err
	}
	if err = validate(ctx, validationFile); err != nil {
		return m, err
	}
	hash, size, err := digest(dump)
	if err != nil {
		return m, err
	}
	if size == 0 {
		return m, errors.New("empty dump")
	}
	m = Manifest{Version: 1, CreatedAt: time.Now().UTC(), Metadata: meta, DumpSHA256: hash, DumpSize: size}
	mf, err := createFile(dir, "manifest.json")
	if err != nil {
		return Manifest{}, err
	}
	defer mf.Close()
	data, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, err
	}
	if len(data) > 1024*1024 {
		return Manifest{}, errors.New("manifest too large")
	}
	if err = uniqueJSON(data); err != nil {
		return Manifest{}, err
	}
	if _, err = mf.Write(data); err != nil {
		return Manifest{}, err
	}
	if err = s.boundary(ManifestSync, mf.Sync); err != nil {
		return Manifest{}, err
	}
	if err = sameEntry(dir, "dump", dump); err != nil {
		return Manifest{}, err
	}
	if err = sameEntry(dir, "manifest.json", mf); err != nil {
		return Manifest{}, err
	}
	if err = checkFile(dump, s.ownerUID); err != nil {
		return Manifest{}, err
	}
	if err = privateDir(dir, s.ownerUID); err != nil {
		return Manifest{}, err
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return Manifest{}, err
	}
	if len(entries) != 2 {
		return Manifest{}, errors.New("unexpected staging entries")
	}
	if err = checkFile(mf, s.ownerUID); err != nil {
		return Manifest{}, err
	}
	if err = s.boundary(StageSync, dir.Sync); err != nil {
		return Manifest{}, err
	}
	if err = ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if err = sameEntry(s.root, stage, dir); err != nil {
		return Manifest{}, err
	}
	if err = s.boundary(Rename, func() error {
		return unix.Renameat2(int(s.root.Fd()), stage, int(s.root.Fd()), id, unix.RENAME_NOREPLACE)
	}); err != nil {
		return Manifest{}, err
	}
	published = true
	if err = s.boundary(RootSync, s.root.Sync); err != nil {
		return m, &PublicationError{ID: id, Err: err}
	}
	return m, nil
}
func (s *Store) verified(id string) (Manifest, *os.File, *os.File, error) {
	var m Manifest
	fail := func(err error) (Manifest, *os.File, *os.File, error) { return Manifest{}, nil, nil, err }
	if err := privateDir(s.root, s.ownerUID); err != nil {
		return fail(err)
	}
	if !uuidPattern.MatchString(id) {
		return fail(errors.New("invalid UUIDv4 backup ID"))
	}
	dir, err := openDir(s.root, id, s.ownerUID)
	if err != nil {
		return fail(err)
	}
	ok := false
	defer func() {
		if !ok {
			dir.Close()
		}
	}()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return fail(err)
	}
	if len(entries) != 2 {
		return fail(errors.New("unexpected artifact entries"))
	}
	for _, e := range entries {
		if e.Name() != "dump" && e.Name() != "manifest.json" {
			return fail(errors.New("unknown artifact file"))
		}
	}
	mf, err := openFile(dir, "manifest.json", s.ownerUID)
	if err != nil {
		return fail(err)
	}
	defer mf.Close()
	data, err := io.ReadAll(io.LimitReader(mf, 1024*1024+1))
	if err != nil {
		return fail(err)
	}
	if len(data) > 1024*1024 {
		return fail(errors.New("manifest too large"))
	}
	if err = uniqueJSON(data); err != nil {
		return fail(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil {
		return fail(err)
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return fail(errors.New("trailing manifest data"))
	}
	if m.Version != 1 || m.ID != id || m.CreatedAt.IsZero() || m.DumpSize <= 0 || len(m.DumpSHA256) != 64 {
		return fail(errors.New("invalid or unsupported manifest"))
	}
	if err = validateMetadata(m.Metadata); err != nil {
		return fail(err)
	}
	dump, err := openFile(dir, "dump", s.ownerUID)
	if err != nil {
		return fail(err)
	}
	defer func() {
		if !ok {
			dump.Close()
		}
	}()
	hash, size, err := digest(dump)
	if err != nil {
		return fail(err)
	}
	if hash != m.DumpSHA256 || size != m.DumpSize {
		return fail(errors.New("dump checksum or size mismatch"))
	}
	for _, v := range []struct {
		p *os.File
		n string
		f *os.File
	}{{s.root, id, dir}, {dir, "dump", dump}, {dir, "manifest.json", mf}} {
		if err = sameEntry(v.p, v.n, v.f); err != nil {
			return fail(err)
		}
	}
	if err = checkFile(dump, s.ownerUID); err != nil {
		return fail(err)
	}
	if err = checkFile(mf, s.ownerUID); err != nil {
		return fail(err)
	}
	if _, err = dump.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	ok = true
	return m, dir, dump, nil
}
func (s *Store) Verify(id string) (Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, d, f, err := s.verified(id)
	if err == nil {
		d.Close()
		f.Close()
	}
	return m, err
}

// WithDump verifies first, then provides the pinned read-only dump descriptor.
// Callbacks must not call Store methods or retain the descriptor.
func (s *Store) WithDump(id string, fn func(*os.File) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, d, f, err := s.verified(id)
	if err != nil {
		return err
	}
	defer d.Close()
	defer f.Close()
	return fn(f)
}

// Delete verifies the complete artifact, then durably renames it to the reserved
// deletion namespace before removing bytes. Callers must persist retention intent
// first and invoke ResumeDelete after interruption. Missing artifacts are errors.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opts.ReadOnly {
		return errors.New("store is read-only")
	}
	return s.deleteInitial(id)
}

const (
	DeleteRename     Boundary = "delete-rename"
	DeleteRenameSync Boundary = "delete-rename-sync"
	DeleteDump       Boundary = "delete-dump"
	DeleteManifest   Boundary = "delete-manifest"
)

func (s *Store) deleteInitial(id string) error {
	_, d, f, err := s.verified(id)
	if err != nil {
		return err
	}
	defer d.Close()
	defer f.Close()
	if err = sameEntry(s.root, id, d); err != nil {
		return err
	}
	tomb := ".deleting-" + id
	if err = s.boundary(DeleteRename, func() error {
		return unix.Renameat2(int(s.root.Fd()), id, int(s.root.Fd()), tomb, unix.RENAME_NOREPLACE)
	}); err != nil {
		return err
	}
	return s.finishDelete(tomb, d)
}

// ResumeDelete requires independently durable caller authorization for this ID.
// It resumes a reserved deletion directory, starts a still-complete artifact's
// deletion, or succeeds if both names are already absent. Never use this method
// to infer that an unknown/missing backup was successfully retained or deleted.
func (s *Store) ResumeDelete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opts.ReadOnly {
		return errors.New("store is read-only")
	}
	if !uuidPattern.MatchString(id) {
		return errors.New("invalid UUIDv4 backup ID")
	}
	if err := privateDir(s.root, s.ownerUID); err != nil {
		return err
	}
	tomb := ".deleting-" + id
	var original, deleted unix.Stat_t
	a := unix.Fstatat(int(s.root.Fd()), id, &original, unix.AT_SYMLINK_NOFOLLOW)
	b := unix.Fstatat(int(s.root.Fd()), tomb, &deleted, unix.AT_SYMLINK_NOFOLLOW)
	if a != nil && !errors.Is(a, unix.ENOENT) {
		return a
	}
	if b != nil && !errors.Is(b, unix.ENOENT) {
		return b
	}
	if a == nil && b == nil {
		return errors.New("both original and deletion artifact exist")
	}
	if b != nil {
		if a == nil {
			return s.deleteInitial(id)
		}
		return s.boundary(DeleteSync, s.root.Sync)
	}
	d, err := openDir(s.root, tomb, s.ownerUID)
	if err != nil {
		return err
	}
	defer d.Close()
	return s.finishDelete(tomb, d)
}
func (s *Store) finishDelete(tomb string, d *os.File) error {
	if err := sameEntry(s.root, tomb, d); err != nil {
		return err
	}
	// Validate every remaining entry before touching any, even after interruption.
	if _, err := d.Seek(0, io.SeekStart); err != nil {
		return err
	}
	entries, err := d.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != "dump" && e.Name() != "manifest.json" {
			return errors.New("unknown deletion artifact entry")
		}
		f, err := openFile(d, e.Name(), s.ownerUID)
		if err != nil {
			return err
		}
		err = sameEntry(d, e.Name(), f)
		f.Close()
		if err != nil {
			return err
		}
	}
	if err = s.boundary(DeleteRenameSync, s.root.Sync); err != nil {
		return err
	}
	for _, e := range entries {
		boundary := DeleteDump
		if e.Name() == "manifest.json" {
			boundary = DeleteManifest
		}
		if err = s.boundary(boundary, func() error { return unix.Unlinkat(int(d.Fd()), e.Name(), 0) }); err != nil {
			return err
		}
	}
	if err = s.boundary(DeleteDirSync, d.Sync); err != nil {
		return err
	}
	if err = sameEntry(s.root, tomb, d); err != nil {
		return err
	}
	if err = unix.Unlinkat(int(s.root.Fd()), tomb, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return s.boundary(DeleteSync, s.root.Sync)
}

const (
	StagingRemoveDump     Boundary = "staging-remove-dump"
	StagingRemoveManifest Boundary = "staging-remove-manifest"
	StagingRemoveDirSync  Boundary = "staging-remove-dir-sync"
	StagingRemoveRootSync Boundary = "staging-remove-root-sync"
)

// RemoveStaging removes only .tmp-ID beneath the pinned root. The caller must
// first establish durable failed backup history, an age of at least 48 hours,
// and exclusive ownership of the same PostgreSQL backup advisory lock.
// No root enumeration or published/deletion namespace access is performed.
// Missing staging directories are idempotent under this explicit authorization.
// Partial cleanup is resumable, but all remaining entries must still be safe.
func (s *Store) RemoveStaging(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opts.ReadOnly {
		return errors.New("store is read-only")
	}
	if !uuidPattern.MatchString(id) {
		return errors.New("invalid UUIDv4 backup ID")
	}
	if err := privateDir(s.root, s.ownerUID); err != nil {
		return err
	}
	name := ".tmp-" + id
	d, err := openDir(s.root, name, s.ownerUID)
	if errors.Is(err, unix.ENOENT) {
		return s.boundary(StagingRemoveRootSync, s.root.Sync)
	}
	if err != nil {
		return err
	}
	defer d.Close()
	// Only this exact staging directory is inspected, bounded to three entries:
	// the third entry is sufficient to reject any unexpected contents.
	entries, err := d.ReadDir(3)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) > 2 {
		return errors.New("unknown staging artifact entries")
	}
	type pinned struct {
		name string
		file *os.File
	}
	files := make([]pinned, 0, 2)
	defer func() {
		for _, f := range files {
			f.file.Close()
		}
	}()
	for _, e := range entries {
		if e.Name() != "dump" && e.Name() != "manifest.json" {
			return errors.New("unknown staging artifact entry")
		}
		f, err := openFile(d, e.Name(), s.ownerUID)
		if err != nil {
			return err
		}
		files = append(files, pinned{e.Name(), f})
		if err = sameEntry(d, e.Name(), f); err != nil {
			return err
		}
	}
	if err = sameEntry(s.root, name, d); err != nil {
		return err
	}
	for _, f := range files {
		boundary := StagingRemoveDump
		if f.name == "manifest.json" {
			boundary = StagingRemoveManifest
		}
		if err = s.boundary(boundary, func() error {
			if err := sameEntry(s.root, name, d); err != nil {
				return err
			}
			if err := checkFile(f.file, s.ownerUID); err != nil {
				return err
			}
			if err := sameEntry(d, f.name, f.file); err != nil {
				return err
			}
			return unix.Unlinkat(int(d.Fd()), f.name, 0)
		}); err != nil {
			return err
		}
	}
	if err = s.boundary(StagingRemoveDirSync, d.Sync); err != nil {
		return err
	}
	if err = sameEntry(s.root, name, d); err != nil {
		return err
	}
	if err = unix.Unlinkat(int(s.root.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return s.boundary(StagingRemoveRootSync, s.root.Sync)
}

// encoding/json accepts duplicate object keys; manifests deliberately do not.
func uniqueJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 100 {
			return errors.New("manifest nesting too deep")
		}
		tok, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return errors.New("duplicate manifest object key")
				}
				seen[key] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid manifest JSON")
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing manifest JSON")
	}
	return nil
}
