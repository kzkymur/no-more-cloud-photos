package admin

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestBackupManagerVersionSelectsExactMajor: only 255.x and 259.x are
// supported, each mapped to its own allow-sets; everything else fails closed.
func TestBackupManagerVersionSelectsExactMajor(t *testing.T) {
	for version, want := range map[string]backupManager{
		"255": backupManager255, "255.4-1ubuntu8.6": backupManager255,
		"259": backupManager259, "259.5": backupManager259, "259.5-0ubuntu3.4": backupManager259, "259~rc2": backupManager259,
	} {
		if got, ok := backupManagerFor(version); !ok || got != want {
			t.Errorf("backupManagerFor(%q) = %d, %v; want %d", version, got, ok, want)
		}
	}
	for _, version := range []string{"256", "257.9", "258.1-1", "260", "2590", "259 ", " 259", "v259", "259.5\n", ""} {
		if _, ok := backupManagerFor(version); ok {
			t.Errorf("backupManagerFor(%q) accepted", version)
		}
	}
}

// TestBackupUnitV259MountPaths pins the v259 PrivateTmp change (review
// rationale: unit_add_exec_dependencies, PRIVATE_TMP_CONNECTED).
func TestBackupUnitV259MountPaths(t *testing.T) {
	if got := backupUnitRequiredMountPaths(backupManager259); !reflect.DeepEqual(got, []string{"/var/lib/nmcp-backup", "/run/nmcp-backup"}) {
		t.Fatalf("v259 RequiresMountsFor %q", got)
	}
	if got := backupUnitWantedMountPaths(backupManager259); !reflect.DeepEqual(got, []string{"/tmp", "/var/tmp"}) {
		t.Fatalf("v259 WantsMountsFor %q", got)
	}
	if backupUnitWantedMountPaths(backupManager255) != nil {
		t.Fatal("v255 has no WantsMountsFor")
	}
	if got := backupUnitPathMounts(backupUnitWantedMountPaths(backupManager259)); !reflect.DeepEqual(got, []string{"tmp.mount", "-.mount", "var-tmp.mount", "var.mount"}) {
		t.Fatalf("v259 wanted mount prefixes %q", got)
	}
}

func TestBackupUnitV259DependencyContract(t *testing.T) {
	m := backupManager259
	all := backupUnitPathMounts(append(backupUnitRequiredMountPaths(m), backupUnitWantedMountPaths(m)...))
	for _, tc := range []struct {
		name   string
		mounts []string
		timer  bool
	}{{"minimal", nil, false}, {"timer", nil, true}, {"all-host-mounts", all, true}, {"tmp-and-root", []string{"tmp.mount", "-.mount"}, true}} {
		if err := validateBackupUnitDependencies(backupDependencyFixtureFor(t, m, tc.mounts, tc.timer), backupDependencyTestPG, m); err != nil {
			t.Fatalf("v259 canonical %s rejected: %v", tc.name, err)
		}
	}
	base := backupDependencyFixtureFor(t, m, []string{"tmp.mount", "-.mount"}, true)
	reject := func(label string, q map[string]json.RawMessage) {
		t.Helper()
		if validateBackupUnitDependencies(q, backupDependencyTestPG, m) == nil {
			t.Fatalf("v259 %s accepted", label)
		}
	}
	// v255-only implicit edges are foreign on v259.
	reject("credential mount ordering", backupDependencyWith(t, base, "After", `run-credentials-nmcp\x2dbackup.service.mount`))
	reject("wanted mount not ordered", backupDependencyWith(t, backupDependencyFixtureFor(t, m, nil, true), "Wants", "tmp.mount"))
	reject("wanted path mount required", backupDependencyWith(t, base, "Requires", "tmp.mount"))
	reject("unexpected want", backupDependencyWith(t, base, "Wants", "evil.service"))
	q := map[string]json.RawMessage{}
	for k, v := range base {
		q[k] = v
	}
	q["WantsMountsFor"] = backupContractJSON(t, []string{"/tmp"})
	reject("WantsMountsFor subset", q)
	q["WantsMountsFor"] = backupContractJSON(t, []string{"/tmp", "/var/tmp", "/srv"})
	reject("WantsMountsFor extra", q)
	delete(q, "WantsMountsFor")
	reject("WantsMountsFor missing", q)
	q = map[string]json.RawMessage{}
	for k, v := range base {
		q[k] = v
	}
	q["RequiresMountsFor"] = backupContractJSON(t, backupUnitRequiredMountPaths(backupManager255))
	reject("v255 RequiresMountsFor", q)
	// Neither manager accepts the other's shape.
	if validateBackupUnitDependencies(backupDependencyFixture(t, nil, true), backupDependencyTestPG, m) == nil {
		t.Fatal("v255-shaped unit accepted under v259 rules")
	}
	if validateBackupUnitDependencies(backupDependencyFixtureFor(t, m, nil, true), backupDependencyTestPG, backupManager255) == nil {
		t.Fatal("v259-shaped unit accepted under v255 rules")
	}
	if _, err := backupUnitDependencyContract(backupManager(256), backupDependencyTestPG); err == nil {
		t.Fatal("contract for an unsupported manager")
	}
}

