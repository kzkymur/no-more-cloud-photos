package admin

// The supported Ubuntu/systemd manager, not rendered command strings, supplies
// typed effective properties. Fixed LoadCredential sources authenticate
// administrative provenance; install itself does not authenticate its source.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

type backupUnitProperty struct {
	name, signature string
	want            any
}

const backupUnitFragment = "/etc/systemd/system/nmcp-backup.service"
const backupArtifactDropin = backupUnitFragment + ".d/artifact-paths.conf"
const backupPGDependencyDropin = backupUnitFragment + ".d/10-postgresql.conf"
const migratePGDependencyDropin = "/etc/systemd/system/nmcp-migrate.service.d/10-postgresql.conf"

var backupPGUnitPattern = regexp.MustCompile(`^postgresql@17-([A-Za-z0-9_][A-Za-z0-9_-]{0,62})\.service$`)
var backupPGHBAPathPattern = regexp.MustCompile(`^/etc/postgresql/17/([A-Za-z0-9_][A-Za-z0-9_-]{0,62})/pg_hba\.conf$`)

const backupArtifactDropinBody = "[Service]\nBindReadOnlyPaths=/opt/nmcp-native-ci /opt/nmcp-pg17-ci\n"

// SHA-256 of the exact canonical infra/ubuntu/systemd/nmcp-backup.service
// bytes, which install-ubuntu-host and the root fixtures install verbatim. This
// is an additional guard to the root-owned metadata walk and typed properties.
const backupUnitFragmentSHA256 = "c15204d42d996bec8f7011d2181fc18e25501bf85beb9ad11d56286e0d2e22b4"

// Every activation, ordering, propagation and namespace edge exposed by the
// systemd v255 org.freedesktop.systemd1.Unit interface (dbus-unit.c), plus the
// reverse lists, so an edge injected from any unit file is visible here.
var backupUnitEdgeProperties = []string{
	"Requires", "Requisite", "Wants", "BindsTo", "PartOf", "Upholds",
	"RequiredBy", "RequisiteOf", "WantedBy", "BoundBy", "UpheldBy", "ConsistsOf",
	"Conflicts", "ConflictedBy", "Before", "After",
	"OnSuccess", "OnSuccessOf", "OnFailure", "OnFailureOf", "Triggers", "TriggeredBy",
	"PropagatesReloadTo", "ReloadPropagatedFrom", "PropagatesStopTo", "StopPropagatedFrom", "JoinsNamespaceOf",
}

// The canonical unit's RequiresMountsFor set in systemd v255: the explicit
// RequiresMountsFor=/var/lib/nmcp-backup, /var/tmp from PrivateTmp=yes and
// /run/nmcp-backup from RuntimeDirectory= (unit_add_exec_dependencies).
var backupUnitMountPaths = []string{"/var/lib/nmcp-backup", "/var/tmp", "/run/nmcp-backup"}

func backupUnitIdentityProperties() []backupUnitProperty {
	p := []backupUnitProperty{
		{"Id", "s", "nmcp-backup.service"}, {"LoadState", "s", "loaded"},
		{"FragmentPath", "s", backupUnitFragment}, {"SourcePath", "s", ""}, {"Transient", "b", false},
		{"NeedDaemonReload", "b", false}, {"DropInPaths", "as", nil}, {"DefaultDependencies", "b", true},
		{"RequiresMountsFor", "as", nil},
		{"Conditions", "a(sbbsi)", []any{}}, {"Asserts", "a(sbbsi)", []any{}},
	}
	for _, name := range backupUnitEdgeProperties {
		// Edges whose canonical set is empty are also rejected at the wire layer.
		var want any = []string{}
		switch name {
		case "Requires", "Wants", "After", "Before", "Conflicts", "PartOf", "TriggeredBy":
			want = nil
		}
		p = append(p, backupUnitProperty{name, "as", want})
	}
	return p
}

