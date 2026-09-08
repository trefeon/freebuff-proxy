package wirefacts

import (
	"bytes"
	"os"
	"testing"
)

// TestCatalogGenUpToDate regenerates the modelcat catalog in memory and
// requires it to equal the checked-in catalog_gen.go byte for byte.
func TestCatalogGenUpToDate(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := EmitCatalog(testUpstream, testRegDir, &buf); err != nil {
		t.Fatalf("EmitCatalog: %v", err)
	}
	want, err := os.ReadFile("../modelcat/catalog_gen.go")
	if err != nil {
		t.Fatalf("read catalog_gen.go: %v (run go generate ./backend/internal/wirefacts/)", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("catalog_gen.go is stale: regenerate with go run ./backend/cmd/wiregen -upstream %s and commit the result", testUpstream)
	}
}
