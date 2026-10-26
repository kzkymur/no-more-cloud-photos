package medialifecycle

import (
	"context"
	"testing"
)

func TestExactPurgeReclaimRejectsMissingScopeBeforeDatabaseAccess(t *testing.T) {
	for _, id := range []string{"", "not-a-job", "00000000-0000-0000-0000-000000000000"} {
		if err := (&PostgresRepository{}).ReclaimExpiredPurge(context.Background(), id); !IsKind(err, KindInvalidRequest) {
			t.Fatalf("unscoped reclaim id=%q err=%v", id, err)
		}
	}
}