// unit_name_from_path(path, ".mount") for the fixed ASCII paths above:
// path_simplify, "/" -> "-", root -> "-", unit_name_escape for everything
// outside [A-Za-z0-9:_.] (and a leading ".").
func backupMountUnitName(path string) string {
	p := strings.Trim(path, "/")
	if p == "" {
		return "-.mount"
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ':' || c == '_' || c == '.' && i > 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String() + ".mount"
}

// Mount units for every prefix (PATH_FOREACH_PREFIX_MORE) of every required
// path. systemd v255 unit_add_mount_dependencies/mount_add_mount_dependencies
// add After= only for a loaded mount unit and Requires= only when it also has
// a fragment, so these are host-dependent: exact names, any subset.
func backupUnitPathMounts() []string {
	seen := map[string]bool{}
	var out []string
	for _, path := range backupUnitMountPaths {
		for prefix := path; ; prefix = filepath.Dir(prefix) {
			name := backupMountUnitName(prefix)
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
			if prefix == "/" {
				break
			}
		}
	}
	return out
}

type backupUnitEdges struct{ required, optional []string }

// Exact allow-sets derived from the canonical fragment, its 10-postgresql.conf
// drop-in and systemd v255 implicit dependencies (see the report rationale):
//   - service_add_default_dependencies: Requires/After sysinit.target,
//     After basic.target, Before/Conflicts shutdown.target;
//   - unit_add_slice_dependencies: Requires/After Slice=nmcp.slice;
//   - unit_add_exec_dependencies: PrivateTmp Wants/After tmp.mount and After
//     systemd-tmpfiles-setup.service; journal stdout After
//     systemd-journald.socket; LoadCredential= After the credential mount;
//   - fragment: After=nmcp-migrate.service, PartOf=nmcp.target;
//   - nmcp-backup.timer Unit= adds Before+Triggers on the timer, i.e. After
//     and TriggeredBy here whenever the timer is loaded.
func backupUnitDependencyContract(pgUnit string) (map[string]backupUnitEdges, error) {
	if !backupPGUnitPattern.MatchString(pgUnit) {
		return nil, errCutover
	}
	mounts := func(extra ...string) []string { return append(backupUnitPathMounts(), extra...) }
	c := map[string]backupUnitEdges{}
	for _, name := range backupUnitEdgeProperties {
		c[name] = backupUnitEdges{}
	}
	c["Requires"] = backupUnitEdges{[]string{"sysinit.target", "nmcp.slice", pgUnit}, mounts()}
	c["Wants"] = backupUnitEdges{[]string{"tmp.mount"}, nil}
	c["After"] = backupUnitEdges{[]string{"sysinit.target", "basic.target", "nmcp.slice", "nmcp-migrate.service", pgUnit, "tmp.mount", "systemd-tmpfiles-setup.service", "systemd-journald.socket", `run-credentials-nmcp\x2dbackup.service.mount`}, mounts("nmcp-backup.timer")}
	c["Before"] = backupUnitEdges{[]string{"shutdown.target"}, nil}
	c["Conflicts"] = backupUnitEdges{[]string{"shutdown.target"}, nil}
	c["PartOf"] = backupUnitEdges{[]string{"nmcp.target"}, nil}
	c["TriggeredBy"] = backupUnitEdges{nil, []string{"nmcp-backup.timer"}}
	return c, nil
}

// backupUnitDiagnosticName keeps operator/test diagnostics to systemd unit-name
// characters; these lists carry unit names only, never settings or credentials.
func backupUnitDiagnosticName(name string) string {
	if name == "" || len(name) > 128 {
		return "<invalid>"
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(`:_.@\-`, c)) {
			return "<invalid>"
		}
	}
	return name
}

// backupUnitGuardError labels a fixed guard step. Callers on the cutover path
// still discard it and report only errCutover.
func backupUnitGuardError(step string) error {
	return fmt.Errorf("backup unit %s: %w", step, errCutover)
}

// The D-Bus lists below are unordered sets: dbus-unit.c emits them by iterating
// a hashmap keyed by Unit pointer, and an unreferenced inactive unit may be
// collected and reloaded between two reads. Compare membership, exactly.
func backupUnitIsSetProperty(name string) bool {
	if name == "RequiresMountsFor" {
		return true
	}
	for _, edge := range backupUnitEdgeProperties {
		if edge == name {
			return true
		}
	}
	return false
}
func backupUnitPropertyStable(name string, a, b json.RawMessage) bool {
	if !backupUnitIsSetProperty(name) {
		return bytes.Equal(a, b)
	}
	var x, y []string
	if !backupUnitArray(a, &x) || !backupUnitArray(b, &y) {
		return false
	}
	sort.Strings(x)
	sort.Strings(y)
	return reflect.DeepEqual(x, y)
}

