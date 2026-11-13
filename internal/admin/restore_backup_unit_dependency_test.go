package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const backupDependencyTestPG = "postgresql@17-main.service"

// The manager-shaped Unit-interface fixture for the canonical installed unit.
// mounts/timer select the host-dependent optional edges.
func backupDependencyFixture(t *testing.T, mounts []string, timer bool) map[string]json.RawMessage {
	return backupDependencyFixtureFor(t, backupManager255, mounts, timer)
}

// backupDependencyFixtureFor builds the fixture for one manager major. Each
// given mount unit is treated as loaded with a fragment: ordered, and required
// or wanted exactly as unit_add_mount_dependencies would for its path set.
func backupDependencyFixtureFor(t *testing.T, m backupManager, mounts []string, timer bool) map[string]json.RawMessage {
	t.Helper()
	contract, err := backupUnitDependencyContract(m, backupDependencyTestPG)
	if err != nil {
		t.Fatal(err)
	}
	p := map[string]json.RawMessage{}
	for _, name := range backupUnitEdgeProperties {
		p[name] = backupContractJSON(t, append([]string{}, contract[name].required...))
	}
	add := func(name string, units ...string) {
		var got []string
		if err := json.Unmarshal(p[name], &got); err != nil {
			t.Fatal(err)
		}
		p[name] = backupContractJSON(t, append(got, units...))
	}
	in := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	required, wanted := backupUnitPathMounts(backupUnitRequiredMountPaths(m)), backupUnitPathMounts(backupUnitWantedMountPaths(m))
	var after []string
	for _, unit := range mounts {
		if in(required, unit) {
			add("Requires", unit)
		}
		if in(wanted, unit) {
			var got []string
			_ = json.Unmarshal(p["Wants"], &got)
			if !in(got, unit) {
				add("Wants", unit)
			}
		}
		var got []string
		_ = json.Unmarshal(p["After"], &got)
		if !in(got, unit) && !in(after, unit) {
			after = append(after, unit)
		}
	}
	add("After", after...)
	if timer {
		add("After", "nmcp-backup.timer")
		add("TriggeredBy", "nmcp-backup.timer")
	}
	p["RequiresMountsFor"] = backupContractJSON(t, backupUnitRequiredMountPaths(m))
	if m == backupManager259 {
		p["WantsMountsFor"] = backupContractJSON(t, backupUnitWantedMountPaths(m))
	}
	p["Conditions"] = json.RawMessage(`[]`)
	p["Asserts"] = json.RawMessage(`[]`)
	p["DefaultDependencies"] = json.RawMessage(`true`)
	return p
}
func backupDependencyWith(t *testing.T, p map[string]json.RawMessage, name string, units ...string) map[string]json.RawMessage {
	t.Helper()
	q := map[string]json.RawMessage{}
	for k, v := range p {
		q[k] = v
	}
	var got []string
	if err := json.Unmarshal(q[name], &got); err != nil {
		t.Fatal(err)
	}
	q[name] = backupContractJSON(t, append(got, units...))
	return q
}

func TestBackupUnitPathDerivedMountNames(t *testing.T) {
	got := backupUnitPathMounts(backupUnitRequiredMountPaths(backupManager255))
	want := []string{`var-lib-nmcp\x2dbackup.mount`, "var-lib.mount", "var.mount", "-.mount", "var-tmp.mount", `run-nmcp\x2dbackup.mount`, "run.mount"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("path-derived mounts %q", got)
	}
	for path, name := range map[string]string{"/": "-.mount", "/run/credentials/nmcp-backup.service": `run-credentials-nmcp\x2dbackup.service.mount`, "/a/.b": "a-.b.mount", "/.a": `\x2ea.mount`, "/tmp": "tmp.mount"} {
		if backupMountUnitName(path) != name {
			t.Fatalf("unit_name_from_path(%s)", path)
		}
	}
}

