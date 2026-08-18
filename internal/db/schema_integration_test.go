package db_test

import (
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
)

// A migrated database must satisfy the binary that migrated it.
//
// If this fails, every deployment of this build serves 500s until someone reads
// a log — which is exactly what happened once, when an image was upgraded
// without its migration.
func TestAMigratedSchemaIsUpToDate(t *testing.T) {
	pool, _, _ := dbtest.SetupWithUser(t)

	state, err := db.CheckSchema(t.Context(), pool)
	if err != nil {
		t.Fatalf("CheckSchema() = %v", err)
	}
	if state.Expected == 0 {
		t.Fatal("Expected = 0, so no migrations were found and this check can never fail")
	}
	if !state.UpToDate() {
		t.Errorf("a freshly migrated database is at version %d but this build needs %d",
			state.Applied, state.Expected)
	}
}

// The comparison itself, as a table.
//
// Deliberately not driven by mutating goose_db_version in a real database: the
// integration tests share one, and rolling its recorded version back left every
// other package unable to migrate. A destructive test of shared state is a worse
// bug than the one it is testing for.
func TestSchemaStateUpToDate(t *testing.T) {
	tests := []struct {
		name    string
		state   db.SchemaState
		want    bool
		because string
	}{
		{
			name:    "current",
			state:   db.SchemaState{Applied: 3, Expected: 3},
			want:    true,
			because: "the normal case",
		},
		{
			name:    "the image was upgraded without its migration",
			state:   db.SchemaState{Applied: 2, Expected: 3},
			want:    false,
			because: "this is the outage: the binary queries columns that do not exist",
		},
		{
			name:    "nothing has ever been migrated",
			state:   db.SchemaState{Applied: 0, Expected: 3},
			want:    false,
			because: "a first deployment before its migration Job has run",
		},
		{
			name:  "rolled back to an older binary",
			state: db.SchemaState{Applied: 9, Expected: 3},
			want:  true,
			because: "an old binary's queries work against a superset schema, and treating " +
				"this as a failure would make every rollback an outage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.UpToDate(); got != tt.want {
				t.Errorf("SchemaState{Applied: %d, Expected: %d}.UpToDate() = %v, want %v — %s",
					tt.state.Applied, tt.state.Expected, got, tt.want, tt.because)
			}
		})
	}
}
