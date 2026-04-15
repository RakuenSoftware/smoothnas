package mdadm_test

import (
	"testing"

	mdadmadapter "github.com/JBailes/SmoothNAS/tierd/internal/tiering/mdadm"
)

// ---- Migrate ----------------------------------------------------------------

func TestMigrateEmptyDB(t *testing.T) {
	store := openStore(t)
	if err := mdadmadapter.Migrate(store); err != nil {
		t.Fatalf("Migrate on empty DB: %v", err)
	}
}