func TestBackupUnitEffectiveDependencyContract(t *testing.T) {
	all := backupUnitPathMounts(backupUnitRequiredMountPaths(backupManager255))
	for _, tc := range []struct {
		name   string
		mounts []string
		timer  bool
	}{{"minimal", nil, false}, {"timer", nil, true}, {"all-host-mounts", all, true}, {"root-only", []string{"-.mount"}, true}} {
		if err := validateBackupUnitDependencies(backupDependencyFixture(t, tc.mounts, tc.timer), backupDependencyTestPG, backupManager255); err != nil {
			t.Fatalf("canonical %s rejected: %v", tc.name, err)
		}
	}
	// A loaded mount without a fragment is ordered but not required.
	p := backupDependencyWith(t, backupDependencyFixture(t, nil, true), "After", "var.mount", "run.mount")
	if validateBackupUnitDependencies(p, backupDependencyTestPG, backupManager255) != nil {
		t.Fatal("ordering-only path mount rejected")
	}
	base := backupDependencyFixture(t, []string{"-.mount"}, true)
	reject := func(label string, q map[string]json.RawMessage) {
		t.Helper()
		if validateBackupUnitDependencies(q, backupDependencyTestPG, backupManager255) == nil {
			t.Fatalf("%s accepted", label)
		}
	}
	// Every activation/ordering/propagation edge, and every reverse list.
	for _, name := range backupUnitEdgeProperties {
		reject("injected "+name+"=evil.service", backupDependencyWith(t, base, name, "evil.service"))
		reject("injected "+name+" second PG", backupDependencyWith(t, base, name, "postgresql@17-other.service"))
		q := backupDependencyWith(t, base, name)
		delete(q, name)
		reject("missing "+name, q)
		q = backupDependencyWith(t, base, name)
		q[name] = json.RawMessage(`null`)
		reject("null "+name, q)
	}
	for _, c := range []struct{ property, unit string }{
		{"Wants", "evil.service"}, {"Requires", "other.service"}, {"BindsTo", "evil.service"}, {"Upholds", "evil.service"},
		{"Requisite", "evil.service"}, {"Triggers", "evil.service"}, {"PartOf", "evil.service"},
		{"OnFailure", "evil.service"}, {"OnSuccess", "evil.service"}, {"PropagatesReloadTo", "evil.service"},
		{"JoinsNamespaceOf", "evil.service"}, {"Conflicts", "nmcp-api.service"}, {"Before", "nmcp-api.service"},
		// Not path-derived mounts; never "any *.mount".
		{"Requires", "srv.mount"}, {"After", "srv-data.mount"}, {"Requires", "var-lib-postgresql.mount"},
		{"Wants", "var.mount"}, {"After", "run-credentials-other.service.mount"},
	} {
		reject(c.property+"="+c.unit, backupDependencyWith(t, base, c.property, c.unit))
	}
	// A required path mount must also be ordered (as v255 always adds both).
	reject("required unordered mount", backupDependencyWith(t, backupDependencyFixture(t, nil, true), "Requires", "var.mount"))
	// The timer edge is only ever After+TriggeredBy together.
	q := backupDependencyFixture(t, nil, false)
	reject("TriggeredBy without After", backupDependencyWith(t, q, "TriggeredBy", "nmcp-backup.timer"))
	reject("timer After without TriggeredBy", backupDependencyWith(t, q, "After", "nmcp-backup.timer"))
	reject("duplicate", backupDependencyWith(t, base, "Requires", backupDependencyTestPG))
	reject("empty name", backupDependencyWith(t, base, "After", ""))
	for name, units := range map[string][]string{"Requires": {"sysinit.target", "nmcp.slice"}, "After": {"basic.target"}, "Wants": nil, "Conflicts": nil, "Before": nil, "PartOf": nil} {
		q := backupDependencyWith(t, base, name)
		q[name] = backupContractJSON(t, append([]string{}, units...))
		reject("missing required "+name, q)
	}
	for _, mounts := range []any{[]string{"/var/lib/nmcp-backup", "/var/tmp"}, []string{"/var/lib/nmcp-backup", "/var/tmp", "/run/nmcp-backup", "/srv"}, []string{}, nil} {
		q := backupDependencyWith(t, base, "Requires")
		q["RequiresMountsFor"] = backupContractJSON(t, mounts)
		reject("RequiresMountsFor", q)
	}
	for _, name := range []string{"Conditions", "Asserts"} {
		kind := strings.TrimSuffix(name, "s") + "PathExists"
		for _, value := range []string{
			`[["` + kind + `",false,false,"/nonexistent",0]]`,
			`[["` + kind + `",true,true,"/var/lib/nmcp-backup",1]]`,
			`null`, `{}`,
		} {
			q := backupDependencyWith(t, base, "Requires")
			q[name] = json.RawMessage(value)
			reject("added "+name+" "+value, q)
		}
	}
	q = backupDependencyWith(t, base, "Requires")
	q["DefaultDependencies"] = json.RawMessage(`false`)
	reject("DefaultDependencies=no", q)
	if validateBackupUnitDependencies(base, "postgresql@17-other.service", backupManager255) == nil || validateBackupUnitDependencies(base, "evil.service", backupManager255) == nil {
		t.Fatal("different dependency authority accepted")
	}
}

