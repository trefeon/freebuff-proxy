// Command wiregen regenerates backend/internal/wirefacts/wirefacts_gen.go
// from the verbatim upstream snapshots plus the registry pins.
//
// Usage (repo root; matches the ./backend/cmd/... convention in Taskfile.yml):
//
//	go run ./backend/cmd/wiregen -upstream <sha> [-wire <dir>] [-registry <dir>] [-out <file>]
//
// -upstream must equal the upstream_sha recorded in
// backend/internal/wirefacts/testdata/wire/snapshots.json; anything else is a
// hard failure (re-pin the snapshots first). Unknown TypeScript constructs in
// any snapshot are a hard failure naming file:line, the construct, and the
// upstream commit — the run exits non-zero and writes nothing. Later Wave D
// slices extend the accepted constructs as they add catalog, wirecodes,
// notices, and toolmap emission.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"freebuff-proxy/backend/internal/wirefacts"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("wiregen: ")

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		log.Fatal("cannot locate command source directory")
	}
	// backend/cmd/wiregen -> backend; the module root is one more level up,
	// but every default below stays inside backend/ so only backendRoot matters.
	backendRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	upstream := flag.String("upstream", "", "upstream commit SHA the snapshots were taken from (required; must match the snapshots manifest)")
	wireDir := flag.String("wire", filepath.Join(backendRoot, "internal", "wirefacts", "testdata", "wire"), "directory holding snapshots.json and the verbatim snapshots")
	registryDir := flag.String("registry", filepath.Join(backendRoot, "internal", "registry", "testdata", "upstream"), "directory holding the registry mirror files")
	outPath := flag.String("out", filepath.Join(backendRoot, "internal", "wirefacts", "wirefacts_gen.go"), "output path for the generated Go source")
	// --- S2 catalog (append-only; S3/S4 add their own flags below, never edit this one) ---
	catalogOut := flag.String("catalog-out", filepath.Join(backendRoot, "internal", "modelcat", "catalog_gen.go"), "output path for the generated model catalog Go source")
	// --- S3 wire (append-only; S4 adds its own flags below, never edit this one) ---
	wirecodesOut := flag.String("wirecodes-out", filepath.Join(backendRoot, "internal", "upstream", "wirecodes_gen.go"), "output path for the generated WireCode Go source")
	noticesOut := flag.String("notices-out", filepath.Join(backendRoot, "internal", "upstream", "notices_gen.go"), "output path for the generated notice-copy Go source")
	// --- S4 tools (append-only; last block) ---
	toolsOut := flag.String("tools-out", filepath.Join(backendRoot, "internal", "convert", "toolnames_gen.go"), "output path for the generated tool-names Go source")
	flag.Parse()

	if *upstream == "" {
		fmt.Fprintln(os.Stderr, "wiregen: missing required -upstream <sha>")
		flag.Usage()
		os.Exit(2)
	}
	var buf bytes.Buffer
	// Buffer first: a failing run emits nothing and leaves the previous
	// generated file untouched.
	if err := wirefacts.Run(*upstream, *wireDir, *registryDir, &buf); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*outPath, buf.Bytes(), 0o644); err != nil {
		log.Fatalf("write %s: %v", *outPath, err)
	}
	fmt.Fprintf(os.Stderr, "wiregen: wrote %s at upstream %s\n", *outPath, *upstream)
	// --- S2 catalog (append-only; S3/S4 add their own blocks below, never edit this one) ---
	var cbuf bytes.Buffer
	// Buffer first: a failing run emits nothing and leaves the previous
	// generated file untouched.
	if err := wirefacts.EmitCatalog(*upstream, *registryDir, &cbuf); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*catalogOut, cbuf.Bytes(), 0o644); err != nil {
		log.Fatalf("write %s: %v", *catalogOut, err)
	}
	fmt.Fprintf(os.Stderr, "wiregen: wrote %s at upstream %s\n", *catalogOut, *upstream)
	// --- S3 wire (append-only; S4 adds its own block below, never edit this one) ---
	var wbuf, nbuf bytes.Buffer
	// Buffer first: a failing run emits nothing and leaves the previous
	// generated files untouched.
	if err := wirefacts.EmitWire(*upstream, *wireDir, &wbuf, &nbuf); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*wirecodesOut, wbuf.Bytes(), 0o644); err != nil {
		log.Fatalf("write %s: %v", *wirecodesOut, err)
	}
	if err := os.WriteFile(*noticesOut, nbuf.Bytes(), 0o644); err != nil {
		log.Fatalf("write %s: %v", *noticesOut, err)
	}
	fmt.Fprintf(os.Stderr, "wiregen: wrote %s and %s at upstream %s\n", *wirecodesOut, *noticesOut, *upstream)
	// --- S4 tools (append-only; last block) ---
	var tbuf bytes.Buffer
	// Buffer first: a failing run emits nothing and leaves the previous
	// generated file untouched.
	if err := wirefacts.EmitTools(*upstream, *wireDir, *registryDir, &tbuf); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*toolsOut, tbuf.Bytes(), 0o644); err != nil {
		log.Fatalf("write %s: %v", *toolsOut, err)
	}
	fmt.Fprintf(os.Stderr, "wiregen: wrote %s at upstream %s\n", *toolsOut, *upstream)
}
