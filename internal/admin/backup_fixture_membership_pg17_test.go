package admin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
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
	f.sql(admin, `CREATE ROLE nmcp_root; CREATE ROLE nmcp_a_grantor; GRANT nmcp_root TO nmcp_a_grantor WITH ADMIN TRUE, INHERIT FALSE, SET TRUE GRANTED BY operator`)
	for a := 0; a < 2; a++ {
		for i := 0; i < 2; i++ {
			for s := 0; s < 2; s++ {
				name := fmt.Sprintf("nmcp_m%d%d%d", a, i, s)
				f.sql(admin, fmt.Sprintf("CREATE ROLE %s; GRANT nmcp_root TO %s WITH ADMIN %t, INHERIT %t, SET %t GRANTED BY nmcp_a_grantor", name, name, a == 1, i == 1, s == 1))
			}
		}
	}
	inventory := func() []string {
		rows, err := admin.Query(f.ctx, offlineMembershipDDLQuery)
		if err != nil {
			t.Fatal("membership query")
		}
		result, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal("membership scan")
		}
		return result
	}
	before := inventory()
	if len(before) != 9 {
		t.Fatal("fixture must cover grantor and all eight option tuples")
	}
	f.sql(admin, `REVOKE nmcp_root FROM nmcp_a_grantor CASCADE`)
	if len(inventory()) != 0 {
		t.Fatal("dependent memberships remain after revoke")
	}
	for _, stmt := range before {
		invalid := strings.NewReplacer(" TRUE", " t", " FALSE", " f").Replace(stmt)
		_, err := admin.Exec(f.ctx, invalid)
		if fixtureSQLState(err) != "42601" {
			t.Fatal("original unquoted t/f must fail PG17 syntax")
		}
	}
	for _, stmt := range before {
		f.sql(admin, stmt)
	}
	if !reflect.DeepEqual(before, inventory()) {
		t.Fatal("replayed role/member/grantor/options changed")
	}
	t.Log("Actual PG17: all eight membership tuples and non-operator grantor preserved; original t/f refused")
}
