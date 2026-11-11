package admin

import (
	"strings"
	"testing"
)

var fixtureSliceProperties = []string{"Id", "LoadState", "ActiveState", "SubState", "FragmentPath", "SourcePath", "DropInPaths", "Transient", "ControlGroup", "UnitFileState", "Job", "InvocationID"}

// A slice is implicitly loadable even with no installed definition. This is not
// permission to adopt an existing runtime: only the exact, inactive, never-used
// empty manager object is eligible, followed by independent disk absence and
// exclusive fixture ownership checks. Missing/duplicate/unknown fields fail shut.
func pristineFixtureSlice(unit, text string) bool {
	if unit != "nmcp.slice" {
		return false
	}
	want := map[string]string{
		"Id": unit, "LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead",
		"FragmentPath": "", "SourcePath": "", "DropInPaths": "", "Transient": "no",
		"ControlGroup": "", "UnitFileState": "", "Job": "", "InvocationID": "",
	}
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		expected, known := want[key]
		if !ok || !known || seen[key] || value != expected {
			return false
		}
		seen[key] = true
	}
	return len(seen) == len(want)
}

func TestBackupImplicitSliceFixtureGuard(t *testing.T) {
	const pristine = "Id=nmcp.slice\nLoadState=loaded\nActiveState=inactive\nSubState=dead\nFragmentPath=\nSourcePath=\nDropInPaths=\nTransient=no\nControlGroup=\nUnitFileState=\nJob=\nInvocationID=\n"
	if !pristineFixtureSlice("nmcp.slice", pristine) {
		t.Fatal("reject pristine implicit slice")
	}
	for _, key := range fixtureSliceProperties {
		t.Run(key, func(t *testing.T) {
			lines := strings.Split(strings.TrimSuffix(pristine, "\n"), "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, key+"=") {
					changed := append([]string{}, lines...)
					changed[i] = key + "=unsafe"
					if pristineFixtureSlice("nmcp.slice", strings.Join(changed, "\n")) {
						t.Fatal("accepted non-pristine property")
					}
					if pristineFixtureSlice("nmcp.slice", strings.Replace(pristine, line+"\n", "", 1)) {
						t.Fatal("accepted missing property")
					}
					if pristineFixtureSlice("nmcp.slice", pristine+line+"\n") {
						t.Fatal("accepted duplicate property")
					}
				}
			}
		})
	}
	for original, replacement := range map[string]string{
		"ActiveState=inactive": "ActiveState=active",
		"Transient=no":         "Transient=yes",
		"FragmentPath=\n":      "FragmentPath=/etc/systemd/system/nmcp.slice\n",
		"DropInPaths=\n":       "DropInPaths=/run/systemd/system/nmcp.slice.d/custom.conf\n",
		"ControlGroup=\n":      "ControlGroup=/nmcp.slice\n",
		"Job=\n":               "Job=42\n",
		"InvocationID=\n":      "InvocationID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
	} {
		if pristineFixtureSlice("nmcp.slice", strings.Replace(pristine, original, replacement, 1)) {
			t.Fatalf("accepted occupied slice: %s", replacement)
		}
	}
	for _, unit := range []string{"", "other.slice", "nmcp-api.service", "system.slice"} {
		if pristineFixtureSlice(unit, pristine) {
			t.Fatal("accepted unrelated unit")
		}
	}
	for _, text := range []string{"", pristine + "Unknown=\n", pristine + "malformed\n"} {
		if pristineFixtureSlice("nmcp.slice", text) {
			t.Fatal("accepted malformed inspection")
		}
	}
}
