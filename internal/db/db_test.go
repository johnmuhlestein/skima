package db

import (
	"errors"
	"fmt"
	"io"
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
