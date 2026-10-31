package admin

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

type fixtureMembership struct {
	Role, Member, Grantor             string
	Admin, Inherit, Set, GrantorSuper bool
	DDL                               string
}

func readFixtureMemberships(ctx context.Context, pool *pgxpool.Pool) ([]fixtureMembership, error) {
	rows, err := pool.Query(ctx, offlineMembershipDDLQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []fixtureMembership
	for rows.Next() {
		var row fixtureMembership
		if err = rows.Scan(&row.Role, &row.Member, &row.Grantor, &row.Admin, &row.Inherit, &row.Set, &row.GrantorSuper, &row.DDL); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// ADMIN OPTION is role-specific and is not inherited through other roles.
// Resolve the entire graph before executing any GRANT; unresolved dependencies
// are a refusal, never a reason to change a grantor or grant extra authority.
func orderFixtureMemberships(rows []fixtureMembership) ([]string, error) {
	pending := append([]fixtureMembership(nil), rows...)
	authorized := map[[2]string]bool{}
	var ordered []string
	for len(pending) > 0 {
		var next []fixtureMembership
		for _, row := range pending {
			if !row.GrantorSuper && !authorized[[2]string{row.Role, row.Grantor}] {
				next = append(next, row)
				continue
			}
			ordered = append(ordered, row.DDL)
			if row.Admin {
				authorized[[2]string{row.Role, row.Member}] = true
			}
		}
		if len(next) == len(pending) {
			return nil, errors.New("unresolved fixture membership grant dependencies")
		}
		pending = next
	}
	return ordered, nil
}

func TestBackupFixtureMembershipDependencyOrder(t *testing.T) {
	rows := []fixtureMembership{
		{Role: "r", Member: "a", Grantor: "y", DDL: "third"},
		{Role: "r", Member: "y", Grantor: "z", Admin: true, DDL: "second"},
		{Role: "r", Member: "z", Grantor: "operator", GrantorSuper: true, Admin: true, DDL: "first"},
	}
	ordered, err := orderFixtureMemberships(rows)
	if err != nil || !reflect.DeepEqual(ordered, []string{"first", "second", "third"}) {
		t.Fatal("grantor dependencies not honored")
	}
	for _, invalid := range [][]fixtureMembership{rows[:2], {rows[0], {Role: "r", Member: "y", Grantor: "operator", GrantorSuper: true, Admin: false}}, {rows[0], {Role: "other", Member: "y", Grantor: "operator", GrantorSuper: true, Admin: true}}} {
		if ordered, err := orderFixtureMemberships(invalid); err == nil || ordered != nil {
			t.Fatal("unresolved graph partially admitted")
		}
	}
}
