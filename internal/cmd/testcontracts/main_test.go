package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrimaryMapExactSet(t *testing.T) {
	doc := []byte("" +
		"## Primary file map\n\n" +
		"### `SYN`\n\n```text\na_test.go\n```\n\n" +
		"### `TOOL`\n\n```text\ninternal/tool/main_test.go\n```\n\n" +
		"## Mixed files to separate\n")
	mapped, err := parsePrimaryFileMap(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcilePrimaryMap([]string{"a_test.go", "internal/tool/main_test.go"}, mapped); err != nil {
		t.Fatal(err)
	}
}

func TestPrimaryMapRejectsMissingDuplicateAndStaleFiles(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		if err := reconcilePrimaryMap([]string{"missing_test.go"}, map[string]string{}); err == nil {
			t.Fatal("accepted an unmapped tracked test")
		}
	})
	t.Run("stale", func(t *testing.T) {
		if err := reconcilePrimaryMap(nil, map[string]string{"deleted_test.go": "SYN"}); err == nil {
			t.Fatal("accepted a stale mapped test")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		doc := []byte("" +
			"## Primary file map\n\n" +
			"### `SYN`\n\n```text\na_test.go\n```\n\n" +
			"### `STR`\n\n```text\na_test.go\n```\n\n" +
			"## Mixed files to separate\n")
		if _, err := parsePrimaryFileMap(doc); err == nil {
			t.Fatal("accepted a duplicate mapped test")
		}
	})
}

