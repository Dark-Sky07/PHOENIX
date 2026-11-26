package database

import (
	"errors"
	"testing"
)

func TestIsConcurrentSchemaChangeErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"catalog race", errors.New(`ERROR: duplicate key value violates unique constraint "pg_type_typname_nsp_index" (SQLSTATE 23505)`), true},
		{"index race", errors.New(`ERROR: duplicate key value violates unique constraint "pg_index_indexrelid_index" (SQLSTATE 23505)`), true},
		{"data duplicate", errors.New(`ERROR: duplicate key value violates unique constraint "users_pkey" (SQLSTATE 23505)`), false},
		{"other failure", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isConcurrentSchemaChangeErr(tc.err); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
