package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const backupUnitWireLimit = 1 << 20

// Capture remains bounded even if the child continues writing after overflow.
type backupUnitWireCapture struct {
	data     []byte
	overflow bool
}

func (b *backupUnitWireCapture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := backupUnitWireLimit - len(b.data)
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}
func backupUnitWireError(name, reason string) error {
	if name != "" {
		for i, c := range name {
			if !(c >= 'A' && c <= 'Z' || i > 0 && c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
				name = ""
				break
			}
		}
	}
	if name == "" {
		return fmt.Errorf("%w: backup unit wire: %s", errCutover, reason)
	}
	return fmt.Errorf("%w: backup unit property %s: %s", errCutover, name, reason)
}
func queryBackupUnitProperties(ctx context.Context, iface string, props []backupUnitProperty) (map[string]json.RawMessage, error) {
	if ctx == nil || (iface != "Unit" && iface != "Service" && iface != "org.freedesktop.systemd1.Unit" && iface != "org.freedesktop.systemd1.Service") {
		return nil, backupUnitWireError("", "invalid interface")
	}
	if !strings.HasPrefix(iface, "org.") {
		iface = "org.freedesktop.systemd1." + iface
	}
	if len(props) == 0 {
		return nil, backupUnitWireError("", "empty request")
	}
	args := []string{"--system", "--json=short", "--auto-start=no", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/nmcp_2dbackup_2eservice", iface}
	for _, p := range props {
		args = append(args, p.name)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/busctl", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	var capture backupUnitWireCapture
	cmd.Stdout = &capture
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, backupUnitWireError("", "query failed")
	}
	if capture.overflow {
		return nil, backupUnitWireError("", "output limit")
	}
	return decodeBackupUnitProperties(capture.data, props)
}

func decodeBackupUnitProperties(raw []byte, props []backupUnitProperty) (map[string]json.RawMessage, error) {
	if len(raw) > backupUnitWireLimit || len(props) == 0 {
		return nil, backupUnitWireError("", "invalid size or request")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	out := make(map[string]json.RawMessage, len(props))
	for _, p := range props {
		if _, ok := out[p.name]; ok {
			return nil, backupUnitWireError(p.name, "duplicate request")
		}
		tok, err := d.Token()
		if err != nil || tok != json.Delim('{') {
			return nil, backupUnitWireError(p.name, "invalid envelope")
		}
		fields := map[string]json.RawMessage{}
		for d.More() {
			tok, err = d.Token()
			key, ok := tok.(string)
			if err != nil || !ok || (key != "type" && key != "data") {
				return nil, backupUnitWireError(p.name, "invalid envelope key")
			}
			if _, ok := fields[key]; ok {
				return nil, backupUnitWireError(p.name, "duplicate envelope key")
			}
			var value json.RawMessage
			if d.Decode(&value) != nil {
				return nil, backupUnitWireError(p.name, "invalid envelope value")
			}
			fields[key] = value
		}
		tok, err = d.Token()
		if err != nil || tok != json.Delim('}') || len(fields) != 2 {
			return nil, backupUnitWireError(p.name, "incomplete envelope")
		}
		var signature string
		if json.Unmarshal(fields["type"], &signature) != nil || signature != p.signature {
			return nil, backupUnitWireError(p.name, "signature mismatch")
		}
		if _, ok := backupUnitJSONValue(fields["data"]); !ok {
			return nil, backupUnitWireError(p.name, "invalid data")
		}
		if p.want != nil && !equalBackupUnitJSON(fields["data"], p.want) {
			return nil, backupUnitWireError(p.name, "value mismatch")
		}
		out[p.name] = fields["data"]
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, backupUnitWireError("", "trailing data")
	}
	return out, nil
}

// Token parsing rejects duplicate object keys at every depth and never converts
// integers to float64. Null and an empty array deliberately remain different.
func backupUnitJSONValue(raw []byte) (any, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, ok := backupUnitJSONToken(d, 0)
	if !ok {
		return nil, false
	}
	_, err := d.Token()
	return v, err == io.EOF
}
func backupUnitJSONToken(d *json.Decoder, depth int) (any, bool) {
	if depth > 64 {
		return nil, false
	}
	t, err := d.Token()
	if err != nil {
		return nil, false
	}
	switch t {
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, ok := backupUnitJSONToken(d, depth+1)
			if !ok {
				return nil, false
			}
			a = append(a, v)
		}
		end, err := d.Token()
		return a, err == nil && end == json.Delim(']')
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			key, err := d.Token()
			s, ok := key.(string)
			if err != nil || !ok {
				return nil, false
			}
			if _, ok = m[s]; ok {
				return nil, false
			}
			v, ok := backupUnitJSONToken(d, depth+1)
			if !ok {
				return nil, false
			}
			m[s] = v
		}
		end, err := d.Token()
		return m, err == nil && end == json.Delim('}')
	default:
		if _, bad := t.(json.Delim); bad {
			return nil, false
		}
		return t, true
	}
}
func equalBackupUnitJSON(raw json.RawMessage, want any) bool {
	expected, err := json.Marshal(want)
	if err != nil {
		return false
	}
	a, ok := backupUnitJSONValue(raw)
	if !ok {
		return false
	}
	b, ok := backupUnitJSONValue(expected)
	return ok && reflect.DeepEqual(a, b)
}

func validateBackupUnitCommands(raw json.RawMessage, wanted [][]string) bool {
	value, ok := backupUnitJSONValue(raw)
	if !ok {
		return false
	}
	rows, ok := value.([]any)
	if !ok || len(rows) != len(wanted) {
		return false
	}
	const credentials = "/run/credentials/nmcp-backup.service/"
	for i, v := range rows {
		row, ok := v.([]any)
		if !ok || len(row) != 10 || len(wanted[i]) == 0 {
			return false
		}
		path, ok := row[0].(string)
		if !ok || path != wanted[i][0] {
			return false
		}
		argv, ok := row[1].([]any)
		if !ok || len(argv) != len(wanted[i]) {
			return false
		}
		for j, a := range argv {
			actual, ok := a.(string)
			if !ok {
				return false
			}
			expected := wanted[i][j]
			// Only a credential source operand may retain systemd's exact %d spelling.
			if strings.HasPrefix(expected, credentials) {
				leaf := strings.TrimPrefix(expected, credentials)
				if leaf != "" && !strings.Contains(leaf, "/") && actual == "%d/"+leaf {
					actual = expected
				}
			}
			if actual != expected {
				return false
			}
		}
		flags, ok := row[2].([]any)
		if !ok || len(flags) != 0 {
			return false
		}
		for j := 3; j < 10; j++ {
			n, ok := row[j].(json.Number)
			if !ok {
				return false
			}
			if j < 8 {
				bits := 64
				if j == 7 {
					bits = 32
				}
				if _, err := strconv.ParseUint(string(n), 10, bits); err != nil {
					return false
				}
			} else {
				if _, err := strconv.ParseInt(string(n), 10, 32); err != nil {
					return false
				}
			}
		}
	}
	return true
}
