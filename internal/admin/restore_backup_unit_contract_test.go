package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func backupContractJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func backupContractCommand(t *testing.T, args []string) any {
	return []any{args[0], args, []string{}, uint64(123), uint64(456), uint64(789), uint64(800), uint32(42), int32(1), int32(0)}
}
func backupContractFixture(t *testing.T) map[string]json.RawMessage {
	p := map[string]json.RawMessage{}
	for _, spec := range backupUnitServiceProperties() {
		if spec.want != nil {
			p[spec.name] = backupContractJSON(t, spec.want)
		} else {
			p[spec.name] = json.RawMessage(`[]`)
		}
	}
	p["Environment"] = backupContractJSON(t, []string{"PGPASSFILE=/run/nmcp-backup/control.pgpass", "PGSERVICEFILE=%d/pg_service.conf", "NMCP_BACKUP_DUMP_PASSFILE=/run/nmcp-backup/dump.pgpass"})
	p["LoadCredential"] = backupContractJSON(t, [][]string{{"dump.pgpass", "/etc/nmcp/credentials/backup-dump.pgpass"}, {"control.pgpass", "/etc/nmcp/credentials/backup-control.pgpass"}, {"pg_service.conf", "/etc/nmcp/backup.pg_service.conf"}})
	p["ExecStartEx"] = backupContractJSON(t, []any{backupContractCommand(t, []string{"/opt/nmcp/current/bin/nmcp-admin", "backup", "run", "--if-due", "--json"})})
	p["ExecStartPreEx"] = backupContractJSON(t, []any{backupContractCommand(t, []string{"/usr/bin/install", "-T", "-m", "0600", "%d/control.pgpass", "/run/nmcp-backup/control.pgpass"}), backupContractCommand(t, []string{"/usr/bin/install", "-T", "-m", "0600", "%d/dump.pgpass", "/run/nmcp-backup/dump.pgpass"})})
	p["RestrictAddressFamilies"] = backupContractJSON(t, []any{true, []string{"AF_UNIX", "AF_INET6", "AF_INET"}})
	return p
}
func TestBackupUnitEffectiveContract(t *testing.T) {
	p := backupContractFixture(t)
	if e := validateBackupUnitEffective(p, false); e != nil {
		t.Fatal(e)
	}
	// Every required safety property must be present and non-null.
	for _, spec := range backupUnitServiceProperties() {
		t.Run("missing-"+spec.name, func(t *testing.T) {
			q := backupContractFixture(t)
			delete(q, spec.name)
			if validateBackupUnitEffective(q, false) == nil {
				t.Fatal("missing property accepted")
			}
		})
		t.Run("null-"+spec.name, func(t *testing.T) {
			q := backupContractFixture(t)
			q[spec.name] = json.RawMessage(`null`)
			if validateBackupUnitEffective(q, false) == nil {
				t.Fatal("null property accepted")
			}
		})
	}
	for _, c := range []struct {
		name  string
		value any
	}{
		{"RuntimeDirectoryMode", 0755}, {"RuntimeDirectoryPreserve", "yes"}, {"RuntimeDirectory", []string{"other"}},
		{"PermissionsStartOnly", true}, {"User", "root"}, {"Group", "root"}, {"CapabilityBoundingSet", 1},
		{"RootDirectory", "/other"}, {"PassEnvironment", []string{"PGPASSWORD"}},
		{"SetCredential", []any{[]any{"dump.pgpass", []int{1}}}},
		{"ReadWritePaths", []string{"/var/lib/nmcp-backup", "/run"}},
		{"EnvironmentFiles", []any{[]any{"/etc/nmcp/backup.env", true}}},
		{"Restart", "on-failure"}, {"SendSIGKILL", false}, {"TimeoutStopUSec", 0}, {"SuccessExitStatus", []any{[]int{1, 4}, []int{}}},
		{"SystemCallArchitectures", []string{}},
		{"RestrictAddressFamilies", []any{false, []string{"AF_UNIX", "AF_INET", "AF_INET6"}}},
		{"ExecConditionEx", []any{backupContractCommand(t, []string{"/bin/sh"})}},
		{"ExecStopEx", []any{backupContractCommand(t, []string{"/usr/bin/true"})}},
	} {
		t.Run("unsafe-"+c.name, func(t *testing.T) {
			q := backupContractFixture(t)
			q[c.name] = backupContractJSON(t, c.value)
			if validateBackupUnitEffective(q, false) == nil {
				t.Fatal("unsafe property accepted")
			}
		})
	}
	binds := []any{[]any{"/opt/nmcp-native-ci", "/opt/nmcp-native-ci", false, 16384}, []any{"/opt/nmcp-pg17-ci", "/opt/nmcp-pg17-ci", false, 16384}}
	p["BindReadOnlyPaths"] = backupContractJSON(t, binds)
	if validateBackupUnitEffective(p, false) == nil || validateBackupUnitEffective(p, true) != nil {
		t.Fatal("exact independently-attested read-only bind contract")
	}
	for _, bad := range []any{[]any{}, binds[:1], []any{binds[0], binds[0]}, []any{[]any{"/opt/nmcp-native-ci", "/run/nmcp-backup", false, 16384}, binds[1]}, []any{[]any{"/opt/nmcp-native-ci", "/opt/nmcp-native-ci", true, 16384}, binds[1]}, []any{[]any{"/opt/nmcp-native-ci", "/opt/nmcp-native-ci", false, 0}, binds[1]}, []any{[]any{"/opt/nmcp-native-ci", "/opt/nmcp-native-ci", false, 16385}, binds[1]}} {
		p["BindReadOnlyPaths"] = backupContractJSON(t, bad)
		if validateBackupUnitEffective(p, true) == nil {
			t.Fatal("unattested/remapped/weak/duplicate bind accepted")
		}
	}
}