// Wire-level: the manager reply is rejected before policy for any non-empty
// condition/assert list or an edge whose canonical set is empty.
func TestBackupUnitIdentityWireRejectsConditionsAndEmptyEdges(t *testing.T) {
	props := backupUnitIdentityProperties(backupManager255)
	signatures := map[string]string{}
	for _, p := range props {
		if _, dup := signatures[p.name]; dup {
			t.Fatal("duplicate identity property")
		}
		signatures[p.name] = p.signature
	}
	for _, name := range backupUnitEdgeProperties {
		if signatures[name] != "as" {
			t.Fatalf("edge %s not queried as typed string array", name)
		}
	}
	for name, sig := range map[string]string{"Conditions": "a(sbbsi)", "Asserts": "a(sbbsi)", "RequiresMountsFor": "as", "DefaultDependencies": "b", "SourcePath": "s"} {
		if signatures[name] != sig {
			t.Fatalf("%s not typed %s", name, sig)
		}
	}
	values := backupDependencyFixture(t, []string{"-.mount"}, true)
	for k, v := range map[string]any{"Id": "nmcp-backup.service", "LoadState": "loaded", "FragmentPath": backupUnitFragment, "SourcePath": "", "Transient": false, "NeedDaemonReload": false, "DropInPaths": []string{backupPGDependencyDropin}} {
		values[k] = backupContractJSON(t, v)
	}
	encode := func(v map[string]json.RawMessage) []byte {
		var b bytes.Buffer
		for _, p := range props {
			fmt.Fprintf(&b, "{\"type\":%q,\"data\":%s}\n", p.signature, v[p.name])
		}
		return b.Bytes()
	}
	decoded, err := decodeBackupUnitProperties(encode(values), props)
	if err != nil || validateBackupUnitDependencies(decoded, backupDependencyTestPG, backupManager255) != nil {
		t.Fatalf("canonical wire rejected: %v", err)
	}
	for name, value := range map[string]string{
		"Conditions": `[["ConditionPathExists",false,false,"/nonexistent",0]]`,
		"Asserts":    `[["AssertPathExists",false,false,"/nonexistent",0]]`,
		"Upholds":    `["evil.service"]`, "BindsTo": `["evil.service"]`, "Requisite": `["evil.service"]`,
		"Triggers": `["evil.service"]`, "OnFailure": `["evil.service"]`, "JoinsNamespaceOf": `["evil.service"]`,
	} {
		q := map[string]json.RawMessage{}
		for k, v := range values {
			q[k] = v
		}
		q[name] = json.RawMessage(value)
		if _, err := decodeBackupUnitProperties(encode(q), props); err == nil {
			t.Fatalf("wire admitted %s", name)
		}
	}
}

