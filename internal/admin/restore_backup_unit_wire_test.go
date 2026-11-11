package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestBackupUnitWireProperties(t *testing.T) {
	props := []backupUnitProperty{{name: "User", signature: "s", want: "nmcp-backup"}, {name: "RuntimeDirectoryMode", signature: "u", want: 448}, {name: "PermissionsStartOnly", signature: "b", want: false}}
	good := `{"type":"s","data":"nmcp-backup"}` + "\n" + `{"data":448,"type":"u"}` + "\n" + `{"type":"b","data":false}`
	got, err := decodeBackupUnitProperties([]byte(good), props)
	if err != nil || len(got) != 3 || string(got["RuntimeDirectoryMode"]) != "448" {
		t.Fatal("valid ordered independent properties refused")
	}
	bad := []string{
		strings.Replace(good, `"type":"s"`, `"type":"s","type":"s"`, 1),
		strings.Replace(good, `"data":"nmcp-backup"`, `"data":"nmcp-backup","data":"nmcp-backup"`, 1),
		strings.Replace(good, `"type":"s"`, `"type":"s","extra":false`, 1),
		strings.Replace(good, `"type":"s"`, `"type":"as"`, 1),
		strings.Replace(good, `"data":448`, `"data":"448"`, 1),
		strings.Replace(good, `"data":448`, `"data":448.0`, 1),
		strings.Replace(good, `"data":false`, `"data":null`, 1),
		strings.Replace(good, `"data":false`, `"data":0`, 1),
		strings.Replace(good, `"data":"nmcp-backup"`, `"data":"CANARY-secret"`, 1),
		good + ` {}`, good + ` trailing`, good[:len(good)-1], `[]`, `null`,
		`{"type":"s","data":"nmcp-backup"}`, `{"type":"s"}`, `{"type":"s","data":`,
	}
	for i, raw := range bad {
		if _, err := decodeBackupUnitProperties([]byte(raw), props); err == nil || !errors.Is(err, errCutover) || strings.Contains(err.Error(), "CANARY-secret") {
			t.Fatalf("unsafe property input %d accepted or disclosed", i)
		}
	}
	if _, err := decodeBackupUnitProperties(bytes.Repeat([]byte(" "), backupUnitWireLimit+1), props); err == nil {
		t.Fatal("oversized input accepted")
	}
	if _, err := decodeBackupUnitProperties([]byte(`{"type":"s","data":"a"}{"type":"s","data":"a"}`), []backupUnitProperty{{name: "User", signature: "s"}, {name: "User", signature: "s"}}); err == nil {
		t.Fatal("duplicate requested name accepted")
	}
}

func TestBackupUnitWireJSONExactTypes(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  any
		valid bool
	}{
		{`[]`, []string{}, true}, {`null`, []string{}, false}, {`[]`, nil, false},
		{`18446744073709551615`, uint64(math.MaxUint64), true},
		{`18446744073709551614`, uint64(math.MaxUint64), false},
		{`{"a":1,"a":1}`, map[string]int{"a": 1}, false},
		{`{"b":2,"a":1}`, map[string]int{"a": 1, "b": 2}, true},
		{`[1]`, []string{"1"}, false}, {`false`, false, true}, {`0`, false, false},
		{`[] {}`, []string{}, false}, {`[`, []string{}, false},
	} {
		if equalBackupUnitJSON(json.RawMessage(tc.raw), tc.want) != tc.valid {
			t.Fatalf("JSON comparison mismatch for %s", tc.raw)
		}
	}
	if _, err := decodeBackupUnitProperties([]byte(`{"type":"a{sv}","data":{"a":1,"a":2}}`), []backupUnitProperty{{name: "Extra", signature: "a{sv}"}}); err == nil {
		t.Fatal("nested duplicate accepted")
	}
}