func TestBackupUnitProductionTemplateContract(t *testing.T) {
	// Actual shipped declarations, not a second hard-coded old credential env,
	// must pass the policy used before cutover. This is NOT a manager simulation.
	body, e := os.ReadFile(filepath.Join(pg17Repository(t), "infra/ubuntu/systemd/nmcp-backup.service"))
	if e != nil {
		t.Fatal(e)
	}
	fields := map[string][]string{}
	section := ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section != "[Service]" || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			t.Fatal("unit syntax")
		}
		fields[kv[0]] = append(fields[kv[0]], kv[1])
	}
	p := backupContractFixture(t)
	p["Environment"] = backupContractJSON(t, fields["Environment"])
	var creds [][]string
	for _, value := range fields["LoadCredential"] {
		creds = append(creds, strings.SplitN(value, ":", 2))
	}
	p["LoadCredential"] = backupContractJSON(t, creds)
	for _, pair := range [][2]string{{"ExecStartPre", "ExecStartPreEx"}, {"ExecStart", "ExecStartEx"}} {
		var rows []any
		for _, value := range fields[pair[0]] {
			rows = append(rows, backupContractCommand(t, strings.Fields(value)))
		}
		p[pair[1]] = backupContractJSON(t, rows)
	}
	for _, spec := range backupUnitServiceProperties() {
		values, ok := fields[spec.name]
		if !ok || len(values) != 1 {
			continue
		}
		value := values[0]
		switch spec.want.(type) {
		case string:
			p[spec.name] = backupContractJSON(t, value)
		case bool:
			p[spec.name] = backupContractJSON(t, value == "yes")
		case []string:
			p[spec.name] = backupContractJSON(t, strings.Fields(value))
		}
	}
	for _, key := range []string{"UMask", "RuntimeDirectoryMode"} {
		n, e := strconv.ParseUint(fields[key][0], 8, 32)
		if e != nil {
			t.Fatal(e)
		}
		p[key] = backupContractJSON(t, n)
	}
	if e := validateBackupUnitEffective(p, false); e != nil {
		t.Fatal(e)
	}
}
func TestCutoverBackupEnvironmentCannotOverrideCredentialRoute(t *testing.T) {
	good := "NMCP_DATABASE_URL=service=nmcp_backup_control\nNMCP_BACKUP_DUMP_SERVICE=nmcp_backup_dump\nNMCP_BACKUP_ROOT=/var/lib/nmcp-backup\nNMCP_BACKUP_PG_DUMP=/usr/bin/pg_dump\n"
	if validateCutoverBackupEnvironment([]byte(good)) != nil {
		t.Fatal("canonical environment rejected")
	}
	for _, extra := range []string{"PGPASSFILE=/tmp/private", "PGSERVICEFILE=/tmp/service", "PGHOST=evil", "NMCP_BACKUP_DUMP_PASSFILE=/tmp/private", "NMCP_CONFIG_FILE=/tmp/override", "LD_PRELOAD=/tmp/library", "NMCP_BACKUP_ROOT=/var/lib/nmcp-backup", "NMCP_LOG_LEVEL=info bad", "NMCP_LOG_LEVEL=\"info\"", "NMCP_BACKUP_NOTIFY_HOOK=relative", "NMCP_BACKUP_PG_RESTORE=/usr/bin/../bin/pg_restore"} {
		if validateCutoverBackupEnvironment([]byte(good+extra+"\n")) == nil {
			t.Fatalf("override admitted for %s", strings.Split(extra, "=")[0])
		}
	}
	if validateCutoverBackupEnvironment([]byte(strings.ReplaceAll(good, "nmcp_backup_control", "nmcp_backup_dump"))) == nil {
		t.Fatal("control role changed")
	}
}
