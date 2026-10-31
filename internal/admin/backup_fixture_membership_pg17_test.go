package admin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBackupPG17FixtureMembershipDDL(t *testing.T) {
	bin := os.Getenv("TEST_BACKUP_PG17_BINDIR")
	if bin == "" {
		t.Skip("TEST_BACKUP_PG17_BINDIR absent: actual membership replay NOT executed")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("PG17 prefix must be absolute")
	}
	version, err := exec.Command(filepath.Join(bin, "postgres"), "--version").Output()
	if err != nil || !strings.Contains(string(version), "(PostgreSQL) 17.") {
		t.Fatal("actual PG17 required")
	}
	f := newPG17Fixture(t, bin)
	admin := f.pool("operator", "postgres")

	f.sql(admin, `CREATE ROLE nmcp_root; CREATE ROLE nmcp_z_grantor; CREATE ROLE nmcp_y_mid;
 GRANT nmcp_root TO nmcp_z_grantor WITH ADMIN TRUE, INHERIT FALSE, SET TRUE GRANTED BY operator;
 GRANT nmcp_root TO nmcp_y_mid WITH ADMIN TRUE, INHERIT TRUE, SET FALSE GRANTED BY nmcp_z_grantor`)
	type tuple struct {
		Role, Member, Grantor string
		Admin, Inherit, Set   bool
	}
	expected := []tuple{{"nmcp_root", "nmcp_y_mid", "nmcp_z_grantor", true, true, false}, {"nmcp_root", "nmcp_z_grantor", "operator", true, false, true}}
	var members []tuple
	for a := 0; a < 2; a++ {
		for i := 0; i < 2; i++ {
			for s := 0; s < 2; s++ {
				name := fmt.Sprintf("nmcp_a%d%d%d", a, i, s)
				f.sql(admin, fmt.Sprintf("CREATE ROLE %s; GRANT nmcp_root TO %s WITH ADMIN %t, INHERIT %t, SET %t GRANTED BY nmcp_y_mid", name, name, a == 1, i == 1, s == 1))
				members = append(members, tuple{"nmcp_root", name, "nmcp_y_mid", a == 1, i == 1, s == 1})
			}
		}
	}
	expected = append(members, expected...)
	// Independent raw catalog oracle: no generated DDL/format/CASE is involved.
	inventory := func() []tuple {
		rows, err := admin.Query(f.ctx, `SELECT r.rolname,m.rolname,g.rolname,a.admin_option,a.inherit_option,a.set_option FROM pg_auth_members a JOIN pg_roles r ON r.oid=a.roleid JOIN pg_roles m ON m.oid=a.member JOIN pg_roles g ON g.oid=a.grantor WHERE r.rolname='nmcp_root' ORDER BY m.rolname,g.rolname`)
		if err != nil {
			t.Fatal("raw membership query")
		}
		defer rows.Close()
		var tuples []tuple
		for rows.Next() {
			var row tuple
			if rows.Scan(&row.Role, &row.Member, &row.Grantor, &row.Admin, &row.Inherit, &row.Set) != nil {
				t.Fatal("raw membership scan")
			}
			tuples = append(tuples, row)
		}
		if rows.Err() != nil {
			t.Fatal("raw membership rows")
		}
		return tuples
	}
	before := inventory()
	if !reflect.DeepEqual(expected, before) {
		t.Fatal("initial raw graph differs from independently constructed option tuples")
	}
	rows, err := readFixtureMemberships(f.ctx, admin)
	if err != nil || len(rows) != 10 {
		t.Fatal("fixture must cover two-level grantors and all eight options")
	}
	ordered, err := orderFixtureMemberships(rows)
	if err != nil {
		t.Fatal("valid dependency graph refused")
	}
	f.sql(admin, `REVOKE nmcp_root FROM nmcp_z_grantor CASCADE`)
	if len(inventory()) != 0 {
		t.Fatal("dependent memberships remain after revoke")
	}
	for _, row := range rows {
		invalid := strings.NewReplacer(" TRUE", " t", " FALSE", " f").Replace(row.DDL)
		_, err := admin.Exec(f.ctx, invalid)
		if fixtureSQLState(err) != "42601" {
			t.Fatal("original t/f must fail PG17 syntax")
		}
	}
	if _, err := admin.Exec(f.ctx, rows[0].DDL); fixtureSQLState(err) != "42501" {
		t.Fatal("lexical member-first replay must fail missing grantor ADMIN")
	}
	if len(inventory()) != 0 {
		t.Fatal("rejected lexical replay changed graph")
	}
	for _, stmt := range ordered {
		f.sql(admin, stmt)
	}
	if !reflect.DeepEqual(expected, inventory()) {
		t.Fatal("dependency replay changed raw role/member/grantor/option tuples")
	}
	t.Log("Actual PG17: reversed two-level grantors and all eight raw option tuples preserved; lexical ADMIN and original t/f replay refused")
}
