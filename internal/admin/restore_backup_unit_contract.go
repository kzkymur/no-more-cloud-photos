package admin

// The supported Ubuntu/systemd manager, not rendered command strings, supplies
// typed effective properties. Fixed LoadCredential sources authenticate
// administrative provenance; install itself does not authenticate its source.
import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
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
const backupArtifactDropinBody = "[Service]\nBindReadOnlyPaths=/opt/nmcp-native-ci /opt/nmcp-pg17-ci\n"

func backupUnitIdentityProperties() []backupUnitProperty {
	return []backupUnitProperty{
		{"Id", "s", "nmcp-backup.service"}, {"LoadState", "s", "loaded"},
		{"FragmentPath", "s", backupUnitFragment}, {"Transient", "b", false},
		{"NeedDaemonReload", "b", false}, {"DropInPaths", "as", nil},
		{"OnFailure", "as", []string{}}, {"OnSuccess", "as", []string{}}, {"PropagatesStopTo", "as", []string{}},
	}
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

func validateLoadedBackupUnit(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	before, err := queryBackupUnitProperties(ctx, "Unit", backupUnitIdentityProperties())
	if err != nil {
		return err
	}
	var drops []string
	if !backupUnitArray(before["DropInPaths"], &drops) || (len(drops) != 0 && !reflect.DeepEqual(drops, []string{backupArtifactDropin})) {
		return errCutover
	}
	fragment, err := safeCutoverFile(backupUnitFragment, false, true)
	if err != nil {
		return err
	}
	var drop cutoverFile
	if len(drops) == 1 {
		drop, err = safeCutoverFile(backupArtifactDropin, false, true)
		if err != nil || string(drop.before) != backupArtifactDropinBody {
			return errCutover
		}
	}
	props, err := queryBackupUnitProperties(ctx, "Service", backupUnitServiceProperties())
	if err != nil {
		return err
	}
	if err = validateBackupUnitEffective(props, len(drops) == 1); err != nil {
		return err
	}
	after, err := queryBackupUnitProperties(ctx, "Unit", backupUnitIdentityProperties())
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, after) {
		return errCutover
	}
	again, err := safeCutoverFile(backupUnitFragment, false, true)
	if err != nil || !sameBackupUnitFile(fragment, again) {
		return errCutover
	}
	if len(drops) == 1 {
		again, err = safeCutoverFile(backupArtifactDropin, false, true)
		if err != nil || !sameBackupUnitFile(drop, again) {
			return errCutover
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