// TestBackupUnitV259ServiceModes: v259's PrivateTmp/ProtectControlGroups/
// ProtectHostname booleans are true for every non-"no" mode, so the exact
// modes and the post-v255 namespace settings are pinned.
func TestBackupUnitV259ServiceModes(t *testing.T) {
	m := backupManager259
	p := backupContractFixtureFor(t, m)
	if err := validateBackupUnitEffective(p, false, m); err != nil {
		t.Fatalf("v259 canonical service rejected: %v", err)
	}
	for name, value := range map[string]any{
		"PrivateTmpEx":           "disconnected",
		"ProtectControlGroupsEx": "private",
		"ProtectHostnameEx":      []any{"private", ""},
		"PrivateUsersEx":         "self",
		"PrivatePIDs":            "yes",
		"PrivateBPF":             "yes",
		"ImportCredentialEx":     []any{[]any{"*", ""}},
		"UserNamespacePath":      "/proc/1/ns/user",
	} {
		q := map[string]json.RawMessage{}
		for k, v := range p {
			q[k] = v
		}
		q[name] = backupContractJSON(t, value)
		if validateBackupUnitEffective(q, false, m) == nil {
			t.Errorf("v259 %s=%v accepted", name, value)
		}
		delete(q, name)
		if validateBackupUnitEffective(q, false, m) == nil {
			t.Errorf("v259 without %s accepted", name)
		}
	}
	if validateBackupUnitEffective(backupContractFixture(t), false, m) == nil {
		t.Fatal("v255 property set (no mode pins) accepted under v259 rules")
	}
	if validateBackupUnitEffective(p, false, backupManager255) != nil {
		t.Fatal("v255 rules must ignore the v259-only mode properties")
	}
}

// TestBackupUnitV259IdentityQueriesWantsMountsFor: only the v259 identity
// read asks for WantsMountsFor (v255's manager has no such property), and
// identity drift compares it as a set.
func TestBackupUnitV259IdentityQueriesWantsMountsFor(t *testing.T) {
	has := func(m backupManager) bool {
		for _, p := range backupUnitIdentityProperties(m) {
			if p.name == "WantsMountsFor" {
				return p.signature == "as"
			}
		}
		return false
	}
	if !has(backupManager259) || has(backupManager255) {
		t.Fatal("WantsMountsFor identity property")
	}
	a := map[string]json.RawMessage{"WantsMountsFor": json.RawMessage(`["/tmp","/var/tmp"]`)}
	b := map[string]json.RawMessage{"WantsMountsFor": json.RawMessage(`["/var/tmp","/tmp"]`)}
	if drift := backupUnitIdentityDrift(a, b); len(drift) != 0 {
		t.Fatalf("set reorder reported as drift: %v", drift)
	}
	b["WantsMountsFor"] = json.RawMessage(`["/tmp"]`)
	if drift := backupUnitIdentityDrift(a, b); len(drift) != 1 {
		t.Fatalf("membership change not drift: %v", drift)
	}
}