func TestBackupUnitWireCommands(t *testing.T) {
	wanted := [][]string{{"/usr/bin/install", "-T", "-m", "0600", "/run/credentials/nmcp-backup.service/control.pgpass", "/run/nmcp-backup/control.pgpass"}, {"/usr/bin/install", "-T", "-m", "0600", "/run/credentials/nmcp-backup.service/dump.pgpass", "/run/nmcp-backup/dump.pgpass"}}
	makeRows := func() []any {
		rows := []any{}
		for _, args := range wanted {
			rows = append(rows, []any{args[0], append([]string{}, args...), []string{}, uint64(math.MaxUint64), uint64(123), uint64(456), uint64(789), uint32(1234), int32(1), int32(23)})
		}
		return rows
	}
	check := func(rows any) bool {
		raw, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		return validateBackupUnitCommands(raw, wanted)
	}
	if !check(makeRows()) {
		t.Fatal("valid commands with nonzero dynamic metadata refused")
	}
	rows := makeRows()
	args := rows[0].([]any)[1].([]string)
	args[4] = "%d/control.pgpass"
	if !check(rows) {
		t.Fatal("exact credential source specifier refused")
	}
	for _, flag := range []string{"ignore-failure", "privileged", "no-setuid", "ambient", "no-env-expand", "unknown", "+"} {
		rows = makeRows()
		rows[0].([]any)[2] = []string{flag}
		if check(rows) {
			t.Fatal("command flag accepted")
		}
	}
	mutations := []func([]any){
		func(r []any) { r[0].([]any)[0] = "/bin/sh" },
		func(r []any) { r[0].([]any)[1].([]string)[0] = "overridden-argv0" },
		func(r []any) { r[0].([]any)[1].([]string)[5] = "%d/control.pgpass" },
		func(r []any) { r[0].([]any)[1].([]string)[4] = "%d/other.pgpass" },
		func(r []any) {
			r[0].([]any)[1].([]string)[4] = "/run/credentials/nmcp-backup.service/../control.pgpass"
		},
		func(r []any) { r[0].([]any)[1] = nil }, func(r []any) { r[0].([]any)[2] = nil },
		func(r []any) { r[0].([]any)[3] = nil }, func(r []any) { r[0].([]any)[3] = "123" },
		func(r []any) { r[0].([]any)[3] = json.Number("1.0") }, func(r []any) { r[0].([]any)[3] = json.Number("1e2") },
		func(r []any) { r[0].([]any)[3] = json.Number("-1") }, func(r []any) { r[0].([]any)[3] = json.Number("18446744073709551616") },
		func(r []any) { r[0].([]any)[7] = uint64(1) << 32 }, func(r []any) { r[0].([]any)[8] = int64(1) << 31 },
		func(r []any) { r[0].([]any)[9] = int64(-2147483649) }, func(r []any) { r[0].([]any)[9] = true },
		func(r []any) { r[0] = r[0].([]any)[:9] }, func(r []any) { r[0] = append(r[0].([]any), 0) },
		func(r []any) { r[0], r[1] = r[1], r[0] },
	}
	for i, mutate := range mutations {
		rows = makeRows()
		mutate(rows)
		if check(rows) {
			t.Fatalf("invalid command mutation %d accepted", i)
		}
	}
	if check(makeRows()[:1]) || check(append(makeRows(), makeRows()[0])) || check(nil) {
		t.Fatal("wrong command count/null accepted")
	}
	if !validateBackupUnitCommands(json.RawMessage(`[]`), nil) || validateBackupUnitCommands(json.RawMessage(`null`), nil) {
		t.Fatal("empty command array confused with null")
	}
	if validateBackupUnitCommands(json.RawMessage(`[`), wanted) {
		t.Fatal("truncated commands accepted")
	}
}

func TestBackupUnitWireCaptureBound(t *testing.T) {
	var c backupUnitWireCapture
	if n, err := c.Write(bytes.Repeat([]byte("x"), backupUnitWireLimit)); n != backupUnitWireLimit || err != nil || c.overflow {
		t.Fatal("exact bound incorrectly rejected")
	}
	if n, err := c.Write([]byte("CANARY-overflow")); n != len("CANARY-overflow") || err != nil || !c.overflow || len(c.data) != backupUnitWireLimit || bytes.Contains(c.data, []byte("CANARY")) {
		t.Fatal("overflow capture not bounded")
	}
	if n, err := c.Write([]byte("more")); n != 4 || err != nil || len(c.data) != backupUnitWireLimit {
		t.Fatal("subsequent output grows capture")
	}
}
