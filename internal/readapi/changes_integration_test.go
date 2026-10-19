package readapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestChangesIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := readIntegrationPool(t)
	codec, _ := NewCursorCodec([]byte(strings.Repeat("c", 32)))
	service, err := NewService(pool, codec, "https://files.example/files/")
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.ListChanges(ctx, ChangesRequest{Limit: 1})
	if err != nil || len(page.Items) != 0 || page.NextCursor == "" {
		t.Fatalf("initial: %+v %v", page, err)
	}
	initial := page.NextCursor
	appendEvent := func(tx pgx.Tx, id int) int64 {
		t.Helper()
		var position int64
		if err := tx.QueryRow(ctx, `UPDATE change_feed_state SET last_position=last_position+1 WHERE id=1 RETURNING last_position`).Scan(&position); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO change_events(id,position,event_type,reason,media_id) VALUES($1,$2,'media_deleted','logical_delete',$3)`, integrationUUID(id), position, integrationUUID(id+1000)); err != nil {
			t.Fatal(err)
		}
		return position
	}
	a, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback(context.Background())
	if appendEvent(a, 1) != 1 {
		t.Fatal("first position")
	}
	// A's allocated but uncommitted position must not be readable or skipped.
	page, err = service.ListChanges(ctx, ChangesRequest{Cursor: initial, Limit: 1})
	if err != nil || len(page.Items) != 0 || page.NextCursor != initial {
		t.Fatalf("uncommitted visible: %+v %v", page, err)
	}
	b, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Rollback(context.Background())
	// A second writer cannot commit a larger position while A is delayed.
	blockedCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = b.Exec(blockedCtx, `UPDATE change_feed_state SET last_position=last_position+1 WHERE id=1`)
	stop()
	if err == nil {
		t.Fatal("later writer bypassed serialization")
	}
	_ = b.Rollback(ctx)
	if err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rolled, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if appendEvent(rolled, 2) != 2 {
		t.Fatal("rollback position")
	}
	if err := rolled.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	committed, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if appendEvent(committed, 3) != 2 {
		t.Fatal("rollback did not restore counter")
	}
	if err := committed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	page, err = service.ListChanges(ctx, ChangesRequest{Cursor: initial, Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Position != "1" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	replay, err := service.ListChanges(ctx, ChangesRequest{Cursor: initial, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(page)
	after, _ := json.Marshal(replay)
	if string(before) != string(after) {
		t.Fatal("replay changed")
	}
	// Reconstruct the service and codec, as after process restart, with same key.
	freshCodec, _ := NewCursorCodec([]byte(strings.Repeat("c", 32)))
	fresh, _ := NewService(pool, freshCodec, "https://files.example/files/")
	resumed, err := fresh.ListChanges(ctx, ChangesRequest{Cursor: page.NextCursor, Limit: 1000})
	if err != nil || len(resumed.Items) != 1 || resumed.Items[0].Position != "2" {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	empty, err := fresh.ListChanges(ctx, ChangesRequest{Cursor: resumed.NextCursor, Limit: 1})
	if err != nil || len(empty.Items) != 0 || empty.NextCursor != resumed.NextCursor {
		t.Fatalf("open end: %+v %v", empty, err)
	}
	for _, request := range []ChangesRequest{{Limit: 0}, {Limit: 1001}, {Limit: 1, Cursor: "1"}} {
		if _, err := fresh.ListChanges(ctx, request); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	// Independent parallel transactions must each publish exactly once with
	// contiguous committed positions; consume across irregular page boundaries.
	results := make(chan error, 12)
	for n := 10; n < 22; n++ {
		go func(id int) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				results <- err
				return
			}
			defer tx.Rollback(context.Background())
			_, err = tx.Exec(ctx, `WITH allocated AS (UPDATE change_feed_state SET last_position=last_position+1 WHERE id=1 RETURNING last_position) INSERT INTO change_events(id,position,event_type,reason,media_id) SELECT $1,last_position,'media_deleted','logical_delete',$2 FROM allocated`, integrationUUID(id), integrationUUID(id+1000))
			if err == nil {
				err = tx.Commit(ctx)
			}
			results <- err
		}(n)
	}
	for n := 0; n < 12; n++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	cursor := empty.NextCursor
	seen := map[string]bool{}
	for n := 0; n < 4; n++ {
		page, err := fresh.ListChanges(ctx, ChangesRequest{Cursor: cursor, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 3 {
			t.Fatalf("parallel page: %+v", page)
		}
		for _, event := range page.Items {
			if seen[event.ID] {
				t.Fatal("duplicate across pages")
			}
			seen[event.ID] = true
		}
		cursor = page.NextCursor
	}
	if len(seen) != 12 {
		t.Fatal("parallel events missing")
	}
	final, err := fresh.ListChanges(ctx, ChangesRequest{Cursor: cursor, Limit: 1})
	if err != nil || len(final.Items) != 0 {
		t.Fatalf("final: %+v %v", final, err)
	}

}
