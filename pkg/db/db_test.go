package db

import (
	"errors"
	"fmt"
	"io"
	"skima/pkg/manifest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestAsPgError(t *testing.T) {
	serverErr := &pgconn.PgError{Code: "42P07", Message: "relation already exists"}

	if asPgError(nil) != nil {
		t.Error("nil error should not be a postgres error")
	}
	// a connection/network failure must not panic and must not be treated as a postgres error
	if asPgError(io.ErrUnexpectedEOF) != nil {
		t.Error("network error should not be a postgres error")
	}
	if pgErr := asPgError(serverErr); pgErr == nil || pgErr.Code != "42P07" {
		t.Errorf("EXPECTED postgres error with code 42P07  ACTUAL: %v", pgErr)
	}
	if pgErr := asPgError(fmt.Errorf("applying delta: %w", serverErr)); pgErr == nil || pgErr.Code != "42P07" {
		t.Errorf("EXPECTED wrapped postgres error with code 42P07  ACTUAL: %v", pgErr)
	}
	if asPgError(errors.New("conn closed")) != nil {
		t.Error("plain error should not be a postgres error")
	}
}

func TestHookedDeltas(t *testing.T) {
	hook := manifest.PrePostHook{Pre: manifest.Hook{Sql: []string{"select 1"}}, Post: manifest.Hook{Script: "post.sql"}}
	for name, delta := range map[string]manifest.Delta{
		"SimpleDelta": manifest.SimpleDelta{PrePostHook: hook},
		"ColumnDelta": manifest.ColumnDelta{PrePostHook: hook},
		"FkDelta":     manifest.FkDelta{PrePostHook: hook},
	} {
		d, ok := delta.(hooked)
		if !ok {
			t.Errorf("%s should support pre/post hooks", name)
			continue
		}
		if got := d.Hooks(); len(got.Pre.Sql) != 1 || got.Post.Script != "post.sql" {
			t.Errorf("%s returned the wrong hooks: %+v", name, got)
		}
	}
	if _, ok := manifest.Delta(manifest.SqlDelta{}).(hooked); ok {
		t.Error("SqlDelta should not support pre/post hooks")
	}
}