// backupUnitIdentityDrift returns the sorted names of properties whose value
// changed. Every other property, and every set's membership, remains exact.
func backupUnitIdentityDrift(before, after map[string]json.RawMessage) []string {
	names := map[string]bool{}
	for name := range before {
		names[name] = true
	}
	for name := range after {
		names[name] = true
	}
	var drift []string
	for name := range names {
		a, okA := before[name]
		b, okB := after[name]
		if !okA || !okB || !backupUnitPropertyStable(name, a, b) {
			drift = append(drift, name)
		}
	}
	sort.Strings(drift)
	return drift
}
func backupUnitEdgeSet(raw json.RawMessage, e backupUnitEdges) (map[string]bool, string) {
	var got []string
	if !backupUnitArray(raw, &got) {
		return nil, "malformed list"
	}
	allowed := map[string]bool{}
	for _, s := range e.optional {
		allowed[s] = true
	}
	for _, s := range e.required {
		allowed[s] = true
	}
	seen := map[string]bool{}
	for _, s := range got {
		switch {
		case s == "":
			return nil, "empty name"
		case seen[s]:
			return nil, "duplicate " + backupUnitDiagnosticName(s)
		case !allowed[s]:
			return nil, "unexpected " + backupUnitDiagnosticName(s)
		}
		seen[s] = true
	}
	for _, s := range e.required {
		if !seen[s] {
			return nil, "missing " + backupUnitDiagnosticName(s)
		}
	}
	return seen, ""
}

