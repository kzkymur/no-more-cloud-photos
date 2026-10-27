package admin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func restoreTestIdentity(oid uint32) RestoreIdentity {
	return RestoreIdentity{ClusterID: "123", DatabaseOID: oid, DatabaseName: "candidate", Owner: "operator"}
}
func restoreTestReceipt() RestoreReceipt {
	return RestoreReceipt{Source: restoreTestIdentity(1), Candidate: restoreTestIdentity(2), Template0: true, Empty: true, RoutineLoginDenied: true, OperatorSocketOnly: true, HBASHA256: strings.Repeat("a", 64), IssuedAt: time.Now().Add(-time.Minute)}
}
func restoreTestOperation() RestoreOperation {
	r := restoreTestReceipt()
	return RestoreOperation{ID: newID(), BackupID: newID(), DumpSHA256: strings.Repeat("b", 64), Source: r.Source, Candidate: r.Candidate, Receipt: r, Phase: "prepared", StartedAt: time.Now(), UpdatedAt: time.Now()}
}
func privateTestDir(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	if e := os.Chmod(p, 0700); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestRestoreReceiptFailClosed(t *testing.T) {
	r := restoreTestReceipt()
	if !validReceipt(r, time.Now()) {
		t.Fatal("valid receipt rejected")
	}
	for name, mutate := range map[string]func(*RestoreReceipt){"same": func(r *RestoreReceipt) { r.Candidate = r.Source }, "old": func(r *RestoreReceipt) { r.IssuedAt = time.Now().Add(-2 * time.Hour) }, "future": func(r *RestoreReceipt) { r.IssuedAt = time.Now().Add(time.Hour) }, "gate": func(r *RestoreReceipt) { r.RoutineLoginDenied = false }, "socket": func(r *RestoreReceipt) { r.OperatorSocketOnly = false }, "empty": func(r *RestoreReceipt) { r.Empty = false }, "template": func(r *RestoreReceipt) { r.Template0 = false }, "digest": func(r *RestoreReceipt) { r.HBASHA256 = "bad" }, "identity": func(r *RestoreReceipt) { r.Candidate.DatabaseOID = 0 }} {
		t.Run(name, func(t *testing.T) {
			r := restoreTestReceipt()
			mutate(&r)
			if validReceipt(r, time.Now()) {
				t.Fatal("insufficient receipt accepted")
			}
		})
	}
}
func TestRestoreLedgerDurableAppendAndReservation(t *testing.T) {
	root := privateTestDir(t)
	op := restoreTestOperation()
	if e := reserveCandidate(root, op.Candidate, op.ID); e != nil {
		t.Fatal(e)
	}
	if reserveCandidate(root, op.Candidate, newID()) == nil {
		t.Fatal("candidate authorized twice")
	}
	if e := SaveRestoreOperation(root, op); e != nil {
		t.Fatal(e)
	}
	if SaveRestoreOperation(root, op) == nil {
		t.Fatal("phase overwritten")
	}
	got, e := LoadRestoreOperation(root, op.ID)
	if e != nil || got.ID != op.ID || got.Phase != "prepared" {
		t.Fatalf("load=%+v err=%v", got, e)
	}
	op.Phase = "validated"
	op.UpdatedAt = op.UpdatedAt.Add(time.Second)
	if e := SaveRestoreOperation(root, op); e != nil {
		t.Fatal(e)
	}
	got, e = LoadRestoreOperation(root, op.ID)
	if e != nil || got.Phase != "validated" {
		t.Fatalf("latest=%+v err=%v", got, e)
	}
	if _, e = LoadRestoreOperation(root, "../outside"); e == nil {
		t.Fatal("path traversal accepted")
	}
	op.Phase = "../escape"
	if SaveRestoreOperation(root, op) == nil {
		t.Fatal("phase path accepted")
	}
}
func TestRestoreLedgerRefusesSymlinksAndPublicRoot(t *testing.T) {
	root := privateTestDir(t)
	target := privateTestDir(t)
	link := filepath.Join(root, "link")
	if e := os.Symlink(target, link); e != nil {
		t.Fatal(e)
	}
	if _, e := privateDirectory(link); e == nil {
		t.Fatal("symlink root accepted")
	}
	op := restoreTestOperation()
	name := op.ID + "." + op.Phase + ".json"
	if e := os.Symlink(filepath.Join(target, "outside"), filepath.Join(root, name)); e != nil {
		t.Fatal(e)
	}
	if SaveRestoreOperation(root, op) == nil {
		t.Fatal("symlink ledger accepted")
	}
	if _, e := LoadRestoreOperation(root, op.ID); e == nil {
		t.Fatal("read symlink accepted")
	}
	if e := os.Chmod(target, 0755); e != nil {
		t.Fatal(e)
	}
	if SaveRestoreOperation(target, op) == nil {
		t.Fatal("public ledger accepted")
	}
}
func TestRestorePrivateReceiptStrict(t *testing.T) {
	root := privateTestDir(t)
	p := filepath.Join(root, "receipt")
	r := restoreTestReceipt()
	b, _ := json.Marshal(r)
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	var got RestoreReceipt
	if e := decodePrivate(p, &got); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(p, 0644); e != nil {
		t.Fatal(e)
	}
	if decodePrivate(p, &got) == nil {
		t.Fatal("public receipt accepted")
	}
	if decodeRestore(append(b, []byte(" {}")...), &got) == nil {
		t.Fatal("trailing JSON accepted")
	}
	if decodeRestore([]byte(`{"unknown":true}`), &got) == nil {
		t.Fatal("unknown field accepted")
	}
}
func TestRestoreSettingsBoundaries(t *testing.T) {
	c := RestoreSettings{Root: "/private/backups", LedgerRoot: "/private/restore", ReceiptFile: "/private/receipt", Service: "operator", APIService: "api", ServiceFile: "/private/service", PassFile: "/private/pass", StorageRoot: "/private/storage", FileBaseURL: "https://files.example.test/files/"}
	if c.validate() != nil {
		t.Fatal("valid config rejected")
	}
	for _, root := range []string{"relative", "/private/backups/restore", "/private/storage/restore", "/private", "/private/../restore"} {
		bad := c
		bad.LedgerRoot = root
		if bad.validate() == nil {
			t.Fatalf("unsafe ledger root %s", root)
		}
	}
	c.RestoreTool = "pg_restore"
	if c.validate() == nil {
		t.Fatal("relative tool accepted")
	}
	if _, e := New(nil).Restore(context.Background(), newID(), RestoreSettings{}); e == nil {
		t.Fatal("empty settings admitted")
	}
}
