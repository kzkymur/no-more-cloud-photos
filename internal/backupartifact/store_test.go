package backupartifact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func metadata(t *testing.T) Metadata {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return Metadata{ID: id, SnapshotCutoff: time.Now().UTC(), Config: json.RawMessage(`{"rpo":24}`), ToolVersions: map[string]string{"pg_dump": "17.6", "pg_restore": "17.6"}, ServerVersion: "17.6"}
}
func writer(_ context.Context, f *os.File) error {
	_, err := f.WriteString("PGDMP-test-fixture")
	return err
}
func validator(_ context.Context, f *os.File) error {
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if string(b) != "PGDMP-test-fixture" {
		return errors.New("invalid dump")
	}
	return nil
}
func store(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, root
}
func TestPublishVerifyDelete(t *testing.T) {
	s, root := store(t, Options{})
	meta := metadata(t)
	m, err := s.Publish(context.Background(), meta, writer, validator)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != meta.ID || m.DumpSize != 18 {
		t.Fatalf("bad manifest: %+v", m)
	}
	got, err := s.Verify(m.ID)
	if err != nil || got.DumpSHA256 != m.DumpSHA256 {
		t.Fatalf("verify: %+v %v", got, err)
	}
	if err = s.WithDump(m.ID, func(f *os.File) error { return validator(context.Background(), f) }); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, m.ID)); !os.IsNotExist(err) {
		t.Fatalf("not deleted: %v", err)
	}
}
func TestFailuresNeverPublish(t *testing.T) {
	for _, boundary := range []Boundary{DumpSync, ManifestSync, StageSync, Rename} {
		t.Run(string(boundary), func(t *testing.T) {
			s, root := store(t, Options{Fault: func(b Boundary) error {
				if b == boundary {
					return errors.New("fault")
				}
				return nil
			}})
			meta := metadata(t)
			if _, err := s.Publish(context.Background(), meta, writer, validator); err == nil {
				t.Fatal("unexpected success")
			}
			if _, err := s.Verify(meta.ID); err == nil {
				t.Fatal("successful artifact after failure")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("stage leaked %v %v", entries, err)
			}
		})
	}
}
func TestUncertainPreservesPublished(t *testing.T) {
	s, root := store(t, Options{Fault: func(b Boundary) error {
		if b == RootSync {
			return errors.New("fsync fault")
		}
		return nil
	}})
	meta := metadata(t)
	m, err := s.Publish(context.Background(), meta, writer, validator)
	var uncertain *PublicationError
	if !errors.As(err, &uncertain) || uncertain.ID != meta.ID || m.ID != meta.ID {
		t.Fatalf("wrong uncertainty: %v", err)
	}
	if _, err = s.Verify(meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, meta.ID, "dump")); err != nil {
		t.Fatal(err)
	}
}
func TestCorruptionRefusesVerifyAndDelete(t *testing.T) {
	for _, kind := range []string{"content", "hardlink", "symlink", "unknown-version", "unknown-field", "extra-file", "nonregular"} {
		t.Run(kind, func(t *testing.T) {
			s, root := store(t, Options{})
			m, err := s.Publish(context.Background(), metadata(t), writer, validator)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, m.ID)
			dump := filepath.Join(dir, "dump")
			manifest := filepath.Join(dir, "manifest.json")
			switch kind {
			case "content":
				err = os.WriteFile(dump, []byte("corrupt"), 0600)
			case "hardlink":
				err = os.Link(dump, filepath.Join(root, "other"))
			case "symlink":
				if err = os.Remove(dump); err == nil {
					err = os.Symlink("/etc/passwd", dump)
				}
			case "nonregular":
				if err = os.Remove(dump); err == nil {
					err = os.Mkdir(dump, 0700)
				}
			case "unknown-version":
				m.Version = 2
				var b []byte
				b, err = json.Marshal(m)
				if err == nil {
					err = os.WriteFile(manifest, b, 0600)
				}
			case "unknown-field":
				var b []byte
				b, err = os.ReadFile(manifest)
				if err == nil {
					b = append([]byte(`{"surprise":true,`), b[1:]...)
					err = os.WriteFile(manifest, b, 0600)
				}
			case "extra-file":
				err = os.WriteFile(filepath.Join(dir, "extra"), nil, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Verify(m.ID); err == nil {
				t.Fatal("accepted corruption")
			}
			if err = s.Delete(m.ID); err == nil {
				t.Fatal("deleted unverified backup")
			}
			if _, err = os.Lstat(dir); err != nil {
				t.Fatal("deleted corrupt directory")
			}
		})
	}
}
func TestRootAndIDSecurity(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, filepath.Join(link, "child"), "relative"} {
		if s, err := Open(p, Options{}); err == nil {
			s.Close()
			t.Fatalf("accepted %s", p)
		}
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(root, Options{}); err == nil {
		s.Close()
		t.Fatal("accepted public root")
	}
	s, _ := store(t, Options{})
	for _, id := range []string{"../escape", ".tmp-foo", "00000000-0000-0000-0000-000000000000"} {
		if _, err := s.Verify(id); err == nil {
			t.Fatal("accepted invalid id")
		}
	}
}
func TestAtomicAndNoReplace(t *testing.T) {
	s, root := store(t, Options{})
	meta := metadata(t)
	write := func(ctx context.Context, f *os.File) error {
		if _, err := os.Stat(filepath.Join(root, meta.ID)); !os.IsNotExist(err) {
			t.Fatalf("visible before publication: %v", err)
		}
		return writer(ctx, f)
	}
	if _, err := s.Publish(context.Background(), meta, write, validator); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, meta.ID, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Publish(context.Background(), meta, writer, validator); err == nil {
		t.Fatal("overwrote artifact")
	}
	after, err := os.ReadFile(filepath.Join(root, meta.ID, "manifest.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("published bytes changed")
	}
}
func TestCallbackFailure(t *testing.T) {
	for _, failWrite := range []bool{true, false} {
		s, _ := store(t, Options{})
		bad := func(context.Context, *os.File) error { return errors.New("callback failed") }
		w, v := writer, validator
		if failWrite {
			w = bad
		} else {
			v = bad
		}
		meta := metadata(t)
		if _, err := s.Publish(context.Background(), meta, w, v); err == nil {
			t.Fatal("callback failure accepted")
		}
		if _, err := s.Verify(meta.ID); err == nil {
			t.Fatal("callback failure published")
		}
	}
}
func TestPinnedRootSurvivesReplacement(t *testing.T) {
	s, root := store(t, Options{})
	old := root + "-old"
	if err := os.Rename(root, old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(old) })
	if err := os.Symlink(old, root); err != nil {
		t.Fatal(err)
	}
	meta := metadata(t)
	if _, err := s.Publish(context.Background(), meta, writer, validator); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(meta.ID); err != nil {
		t.Fatal(err)
	}
}
func TestDuplicateManifestKeys(t *testing.T) {
	s, root := store(t, Options{})
	m, err := s.Publish(context.Background(), metadata(t), writer, validator)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, m.ID, "manifest.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b = append([]byte(`{"version":1,`), b[1:]...)
	if err = os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Verify(m.ID); err == nil {
		t.Fatal("duplicate key accepted")
	}
}
func TestPublishRejectsFileSwap(t *testing.T) {
	s, root := store(t, Options{})
	meta := metadata(t)
	validate := func(ctx context.Context, f *os.File) error {
		p := filepath.Join(root, ".tmp-"+meta.ID, "dump")
		if err := os.Rename(p, p+"-old"); err != nil {
			return err
		}
		return os.WriteFile(p, []byte("replacement"), 0600)
	}
	if _, err := s.Publish(context.Background(), meta, writer, validate); err == nil {
		t.Fatal("accepted swapped path")
	}
	if _, err := s.Verify(meta.ID); err == nil {
		t.Fatal("swap published")
	}
}
func TestReadOnlyValidator(t *testing.T) {
	s, _ := store(t, Options{})
	validate := func(ctx context.Context, f *os.File) error {
		if _, err := f.WriteString("bad"); err == nil {
			return errors.New("validator file writable")
		}
		return validator(ctx, f)
	}
	if _, err := s.Publish(context.Background(), metadata(t), writer, validate); err != nil {
		t.Fatal(err)
	}
}

func TestDeletionResumeAtEveryBoundary(t *testing.T) {
	for _, boundary := range []Boundary{DeleteRename, DeleteRenameSync, DeleteDump, DeleteManifest, DeleteDirSync, DeleteSync} {
		t.Run(string(boundary), func(t *testing.T) {
			var fail bool
			s, root := store(t, Options{Fault: func(b Boundary) error {
				if fail && b == boundary {
					return errors.New("delete fault")
				}
				return nil
			}})
			m, err := s.Publish(context.Background(), metadata(t), writer, validator)
			if err != nil {
				t.Fatal(err)
			}
			fail = true
			if err = s.Delete(m.ID); err == nil {
				t.Fatal("fault ignored")
			}
			fail = false
			if err = s.ResumeDelete(m.ID); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("deletion incomplete %v %v", entries, err)
			}
			if err = s.ResumeDelete(m.ID); err != nil {
				t.Fatal("resume not idempotent", err)
			}
			if err = s.Delete(m.ID); err == nil {
				t.Fatal("initial delete accepted missing artifact")
			}
		})
	}
}
func TestDeletionRenameIsDurableBeforeUnlink(t *testing.T) {
	fail := false
	s, root := store(t, Options{Fault: func(b Boundary) error {
		if fail && b == DeleteRenameSync {
			return errors.New("parent fsync fault")
		}
		return nil
	}})
	m, err := s.Publish(context.Background(), metadata(t), writer, validator)
	if err != nil {
		t.Fatal(err)
	}
	fail = true
	if err = s.Delete(m.ID); err == nil {
		t.Fatal("expected failure")
	}
	for _, name := range []string{"dump", "manifest.json"} {
		if _, err = os.Stat(filepath.Join(root, ".deleting-"+m.ID, name)); err != nil {
			t.Fatal("removed before durable rename", err)
		}
	}
}
func TestResumeDeletionRejectsUnsafeRemainingEntries(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			fail := false
			s, root := store(t, Options{Fault: func(b Boundary) error {
				if fail && b == DeleteRenameSync {
					return errors.New("fault")
				}
				return nil
			}})
			m, err := s.Publish(context.Background(), metadata(t), writer, validator)
			if err != nil {
				t.Fatal(err)
			}
			fail = true
			if err = s.Delete(m.ID); err == nil {
				t.Fatal("expected failure")
			}
			fail = false
			dir := filepath.Join(root, ".deleting-"+m.ID)
			dump := filepath.Join(dir, "dump")
			switch kind {
			case "symlink":
				if err = os.Remove(dump); err == nil {
					err = os.Symlink("/etc/passwd", dump)
				}
			case "hardlink":
				err = os.Link(dump, filepath.Join(root, "other"))
			case "unknown":
				err = os.WriteFile(filepath.Join(dir, "extra"), nil, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = s.ResumeDelete(m.ID); err == nil {
				t.Fatal("unsafe deletion accepted")
			}
			if _, err = os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
				t.Fatal("deleted before checking all entries", err)
			}
		})
	}
}
func TestReadOnlyStoreDisallowsAllMutations(t *testing.T) {
	s, root := store(t, Options{})
	m, err := s.Publish(context.Background(), metadata(t), writer, validator)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := Open(root, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err = ro.Verify(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = ro.Publish(context.Background(), metadata(t), writer, validator); err == nil {
		t.Fatal("read-only publication")
	}
	if err = ro.Delete(m.ID); err == nil {
		t.Fatal("read-only deletion")
	}
	if err = ro.ResumeDelete(m.ID); err == nil {
		t.Fatal("read-only resumed deletion")
	}
	if _, err = s.Verify(m.ID); err != nil {
		t.Fatal("read-only store changed data", err)
	}
}
func TestOwnerOverrideAuthorization(t *testing.T) {
	uid := uint32(os.Geteuid())
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, readonly := range []bool{false, true} {
		s, err := Open(root, Options{OwnerUID: &uid, ReadOnly: readonly})
		if os.Geteuid() == 0 {
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
		} else if err == nil {
			s.Close()
			t.Fatal("non-root owner override accepted")
		}
	}
}

// Exercise the independent publication guard on every UID without requiring
// privileges, a real foreign-owned directory, or a successful Open override.
func TestPublicationRequiresEffectiveOwner(t *testing.T) {
	s, root := store(t, Options{})
	s.ownerUID = uint32(os.Geteuid()) + 1
	called := false
	write := func(context.Context, *os.File) error { called = true; return nil }
	_, err := s.Publish(context.Background(), metadata(t), write, validator)
	if err == nil || err.Error() != "publication requires effective UID to match backup owner" || called {
		t.Fatalf("foreign owner publication guard: %v callback=%v", err, called)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("publication mutated root: %v %v", entries, err)
	}
}
func TestRootForeignOwnerCleanup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create genuinely foreign-owned fixture")
	}
	s, root := store(t, Options{})
	m, err := s.Publish(context.Background(), metadata(t), writer, validator)
	if err != nil {
		t.Fatal(err)
	}
	const foreignUID = uint32(65534)
	for _, path := range []string{filepath.Join(root, m.ID, "dump"), filepath.Join(root, m.ID, "manifest.json"), filepath.Join(root, m.ID), root} {
		if err = os.Chown(path, int(foreignUID), -1); err != nil {
			t.Fatal(err)
		}
	}
	owner := foreignUID
	operator, err := Open(root, Options{OwnerUID: &owner})
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	if _, err = operator.Verify(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = operator.Publish(context.Background(), metadata(t), writer, validator); err == nil {
		t.Fatal("root published as backup UID")
	}
	if err = operator.Delete(m.ID); err != nil {
		t.Fatal(err)
	}
	if err = operator.ResumeDelete(m.ID); err != nil {
		t.Fatal(err)
	}
}

func stagingFixture(t *testing.T, root, id string, names ...string) string {
	t.Helper()
	dir := filepath.Join(root, ".tmp-"+id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("partial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
func TestRemoveStagingResumesPartialAndMissing(t *testing.T) {
	for _, names := range [][]string{nil, {"dump"}, {"manifest.json"}, {"dump", "manifest.json"}} {
		s, root := store(t, Options{})
		id := metadata(t).ID
		dir := stagingFixture(t, root, id, names...)
		if err := s.RemoveStaging(id); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("stage remains: %v", err)
		}
		if err := s.RemoveStaging(id); err != nil {
			t.Fatal("missing stage not idempotent", err)
		}
	}
}
func TestRemoveStagingRejectsUnsafe(t *testing.T) {
	for _, kind := range []string{"unknown", "too-many", "symlink", "hardlink", "directory", "public", "public-dir", "symlink-dir"} {
		t.Run(kind, func(t *testing.T) {
			s, root := store(t, Options{})
			id := metadata(t).ID
			dir := stagingFixture(t, root, id, "dump", "manifest.json")
			dump := filepath.Join(dir, "dump")
			var err error
			switch kind {
			case "unknown":
				err = os.Rename(dump, filepath.Join(dir, "unknown"))
			case "too-many":
				err = os.WriteFile(filepath.Join(dir, "extra"), nil, 0600)
			case "symlink":
				if err = os.Remove(dump); err == nil {
					err = os.Symlink("/etc/passwd", dump)
				}
			case "hardlink":
				err = os.Link(dump, filepath.Join(root, "linked"))
			case "directory":
				if err = os.Remove(dump); err == nil {
					err = os.Mkdir(dump, 0700)
				}
			case "public":
				err = os.Chmod(dump, 0644)
			case "public-dir":
				err = os.Chmod(dir, 0755)
			case "symlink-dir":
				target := filepath.Join(root, "target")
				if err = os.Rename(dir, target); err == nil {
					err = os.Symlink(target, dir)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = s.RemoveStaging(id); err == nil {
				t.Fatal("unsafe stage accepted")
			}
			if _, err = os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
				t.Fatal("removed before validating all entries", err)
			}
		})
	}
}
func TestRemoveStagingPreservesOtherNamespaces(t *testing.T) {
	s, root := store(t, Options{})
	m, err := s.Publish(context.Background(), metadata(t), writer, validator)
	if err != nil {
		t.Fatal(err)
	}
	tomb := filepath.Join(root, ".deleting-"+m.ID)
	if err = os.Mkdir(tomb, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tomb, "dump")
	if err = os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	stagingFixture(t, root, m.ID, "dump")
	if err = s.RemoveStaging(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Verify(m.ID); err != nil {
		t.Fatal("published artifact changed", err)
	}
	if b, e := os.ReadFile(marker); e != nil || string(b) != "keep" {
		t.Fatal("deletion namespace changed", e)
	}
	for _, id := range []string{"../escape", ".tmp-" + m.ID, ".deleting-" + m.ID, "00000000-0000-0000-0000-000000000000"} {
		if err = s.RemoveStaging(id); err == nil {
			t.Fatalf("invalid id accepted %q", id)
		}
	}
}
func TestRemoveStagingReadOnly(t *testing.T) {
	_, root := store(t, Options{})
	id := metadata(t).ID
	dir := stagingFixture(t, root, id, "dump")
	ro, err := Open(root, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err = ro.RemoveStaging(id); err == nil {
		t.Fatal("read-only cleanup accepted")
	}
	if _, err = os.Stat(filepath.Join(dir, "dump")); err != nil {
		t.Fatal("read-only cleanup mutated stage", err)
	}
}
func TestRemoveStagingDurabilityFailureResume(t *testing.T) {
	for _, boundary := range []Boundary{StagingRemoveDump, StagingRemoveManifest, StagingRemoveDirSync, StagingRemoveRootSync} {
		t.Run(string(boundary), func(t *testing.T) {
			fail := true
			s, root := store(t, Options{Fault: func(b Boundary) error {
				if fail && b == boundary {
					return errors.New("staging fault")
				}
				return nil
			}})
			id := metadata(t).ID
			dir := stagingFixture(t, root, id, "dump", "manifest.json")
			if err := s.RemoveStaging(id); err == nil {
				t.Fatal("ignored fault")
			}
			fail = false
			if err := s.RemoveStaging(id); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("stage remains", err)
			}
		})
	}
}
func TestRemoveStagingRejectsFileSwapBeforeUnlink(t *testing.T) {
	var swap func() error
	s, root := store(t, Options{Fault: func(b Boundary) error {
		if b == StagingRemoveDump && swap != nil {
			return swap()
		}
		return nil
	}})
	id := metadata(t).ID
	dir := stagingFixture(t, root, id, "dump")
	p := filepath.Join(dir, "dump")
	swap = func() error {
		if err := os.Rename(p, p+"-old"); err != nil {
			return err
		}
		return os.WriteFile(p, []byte("replacement"), 0600)
	}
	if err := s.RemoveStaging(id); err == nil {
		t.Fatal("accepted swapped file")
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "replacement" {
		t.Fatal("removed replacement", err)
	}
}
func TestRemoveStagingRejectsDirectorySwapBeforeUnlink(t *testing.T) {
	var swap func() error
	s, root := store(t, Options{Fault: func(b Boundary) error {
		if b == StagingRemoveDump && swap != nil {
			return swap()
		}
		return nil
	}})
	id := metadata(t).ID
	dir := stagingFixture(t, root, id, "dump")
	moved := dir + "-moved"
	swap = func() error {
		if err := os.Rename(dir, moved); err != nil {
			return err
		}
		return os.Symlink(moved, dir)
	}
	if err := s.RemoveStaging(id); err == nil {
		t.Fatal("accepted swapped stage directory")
	}
	if _, err := os.Stat(filepath.Join(moved, "dump")); err != nil {
		t.Fatal("mutated detached stage directory", err)
	}
}

func TestOperationLockExcludesReadersAndWriters(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	writerStore, err := Open(root, Options{Lock: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []Options{{Lock: true}, {Lock: true, ReadOnly: true}} {
		competing, err := Open(root, opts)
		if competing != nil {
			competing.Close()
		}
		if !errors.Is(err, ErrLocked) {
			t.Fatalf("exclusive holder did not reject a competing operation: %v", err)
		}
	}
	if err = writerStore.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := Open(root, Options{Lock: true, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(root, Options{Lock: true, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	competing, err := Open(root, Options{Lock: true})
	if competing != nil {
		competing.Close()
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("shared readers did not fence destructive operation: %v", err)
	}
}