// validateBackupUnitDependencies is the backup-only effective edge contract.
// The loose migrate/offline helper validateBackupUnitDependencySet is unchanged.
func validateBackupUnitDependencies(p map[string]json.RawMessage, pgUnit string) error {
	bad := func(name string) error { return fmt.Errorf("backup unit %s contract: %w", name, errCutover) }
	contract, err := backupUnitDependencyContract(pgUnit)
	if err != nil {
		return bad("dependency authority")
	}
	sets := map[string]map[string]bool{}
	for _, name := range backupUnitEdgeProperties {
		set, reason := backupUnitEdgeSet(p[name], contract[name])
		if reason != "" {
			return bad(name + " (" + reason + ")")
		}
		sets[name] = set
	}
	// A required mount is always also ordered; the timer adds both edges at once.
	for unit := range sets["Requires"] {
		if !sets["After"][unit] {
			return bad("Requires")
		}
	}
	if sets["After"]["nmcp-backup.timer"] != sets["TriggeredBy"]["nmcp-backup.timer"] {
		return bad("TriggeredBy")
	}
	if !backupUnitStringSet(p["RequiresMountsFor"], backupUnitMountPaths) {
		return bad("RequiresMountsFor")
	}
	for _, name := range []string{"Conditions", "Asserts"} {
		if !equalBackupUnitJSON(p[name], []any{}) {
			return bad(name)
		}
	}
	if !equalBackupUnitJSON(p["DefaultDependencies"], true) {
		return bad("DefaultDependencies")
	}
	return nil
}
func validateBackupUnitFragment(body []byte) error {
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != backupUnitFragmentSHA256 {
		return fmt.Errorf("backup unit fragment content: %w", errCutover)
	}
	return nil
}
func backupUnitServiceProperties() []backupUnitProperty {
	p := []backupUnitProperty{
		{"Type", "s", "oneshot"}, {"RemainAfterExit", "b", false}, {"KillMode", "s", "control-group"},
		{"Restart", "s", "no"}, {"SendSIGKILL", "b", true}, {"KillSignal", "i", 15}, {"FinalKillSignal", "i", 9},
		{"TimeoutStartUSec", "t", uint64(21600000000)}, {"TimeoutStopUSec", "t", uint64(60000000)},
		{"SuccessExitStatus", "(aiai)", []any{[]int{4}, []int{}}},
		{"RestartForceExitStatus", "(aiai)", []any{[]int{}, []int{}}},
		{"SystemCallArchitectures", "as", []string{"native"}},
		{"User", "s", "nmcp-backup"}, {"Group", "s", "nmcp-backup"},
		{"SupplementaryGroups", "as", []string{}}, {"DynamicUser", "b", false},
		// This deprecated property is hidden from GetAll; explicitly query it.
		{"PermissionsStartOnly", "b", false}, {"RootDirectoryStartOnly", "b", false},
		{"RuntimeDirectory", "as", []string{"nmcp-backup"}}, {"RuntimeDirectoryMode", "u", 448},
		{"RuntimeDirectoryPreserve", "s", "no"}, {"RuntimeDirectorySymlink", "a(sst)", []any{}},
		{"UMask", "u", 63}, {"Environment", "as", nil}, {"LoadCredential", "a(ss)", nil},
		{"EnvironmentFiles", "a(sb)", []any{[]any{"/etc/nmcp/backup.env", false}}},
		{"PassEnvironment", "as", []string{}}, {"UnsetEnvironment", "as", []string{}},
		{"LoadCredentialEncrypted", "a(ss)", []any{}}, {"SetCredential", "a(say)", []any{}},
		{"SetCredentialEncrypted", "a(say)", []any{}}, {"ImportCredential", "as", []string{}},
		{"CapabilityBoundingSet", "t", 0}, {"AmbientCapabilities", "t", 0},
		{"ProtectHome", "s", "yes"}, {"ProtectSystem", "s", "strict"},
		{"ProtectProc", "s", "invisible"}, {"ProcSubset", "s", "pid"},
		{"RootDirectory", "s", ""}, {"RootImage", "s", ""}, {"RootEphemeral", "b", false},
		{"ReadWritePaths", "as", []string{"/var/lib/nmcp-backup"}},
		{"BindPaths", "a(ssbt)", []any{}}, {"BindReadOnlyPaths", "a(ssbt)", nil},
		{"TemporaryFileSystem", "a(ss)", []any{}}, {"MountImages", "a(ssba(ss))", []any{}},
		{"ExtensionImages", "a(sba(ss))", []any{}}, {"ExtensionDirectories", "as", []string{}},
		{"RestrictAddressFamilies", "(bas)", nil},
	}
	for _, name := range []string{"NoNewPrivileges", "PrivateTmp", "PrivateDevices", "ProtectKernelTunables", "ProtectKernelModules", "ProtectControlGroups", "ProtectClock", "ProtectHostname", "RestrictSUIDSGID", "LockPersonality", "MemoryDenyWriteExecute"} {
		p = append(p, backupUnitProperty{name, "b", true})
	}
	for _, name := range []string{"ExecConditionEx", "ExecStartPreEx", "ExecStartEx", "ExecStartPostEx", "ExecReloadEx", "ExecStopEx", "ExecStopPostEx"} {
		p = append(p, backupUnitProperty{name, "a(sasasttttuii)", nil})
	}
	return p
}
func backupUnitArray(raw json.RawMessage, out any) bool {
	return strings.HasPrefix(strings.TrimSpace(string(raw)), "[") && json.Unmarshal(raw, out) == nil
}
func backupUnitStringSet(raw json.RawMessage, want []string) bool {
	var got []string
	if !backupUnitArray(raw, &got) || len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, s := range got {
		if seen[s] {
			return false
		}
		seen[s] = true
	}
	for _, s := range want {
		if !seen[s] {
			return false
		}
	}
	return true
}
func validateBackupUnitEffective(p map[string]json.RawMessage, artifactBind bool) error {
	bad := func(name string) error { return fmt.Errorf("backup unit %s contract: %w", name, errCutover) }
	// Validate every fixed value here too, so parser-independent policy tests
	// cannot accidentally omit a safety property.
	for _, spec := range backupUnitServiceProperties() {
		raw, ok := p[spec.name]
		if !ok || (spec.want != nil && !equalBackupUnitJSON(raw, spec.want)) {
			return bad(spec.name)
		}
	}
	var env []string
	var creds [][]string
	if !backupUnitArray(p["Environment"], &env) || !backupUnitArray(p["LoadCredential"], &creds) {
		return bad("credentials")
	}
	// Keep D-Bus element boundaries: flattening permits whitespace inside one
	// value to masquerade as an additional credential or environment entry.
	if validateCutoverBackupUnit(env, creds) != nil {
		return bad("credentials")
	}
	commands := map[string][][]string{
		"ExecStartPreEx": {
			{"/usr/bin/install", "-T", "-m", "0600", "/run/credentials/nmcp-backup.service/control.pgpass", "/run/nmcp-backup/control.pgpass"},
			{"/usr/bin/install", "-T", "-m", "0600", "/run/credentials/nmcp-backup.service/dump.pgpass", "/run/nmcp-backup/dump.pgpass"},
		},
		"ExecStartEx": {{"/opt/nmcp/current/bin/nmcp-admin", "backup", "run", "--if-due", "--json"}},
	}
	for _, name := range []string{"ExecConditionEx", "ExecStartPreEx", "ExecStartEx", "ExecStartPostEx", "ExecReloadEx", "ExecStopEx", "ExecStopPostEx"} {
		if !validateBackupUnitCommands(p[name], commands[name]) {
			return bad(name)
		}
	}
	var families []json.RawMessage
	if !backupUnitArray(p["RestrictAddressFamilies"], &families) || len(families) != 2 || !equalBackupUnitJSON(families[0], true) || !backupUnitStringSet(families[1], []string{"AF_UNIX", "AF_INET", "AF_INET6"}) {
		return bad("RestrictAddressFamilies")
	}
	if !artifactBind {
		if !equalBackupUnitJSON(p["BindReadOnlyPaths"], []any{}) {
			return bad("BindReadOnlyPaths")
		}
	} else {
		var binds []json.RawMessage
		if !backupUnitArray(p["BindReadOnlyPaths"], &binds) || len(binds) != 2 {
			return bad("BindReadOnlyPaths")
		}
		seen := map[string]bool{}
		for _, raw := range binds {
			found := ""
			for _, path := range []string{"/opt/nmcp-native-ci", "/opt/nmcp-pg17-ci"} {
				if equalBackupUnitJSON(raw, []any{path, path, false, 16384}) {
					found = path
				}
			}
			if found == "" || seen[found] {
				return bad("BindReadOnlyPaths")
			}
			seen[found] = true
		}
	}
	return nil
}