func TestBackupUnitFragmentContentPin(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(pg17Repository(t), "infra/ubuntu/systemd/nmcp-backup.service"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != backupUnitFragmentSHA256 || validateBackupUnitFragment(body) != nil {
		t.Fatal("pinned digest is not the canonical shipped fragment")
	}
	for _, changed := range [][]byte{
		bytes.Replace(body, []byte("PartOf=nmcp.target\n"), []byte("PartOf=nmcp.target\nWants=evil.service\n"), 1),
		bytes.Replace(body, []byte("[Service]\n"), []byte("[Service]\nExecStartPre=+/bin/true\n"), 1),
		bytes.Replace(body, []byte("[Unit]\n"), []byte("[Unit]\nConditionPathExists=/nonexistent\n"), 1),
		append(append([]byte{}, body...), '\n'),
		bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n")),
		body[:len(body)-1], {}, nil,
	} {
		if validateBackupUnitFragment(changed) == nil {
			t.Fatal("changed fragment body accepted")
		}
	}
}

// Ties the systemd v255 implicit-dependency derivation to the declarations the
// canonical fragment really contains; adding a dependency-relevant directive
// there fails here until the effective contract is re-derived.
func TestBackupUnitDependencyContractMatchesCanonicalFragment(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(pg17Repository(t), "infra/ubuntu/systemd/nmcp-backup.service"))
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]map[string][]string{}
	section := ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "[") {
			section = line
			fields[section] = map[string][]string{}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 || section == "" {
			t.Fatal("unit syntax")
		}
		fields[section][kv[0]] = append(fields[section][kv[0]], kv[1])
	}
	var unitKeys []string
	for k := range fields["[Unit]"] {
		unitKeys = append(unitKeys, k)
	}
	sort.Strings(unitKeys)
	if !reflect.DeepEqual(unitKeys, []string{"After", "Description", "Documentation", "PartOf", "RequiresMountsFor"}) || len(fields) != 2 {
		t.Fatalf("canonical [Unit] directives changed: %q", unitKeys)
	}
	contract, err := backupUnitDependencyContract(backupManager255, backupDependencyTestPG)
	if err != nil {
		t.Fatal(err)
	}
	has := func(set []string, unit string) bool {
		for _, s := range set {
			if s == unit {
				return true
			}
		}
		return false
	}
	if !reflect.DeepEqual(fields["[Unit]"]["After"], []string{"nmcp-migrate.service"}) || !has(contract["After"].required, "nmcp-migrate.service") {
		t.Fatal("fragment After= not in contract")
	}
	if !reflect.DeepEqual(fields["[Unit]"]["PartOf"], contract["PartOf"].required) {
		t.Fatal("fragment PartOf= mismatch")
	}
	if !reflect.DeepEqual(fields["[Unit]"]["RequiresMountsFor"], []string{"/var/lib/nmcp-backup"}) || !has(backupUnitRequiredMountPaths(backupManager255), "/var/lib/nmcp-backup") {
		t.Fatal("fragment RequiresMountsFor= mismatch")
	}
	service := fields["[Service]"]
	for key, want := range map[string]string{"Slice": "nmcp.slice", "PrivateTmp": "yes", "RuntimeDirectory": "nmcp-backup"} {
		if !reflect.DeepEqual(service[key], []string{want}) {
			t.Fatalf("fragment %s changed", key)
		}
	}
	if !has(contract["Requires"].required, "nmcp.slice") || !has(contract["After"].required, "nmcp.slice") {
		t.Fatal("Slice= edges")
	}
	if !has(contract["Wants"].required, "tmp.mount") || !has(contract["After"].required, "tmp.mount") || !has(contract["After"].required, "systemd-tmpfiles-setup.service") || !has(backupUnitRequiredMountPaths(backupManager255), "/var/tmp") {
		t.Fatal("PrivateTmp= edges")
	}
	if !has(backupUnitRequiredMountPaths(backupManager255), "/run/"+service["RuntimeDirectory"][0]) {
		t.Fatal("RuntimeDirectory= mount path")
	}
	if len(service["LoadCredential"]) == 0 || !has(contract["After"].required, `run-credentials-nmcp\x2dbackup.service.mount`) {
		t.Fatal("LoadCredential= credential mount ordering")
	}
	// No directive that would add other implicit edges in v255.
	for _, key := range []string{"StandardOutput", "StandardError", "LogNamespace", "StateDirectory", "CacheDirectory", "LogsDirectory", "WorkingDirectory", "RootDirectory", "RootImage", "MountImages", "ManagedOOMSwap", "ManagedOOMMemoryPressure", "BusName", "Sockets", "DefaultDependencies", "OnFailure", "OnSuccess"} {
		if _, ok := service[key]; ok {
			t.Fatalf("fragment declares %s; re-derive implicit dependencies", key)
		}
	}
}