func TestFuzzOwnershipExactSet(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"root_test.go":         "package root\nimport \"testing\"\nfunc FuzzRoot(f *testing.F) {}\n",
		"simd/kernel_test.go":  "package simd\nimport \"testing\"\nfunc FuzzKernel(f *testing.F) {}\n",
		"plain/helper_test.go": "package plain\nfunc TestHelper() {}\n",
	}
	var paths []string
	for path, source := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	targets, err := discoverFuzzTargets(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	doc := []byte("" +
		"## Fuzz target ownership\n\n" +
		"| Package | Target | Campaign |\n| --- | --- | ---: |\n" +
		"| `./` | `FuzzRoot` | 1 |\n" +
		"| `./simd` | `FuzzKernel` | 10 |\n\n" +
		"## Corpus migration ledger\n")
	ownership, err := parseFuzzOwnership(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileFuzzOwnership(targets, ownership); err != nil {
		t.Fatal(err)
	}
}

func TestFuzzOwnershipRejectsMissingDuplicateAndUnknownCampaign(t *testing.T) {
	target := []fuzzTarget{{Package: "./", Name: "FuzzRoot"}}
	t.Run("missing", func(t *testing.T) {
		if err := reconcileFuzzOwnership(target, map[string]int{}); err == nil {
			t.Fatal("accepted a fuzz target without ownership")
		}
	})
	t.Run("stale", func(t *testing.T) {
		if err := reconcileFuzzOwnership(nil, map[string]int{"./::FuzzGone": 1}); err == nil {
			t.Fatal("accepted stale fuzz ownership")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		doc := ownershipDocument("" +
			"| `./` | `FuzzRoot` | 1 |\n" +
			"| `./` | `FuzzRoot` | 2 |\n")
		if _, err := parseFuzzOwnership(doc); err == nil {
			t.Fatal("accepted duplicate fuzz ownership")
		}
	})
	t.Run("unknown campaign", func(t *testing.T) {
		doc := ownershipDocument("| `./` | `FuzzRoot` | 11 |\n")
		if _, err := parseFuzzOwnership(doc); err == nil {
			t.Fatal("accepted unknown campaign")
		}
	})
}

func TestCorpusManifestAcceptsCurrentFormat(t *testing.T) {
	root := t.TempDir()
	entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzRoot/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
	manifest := corpusManifest{Version: 1, Entries: []corpusEntry{entry}}
	if err := validateCorpusManifest(root, manifest, []string{entry.Path}, []fuzzTarget{{Package: "./", Name: "FuzzRoot"}}); err != nil {
		t.Fatal(err)
	}
}

func TestCorpusManifestRejectsOrphanMissingDigestDriftAndUnknownTarget(t *testing.T) {
	t.Run("orphan seed", func(t *testing.T) {
		root := t.TempDir()
		entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzRoot/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
		err := validateCorpusManifest(root, corpusManifest{Version: 1}, []string{entry.Path}, []fuzzTarget{{Package: "./", Name: "FuzzRoot"}})
		if err == nil || !strings.Contains(err.Error(), "missing from manifest") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing seed", func(t *testing.T) {
		root := t.TempDir()
		entry := corpusEntry{
			Path: "testdata/fuzz/FuzzRoot/deleted", OriginPackage: "./", OriginTarget: "FuzzRoot",
			OwnerPackage: "./", OwnerTarget: "FuzzRoot", Status: "retained",
		}
		err := validateCorpusManifest(root, corpusManifest{Version: 1, Entries: []corpusEntry{entry}}, nil, []fuzzTarget{{Package: "./", Name: "FuzzRoot"}})
		if err == nil || !strings.Contains(err.Error(), "untracked or missing") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("digest drift", func(t *testing.T) {
		root := t.TempDir()
		entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzRoot/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
		entry.SHA256 = strings.Repeat("0", 64)
		err := validateCorpusManifest(root, corpusManifest{Version: 1, Entries: []corpusEntry{entry}}, []string{entry.Path}, []fuzzTarget{{Package: "./", Name: "FuzzRoot"}})
		if err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("byte drift", func(t *testing.T) {
		root := t.TempDir()
		entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzRoot/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(entry.Path)), []byte("go test fuzz v1\n[]byte(\"changed\")\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := validateCorpusManifest(root, corpusManifest{Version: 1, Entries: []corpusEntry{entry}}, []string{entry.Path}, []fuzzTarget{{Package: "./", Name: "FuzzRoot"}})
		if err == nil {
			t.Fatal("accepted mutated seed")
		}
	})
	t.Run("unknown target", func(t *testing.T) {
		root := t.TempDir()
		entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzGone/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
		err := validateCorpusManifest(root, corpusManifest{Version: 1, Entries: []corpusEntry{entry}}, []string{entry.Path}, nil)
		if err == nil || !strings.Contains(err.Error(), "unknown owner target") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestCorpusLedgerRenderIsDeterministic(t *testing.T) {
	a := corpusEntry{Path: "testdata/fuzz/FuzzRoot/a", OriginPackage: "./", OriginTarget: "FuzzRoot", OwnerPackage: "./", OwnerTarget: "FuzzRoot", Status: "retained"}
	b := corpusEntry{Path: "testdata/fuzz/FuzzRoot/b", OriginPackage: "./", OriginTarget: "FuzzRoot", OwnerPackage: "./", OwnerTarget: "FuzzRoot", Status: "retained"}
	forward := renderCorpusLedger([]corpusEntry{a, b})
	reverse := renderCorpusLedger([]corpusEntry{b, a})
	if string(forward) != string(reverse) {
		t.Fatal("ledger rendering depends on manifest order")
	}
	block, err := generatedBlock(append([]byte("prefix\n"), append(forward, []byte("suffix\n")...)...), corpusBeginMarker, corpusEndMarker)
	if err != nil {
		t.Fatal(err)
	}
	if string(block) != string(forward) {
		t.Fatal("generated block did not round-trip")
	}
}

func ownershipDocument(rows string) []byte {
	return []byte("" +
		"## Fuzz target ownership\n\n" +
		"| Package | Target | Campaign |\n| --- | --- | ---: |\n" + rows + "\n" +
		"## Corpus migration ledger\n")
}

func writeCorpusEntry(t *testing.T, root, path string, data []byte) corpusEntry {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return corpusEntry{
		Path: path, OriginPackage: "./", OriginTarget: "FuzzRoot",
		OwnerPackage: "./", OwnerTarget: strings.Split(filepath.ToSlash(path), "/")[2],
		Bytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), Status: "retained",
	}
}