// Authority is supplied by the authenticated source cluster or externally
// attested offline receipt. Never learn an allowed PG unit from a drop-in.
func backupPGDependencyBody(expected string) (string, error) {
	if !backupPGUnitPattern.MatchString(expected) {
		return "", errCutover
	}
	return "[Unit]\nRequires=" + expected + "\nAfter=" + expected + "\n", nil
}
func backupPGUnitFromHBA(path string) (string, error) {
	match := backupPGHBAPathPattern.FindStringSubmatch(path)
	if match == nil {
		return "", errCutover
	}
	return "postgresql@17-" + match[1] + ".service", nil
}
func validateBackupUnitDependencySet(raw json.RawMessage, expected string) bool {
	if !backupPGUnitPattern.MatchString(expected) {
		return false
	}
	var dependencies []string
	if !backupUnitArray(raw, &dependencies) {
		return false
	}
	found := false
	seen := map[string]bool{}
	for _, unit := range dependencies {
		if unit == "" || seen[unit] {
			return false
		}
		seen[unit] = true
		if unit == expected {
			found = true
		} else if strings.HasPrefix(unit, "postgresql") {
			return false
		}
	}
	return found
}
func backupUnitDropins(raw json.RawMessage) (bool, error) {
	if backupUnitStringSet(raw, []string{backupPGDependencyDropin}) {
		return false, nil
	}
	if backupUnitStringSet(raw, []string{backupPGDependencyDropin, backupArtifactDropin}) {
		return true, nil
	}
	return false, errCutover
}
func validateBackupPGDependencyBodies(backup, migrate []byte, expected string) error {
	body, err := backupPGDependencyBody(expected)
	if err != nil || string(backup) != body || string(migrate) != body {
		return errCutover
	}
	return nil
}

// The effective-unit allow-sets above are derived from systemd v255 semantics
// (Ubuntu 24.04, the only supported production manager). Any other manager
// version fails closed rather than being validated against the wrong rules.
var backupManagerVersionPattern = regexp.MustCompile(`^255(\.[0-9]{1,4})?([-~+][A-Za-z0-9.+~-]{1,64})?$`)

func supportedBackupManagerVersion(version string) bool {
	return backupManagerVersionPattern.MatchString(version)
}

var backupManagerVersion = func(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/busctl", "--system", "--json=short", "--auto-start=no", "get-property",
		"org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "Version")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	var capture backupUnitWireCapture
	cmd.Stdout = &capture
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil || capture.overflow {
		return "", backupUnitGuardError("manager version query")
	}
	var reply struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	if json.Unmarshal(capture.data, &reply) != nil || reply.Type != "s" {
		return "", backupUnitGuardError("manager version reply")
	}
	return reply.Data, nil
}