func TestBackupUnitIdentityDriftComparesSetsByMembershipOnly(t *testing.T) {
	base := map[string]json.RawMessage{
		"After":             json.RawMessage(`["a.service","b.service","c.mount"]`),
		"RequiresMountsFor": json.RawMessage(`["/var/tmp","/run/nmcp-backup"]`),
		"DropInPaths":       json.RawMessage(`["/x/10.conf","/x/artifact.conf"]`),
		"FragmentPath":      json.RawMessage(`"/etc/systemd/system/nmcp-backup.service"`),
	}
	clone := func() map[string]json.RawMessage {
		out := map[string]json.RawMessage{}
		for k, v := range base {
			out[k] = v
		}
		return out
	}
	// The manager reordered both sets after collecting and reloading the unit.
	reordered := clone()
	reordered["After"] = json.RawMessage(`["c.mount","a.service","b.service"]`)
	reordered["RequiresMountsFor"] = json.RawMessage(`["/run/nmcp-backup","/var/tmp"]`)
	if drift := backupUnitIdentityDrift(base, reordered); len(drift) != 0 {
		t.Fatalf("reordered unordered sets reported as drift: %v", drift)
	}
	for _, c := range []struct {
		name, value string
	}{
		{"After", `["a.service","b.service"]`},                       // removed member
		{"After", `["a.service","b.service","c.mount","d.service"]`}, // added member
		{"After", `["a.service","b.service","c.mount","c.mount"]`},   // duplicate is not a set match
		{"After", `"a.service"`},                                     // wrong shape never matches
		{"RequiresMountsFor", `["/var/tmp","/other"]`},               // replaced member
		{"DropInPaths", `["/x/artifact.conf","/x/10.conf"]`},         // not a set property: order is exact
		{"FragmentPath", `"/etc/systemd/system/other.service"`},
	} {
		changed := clone()
		changed[c.name] = json.RawMessage(c.value)
		if drift := backupUnitIdentityDrift(base, changed); !reflect.DeepEqual(drift, []string{c.name}) {
			t.Fatalf("%s=%s drift = %v", c.name, c.value, drift)
		}
	}
	missing := clone()
	delete(missing, "After")
	if drift := backupUnitIdentityDrift(base, missing); !reflect.DeepEqual(drift, []string{"After"}) {
		t.Fatalf("missing property drift = %v", drift)
	}
	if !backupUnitIsSetProperty("TriggeredBy") || backupUnitIsSetProperty("Conditions") || backupUnitIsSetProperty("DropInPaths") {
		t.Fatal("set-property classification")
	}
}

func TestBackupUnitDependencyDiagnosticNamesRejection(t *testing.T) {
	p := backupDependencyFixture(t, nil, false)
	inject := backupDependencyWith(t, p, "Wants", `evil\x2dunit.service`)
	err := validateBackupUnitDependencies(inject, backupDependencyTestPG, backupManager255)
	if !errors.Is(err, errCutover) || !strings.Contains(err.Error(), `backup unit Wants (unexpected evil\x2dunit.service) contract`) {
		t.Fatalf("diagnostic = %v", err)
	}
	secret := backupDependencyWith(t, p, "Wants", "password=CANARY-secret value")
	err = validateBackupUnitDependencies(secret, backupDependencyTestPG, backupManager255)
	if !errors.Is(err, errCutover) || strings.Contains(err.Error(), "CANARY") || !strings.Contains(err.Error(), "<invalid>") {
		t.Fatalf("unsanitized diagnostic = %v", err)
	}
	missing := map[string]json.RawMessage{}
	for k, v := range p {
		missing[k] = v
	}
	missing["Conflicts"] = json.RawMessage(`[]`)
	err = validateBackupUnitDependencies(missing, backupDependencyTestPG, backupManager255)
	if !errors.Is(err, errCutover) || !strings.Contains(err.Error(), "missing shutdown.target") {
		t.Fatalf("missing diagnostic = %v", err)
	}
}
