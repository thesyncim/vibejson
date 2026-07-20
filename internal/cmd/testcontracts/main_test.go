package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	manifest := corpusManifest{Version: 2, Entries: []corpusEntry{entry}}
	if err := validateCorpusManifest(root, manifest, []string{entry.Path}, []fuzzTarget{{Package: "./", Name: "FuzzRoot"}}, baselineCorpusFor(entry)); err != nil {
		t.Fatal(err)
	}
}

func TestCorpusManifestRejectsOrphanMissingDigestDriftAndUnknownTarget(t *testing.T) {
	t.Run("orphan seed", func(t *testing.T) {
		root := t.TempDir()
		entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzRoot/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
		err := validateCorpusManifest(root, corpusManifest{Version: 2}, []string{entry.Path}, []fuzzTarget{{Package: "./", Name: "FuzzRoot"}}, nil)
		if err == nil || !strings.Contains(err.Error(), "missing from manifest") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing seed", func(t *testing.T) {
		root := t.TempDir()
		entry := corpusEntry{
			Path: "testdata/fuzz/FuzzRoot/deleted", OriginPath: "testdata/fuzz/FuzzRoot/deleted",
			OriginPackage: "./", OriginTarget: "FuzzRoot", OwnerPackage: "./", OwnerTarget: "FuzzRoot", Status: "retained",
		}
		err := validateCorpusManifest(root, corpusManifest{Version: 2, Entries: []corpusEntry{entry}}, nil,
			[]fuzzTarget{{Package: "./", Name: "FuzzRoot"}}, baselineCorpusFor(entry))
		if err == nil || !strings.Contains(err.Error(), "untracked or missing") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("digest drift", func(t *testing.T) {
		root := t.TempDir()
		entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzRoot/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
		entry.SHA256 = strings.Repeat("0", 64)
		err := validateCorpusManifest(root, corpusManifest{Version: 2, Entries: []corpusEntry{entry}}, []string{entry.Path},
			[]fuzzTarget{{Package: "./", Name: "FuzzRoot"}}, baselineCorpusFor(entry))
		if err == nil {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("byte drift", func(t *testing.T) {
		root := t.TempDir()
		entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzRoot/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(entry.Path)), []byte("go test fuzz v1\n[]byte(\"changed\")\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := validateCorpusManifest(root, corpusManifest{Version: 2, Entries: []corpusEntry{entry}}, []string{entry.Path},
			[]fuzzTarget{{Package: "./", Name: "FuzzRoot"}}, baselineCorpusFor(entry))
		if err == nil {
			t.Fatal("accepted mutated seed")
		}
	})
	t.Run("unknown target", func(t *testing.T) {
		root := t.TempDir()
		entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzGone/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
		err := validateCorpusManifest(root, corpusManifest{Version: 2, Entries: []corpusEntry{entry}}, []string{entry.Path}, nil, baselineCorpusFor(entry))
		if err == nil || !strings.Contains(err.Error(), "unknown owner target") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestCorpusManifestRejectsBaselineLineageDrift(t *testing.T) {
	t.Run("missing origin", func(t *testing.T) {
		root := t.TempDir()
		entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzRoot/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
		err := validateCorpusManifest(root, corpusManifest{Version: 2}, nil, nil, baselineCorpusFor(entry))
		if err == nil || !strings.Contains(err.Error(), "missing current descendants") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("duplicate origin", func(t *testing.T) {
		root := t.TempDir()
		origin := writeCorpusEntry(t, root, "testdata/fuzz/FuzzRoot/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
		migrated := writeCorpusEntry(t, root, "testdata/fuzz/FuzzSecond/seed", []byte("go test fuzz v1\n[]byte(\"changed\")\n"))
		migrated.OriginPath = origin.OriginPath
		migrated.OriginPackage = origin.OriginPackage
		migrated.OriginTarget = origin.OriginTarget
		migrated.Status = "migrated"
		entries := []corpusEntry{origin, migrated}
		err := validateCorpusManifest(root, corpusManifest{Version: 2, Entries: entries},
			[]string{migrated.Path, origin.Path},
			[]fuzzTarget{{Package: "./", Name: "FuzzRoot"}, {Package: "./", Name: "FuzzSecond"}},
			baselineCorpusFor(origin))
		if err == nil || !strings.Contains(err.Error(), "multiple current descendants") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("fabricated origin", func(t *testing.T) {
		root := t.TempDir()
		entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzRoot/seed", []byte("go test fuzz v1\n[]byte(\"ok\")\n"))
		baseline := baselineCorpusFor(entry)
		entry.OriginPath = "testdata/fuzz/FuzzFabricated/seed"
		entry.OriginTarget = "FuzzFabricated"
		entry.Status = "migrated"
		err := validateCorpusManifest(root, corpusManifest{Version: 2, Entries: []corpusEntry{entry}}, []string{entry.Path},
			[]fuzzTarget{{Package: "./", Name: "FuzzRoot"}}, baseline)
		if err == nil || !strings.Contains(err.Error(), "fabricated baseline origin") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("retained drift", func(t *testing.T) {
		root := t.TempDir()
		entry := writeCorpusEntry(t, root, "testdata/fuzz/FuzzRoot/seed", []byte("go test fuzz v1\n[]byte(\"old\")\n"))
		baseline := baselineCorpusFor(entry)
		data := []byte("go test fuzz v1\n[]byte(\"new\")\n")
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(entry.Path)), data, 0o644); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		entry.Bytes = int64(len(data))
		entry.SHA256 = hex.EncodeToString(digest[:])
		err := validateCorpusManifest(root, corpusManifest{Version: 2, Entries: []corpusEntry{entry}}, []string{entry.Path},
			[]fuzzTarget{{Package: "./", Name: "FuzzRoot"}}, baseline)
		if err == nil || !strings.Contains(err.Error(), "retained seed differs") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestCorpusLedgerRenderIsDeterministic(t *testing.T) {
	a := corpusEntry{Path: "testdata/fuzz/FuzzRoot/a", OriginPath: "testdata/fuzz/FuzzRoot/a", OriginPackage: "./", OriginTarget: "FuzzRoot", OwnerPackage: "./", OwnerTarget: "FuzzRoot", Status: "retained"}
	b := corpusEntry{Path: "testdata/fuzz/FuzzRoot/b", OriginPath: "testdata/fuzz/FuzzRoot/b", OriginPackage: "./", OriginTarget: "FuzzRoot", OwnerPackage: "./", OwnerTarget: "FuzzRoot", Status: "retained"}
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

func TestMaintenanceBaselineAcceptsFixedRecord(t *testing.T) {
	path := filepath.Join("..", "..", "..", filepath.FromSlash(maintenanceBaselinePath))
	if err := validateMaintenanceBaseline(path); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceBaselineRejectsByteDrift(t *testing.T) {
	path := filepath.Join("..", "..", "..", filepath.FromSlash(maintenanceBaselinePath))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"whitespace":    append(bytes.Clone(data), '\n'),
		"unknown field": bytes.Replace(data, []byte("{\n"), []byte("{\n  \"ignored\": true,\n"), 1),
	}
	for name, mutated := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateMaintenanceBaseline(writeMaintenanceBaselineData(t, mutated)); err == nil || !strings.Contains(err.Error(), "sha256") {
				t.Fatalf("error = %v, want sha256 drift", err)
			}
		})
	}
}

func TestMaintenanceBaselineRejectsDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*maintenanceBaseline)
	}{
		{"identity", func(b *maintenanceBaseline) { b.Repository.Commit = "moving" }},
		{"source totals", func(b *maintenanceBaseline) { b.Source.Totals.TestLines++ }},
		{"API totals", func(b *maintenanceBaseline) { b.ExportedAPI.Root.Methods++ }},
		{"unsafe count", func(b *maintenanceBaseline) { b.Unsafe.GeneratedScopes++ }},
		{"fuzz target order", func(b *maintenanceBaseline) { b.Fuzz.TargetNames = []string{"FuzzB", "FuzzA"}; b.Fuzz.Targets = 2 }},
		{"corpus digest", func(b *maintenanceBaseline) { b.Fuzz.DiskCorpus.Entries[0].SHA256 = "bad" }},
		{"performance source", func(b *maintenanceBaseline) { b.Performance.PublicationCommit = "moving" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseline := validMaintenanceBaseline()
			test.mutate(&baseline)
			data, err := json.Marshal(baseline)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateMaintenanceBaselineData("maintenance-baseline.json", data); err == nil {
				t.Fatal("accepted a mutated maintenance baseline")
			}
		})
	}
}

func validMaintenanceBaseline() maintenanceBaseline {
	var baseline maintenanceBaseline
	baseline.SchemaVersion = 1
	baseline.Purpose = "fixed pre-v1 simplification baseline"
	baseline.Immutable = true
	baseline.Repository.Commit = maintenanceBaselineRef
	baseline.Source.Areas = map[string]baselineSourceArea{"root": {ProductionFiles: 1, ProductionLines: 2, TestFiles: 3, TestLines: 4}}
	baseline.Source.Totals = baseline.Source.Areas["root"]
	baseline.ExportedAPI.Root = baselineAPI{DeclarationHeads: 4, VariablesAndConstants: 1, FunctionsAndConstructors: 1, Types: 1, Methods: 1}
	baseline.ExportedAPI.SIMD = baselineAPI{DeclarationHeads: 4, VariablesAndConstants: 1, FunctionsAndConstructors: 1, Types: 1, Methods: 1}
	baseline.Unsafe.GeneratedScopes = 240
	baseline.Unsafe.ProductionFiles = 51
	baseline.Unsafe.FirstPassTargetMaxScopes = 156
	baseline.Fuzz.Targets = 1
	baseline.Fuzz.TargetNames = []string{"FuzzA"}
	digest := sha256.Sum256([]byte("x"))
	baseline.Fuzz.DiskCorpus.Files = 1
	baseline.Fuzz.DiskCorpus.Bytes = 1
	baseline.Fuzz.DiskCorpus.Entries = []baselineCorpusEntry{{Path: "testdata/fuzz/FuzzA/a", Bytes: 1, SHA256: hex.EncodeToString(digest[:])}}
	baseline.Performance.PublicationFile = "benchmarks/results/latest.json"
	baseline.Performance.PublicationCommit = "b05b7ce145bb9a3c53301beb2619241180c786ce"
	return baseline
}

func writeMaintenanceBaselineData(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "maintenance-baseline.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
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
		Path: path, OriginPath: path, OriginPackage: "./", OriginTarget: strings.Split(filepath.ToSlash(path), "/")[2],
		OwnerPackage: "./", OwnerTarget: strings.Split(filepath.ToSlash(path), "/")[2],
		Bytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), Status: "retained",
	}
}

func baselineCorpusFor(entries ...corpusEntry) []baselineCorpusEntry {
	baseline := make([]baselineCorpusEntry, len(entries))
	for i, entry := range entries {
		baseline[i] = baselineCorpusEntry{Path: entry.OriginPath, Bytes: entry.Bytes, SHA256: entry.SHA256}
	}
	return baseline
}