func validateLoadedBackupUnit(parent context.Context, expectedPGUnit string) error {
	if _, err := backupPGDependencyBody(expectedPGUnit); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	version, err := backupManagerVersion(ctx)
	if err != nil {
		return err
	}
	if !supportedBackupManagerVersion(version) {
		return backupUnitGuardError("systemd manager version unsupported (" + backupUnitDiagnosticName(version) + "; supported: 255)")
	}
	before, err := queryBackupUnitProperties(ctx, "Unit", backupUnitIdentityProperties())
	if err != nil {
		return err
	}
	artifactBind, err := backupUnitDropins(before["DropInPaths"])
	if err != nil {
		return backupUnitGuardError("drop-in set")
	}
	if err = validateBackupUnitDependencies(before, expectedPGUnit); err != nil {
		return err
	}
	snapshots := []cutoverFile{}
	for _, path := range []string{backupUnitFragment, backupPGDependencyDropin, migratePGDependencyDropin} {
		file, err := safeCutoverFile(path, false, true)
		if err != nil {
			return backupUnitGuardError("file metadata " + filepath.Base(filepath.Dir(path)) + "/" + filepath.Base(path))
		}
		snapshots = append(snapshots, file)
	}
	if err = validateBackupUnitFragment(snapshots[0].before); err != nil {
		return err
	}
	if validateBackupPGDependencyBodies(snapshots[1].before, snapshots[2].before, expectedPGUnit) != nil {
		return backupUnitGuardError("PostgreSQL dependency body")
	}
	if artifactBind {
		file, err := safeCutoverFile(backupArtifactDropin, false, true)
		if err != nil {
			return backupUnitGuardError("artifact drop-in metadata")
		}
		if string(file.before) != backupArtifactDropinBody {
			return backupUnitGuardError("artifact drop-in body")
		}
		snapshots = append(snapshots, file)
	}
	props, err := queryBackupUnitProperties(ctx, "Service", backupUnitServiceProperties())
	if err != nil {
		return err
	}
	if err = validateBackupUnitEffective(props, artifactBind); err != nil {
		return err
	}
	after, err := queryBackupUnitProperties(ctx, "Unit", backupUnitIdentityProperties())
	if err != nil {
		return err
	}
	if drift := backupUnitIdentityDrift(before, after); len(drift) != 0 {
		return backupUnitGuardError("identity changed during validation: " + strings.Join(drift, ","))
	}
	for _, file := range snapshots {
		again, err := safeCutoverFile(file.path, false, true)
		if err != nil || !sameBackupUnitFile(file, again) {
			return backupUnitGuardError("file changed during validation " + filepath.Base(file.path))
		}
	}
	return nil
}
func sameBackupUnitFile(a, b cutoverFile) bool {
	// safeCutoverFile already verifies root-owned non-symlink ancestors, regular
	// 0644 single-link leaves. Recheck ownership/mode/content as well.
	return reflect.DeepEqual(a, b)
}

// EnvironmentFile overrides Environment, so accepting only the manager's
// Environment property is insufficient: reject all credential-routing and
// execution-environment overrides in this root-private file too.
func validateCutoverBackupEnvironment(data []byte) error {
	required := map[string]string{"NMCP_DATABASE_URL": "service=nmcp_backup_control", "NMCP_BACKUP_DUMP_SERVICE": "nmcp_backup_dump", "NMCP_BACKUP_ROOT": "/var/lib/nmcp-backup"}
	optional := map[string]bool{"NMCP_BACKUP_PG_DUMP": true, "NMCP_BACKUP_PG_RESTORE": true, "NMCP_BACKUP_NOTIFY_HOOK": true, "NMCP_LOG_LEVEL": true, "NMCP_SHUTDOWN_TIMEOUT": true}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 || seen[kv[0]] || kv[1] == "" {
			return errCutover
		}
		key, value := kv[0], kv[1]
		seen[key] = true
		if strings.ContainsAny(value, "'\"\\$%\x00") || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
			return errCutover
		}
		if want, ok := required[key]; ok {
			if value != want {
				return errCutover
			}
		} else if !optional[key] {
			return errCutover
		}
		if key == "NMCP_BACKUP_PG_DUMP" || key == "NMCP_BACKUP_PG_RESTORE" || key == "NMCP_BACKUP_NOTIFY_HOOK" {
			if !filepath.IsAbs(value) || filepath.Clean(value) != value {
				return errCutover
			}
		}
	}
	for key := range required {
		if !seen[key] {
			return errCutover
		}
	}
	return nil
}
