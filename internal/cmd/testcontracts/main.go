// Command testcontracts verifies the repository's test ownership and fuzz-corpus ledger.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	contractsPath           = "TEST_CONTRACTS.md"
	manifestPath            = "testdata/FUZZ_CORPUS.json"
	maintenanceBaselinePath = "docs/maintenance-baseline.json"
	maintenanceBaselineRef  = "d779a8165638da22d7c10b149e04ac637b9603cf"

	corpusBeginMarker = "<!-- BEGIN GENERATED FUZZ CORPUS LEDGER -->"
	corpusEndMarker   = "<!-- END GENERATED FUZZ CORPUS LEDGER -->"
)

var knownContracts = map[string]bool{
	"SYN": true, "STR": true, "NUM": true, "DEC": true, "ENC": true,
	"HOOK": true, "DOC": true, "STREAM": true, "XFORM": true,
	"OWN": true, "RES": true, "ROUTE": true, "API": true,
	"PERF": true, "TOOL": true,
}

type fuzzTarget struct {
	Package string
	Name    string
}

func (t fuzzTarget) key() string {
	return t.Package + "::" + t.Name
}

type corpusManifest struct {
	Version int           `json:"version"`
	Entries []corpusEntry `json:"entries"`
}

type corpusEntry struct {
	Path          string `json:"path"`
	OriginPackage string `json:"origin_package"`
	OriginTarget  string `json:"origin_target"`
	OwnerPackage  string `json:"owner_package"`
	OwnerTarget   string `json:"owner_target"`
	Bytes         int64  `json:"bytes"`
	SHA256        string `json:"sha256"`
	Status        string `json:"status"`
}

type baselineSourceArea struct {
	ProductionFiles int `json:"production_files"`
	ProductionLines int `json:"production_lines"`
	TestFiles       int `json:"test_files"`
	TestLines       int `json:"test_lines"`
}

type baselineAPI struct {
	DeclarationHeads         int `json:"declaration_heads"`
	VariablesAndConstants    int `json:"variables_and_constants"`
	FunctionsAndConstructors int `json:"functions_and_constructors"`
	Types                    int `json:"types"`
	Methods                  int `json:"methods"`
}

type baselineCorpusEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type maintenanceBaseline struct {
	SchemaVersion int    `json:"schema_version"`
	Purpose       string `json:"purpose"`
	Immutable     bool   `json:"immutable"`
	Repository    struct {
		Commit string `json:"commit"`
	} `json:"repository"`
	Source struct {
		Areas  map[string]baselineSourceArea `json:"areas"`
		Totals baselineSourceArea            `json:"totals"`
	} `json:"source"`
	ExportedAPI struct {
		Root baselineAPI `json:"root"`
		SIMD baselineAPI `json:"simd"`
	} `json:"exported_api"`
	Unsafe struct {
		GeneratedScopes          int `json:"generated_scopes"`
		ProductionFiles          int `json:"production_files"`
		FirstPassTargetMaxScopes int `json:"first_pass_target_max_scopes"`
	} `json:"unsafe"`
	Fuzz struct {
		Targets     int      `json:"targets"`
		TargetNames []string `json:"target_names"`
		DiskCorpus  struct {
			Files   int                   `json:"files"`
			Bytes   int64                 `json:"bytes"`
			Entries []baselineCorpusEntry `json:"entries"`
		} `json:"disk_corpus"`
	} `json:"fuzz"`
	Performance struct {
		PublicationFile   string `json:"publication_file"`
		PublicationCommit string `json:"publication_commit"`
	} `json:"performance"`
}

func main() {
	check := flag.Bool("check", false, "check test contracts, fuzz corpus, and maintenance baseline")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if !*check || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := checkRepository(*root); err != nil {
		fmt.Fprintln(os.Stderr, "testcontracts:", err)
		os.Exit(1)
	}
}

func checkRepository(root string) error {
	tracked, err := trackedFiles(root)
	if err != nil {
		return err
	}

	testFiles := filterTrackedTests(tracked)
	contracts, err := os.ReadFile(filepath.Join(root, contractsPath))
	if err != nil {
		return err
	}
	primary, err := parsePrimaryFileMap(contracts)
	if err != nil {
		return fmt.Errorf("primary file map: %w", err)
	}
	if err := reconcilePrimaryMap(testFiles, primary); err != nil {
		return fmt.Errorf("primary file map: %w", err)
	}

	targets, err := discoverFuzzTargets(root, testFiles)
	if err != nil {
		return err
	}
	ownership, err := parseFuzzOwnership(contracts)
	if err != nil {
		return fmt.Errorf("fuzz target ownership: %w", err)
	}
	if err := reconcileFuzzOwnership(targets, ownership); err != nil {
		return fmt.Errorf("fuzz target ownership: %w", err)
	}

	manifest, err := loadCorpusManifest(filepath.Join(root, manifestPath))
	if err != nil {
		return err
	}
	corpusFiles, err := filterTrackedCorpus(tracked)
	if err != nil {
		return err
	}
	if err := validateCorpusManifest(root, manifest, corpusFiles, targets); err != nil {
		return fmt.Errorf("fuzz corpus manifest: %w", err)
	}

	want := renderCorpusLedger(manifest.Entries)
	got, err := generatedBlock(contracts, corpusBeginMarker, corpusEndMarker)
	if err != nil {
		return fmt.Errorf("corpus migration ledger: %w", err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("corpus migration ledger is stale")
	}
	if err := validateMaintenanceBaseline(filepath.Join(root, maintenanceBaselinePath)); err != nil {
		return fmt.Errorf("maintenance baseline: %w", err)
	}
	return nil
}

func validateMaintenanceBaseline(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var baseline maintenanceBaseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if baseline.SchemaVersion != 1 || baseline.Purpose != "fixed pre-v1 simplification baseline" || !baseline.Immutable {
		return fmt.Errorf("invalid identity or schema")
	}
	if baseline.Repository.Commit != maintenanceBaselineRef {
		return fmt.Errorf("repository commit is %q, want %s", baseline.Repository.Commit, maintenanceBaselineRef)
	}

	var totals baselineSourceArea
	for name, area := range baseline.Source.Areas {
		if name == "" || area.ProductionFiles < 0 || area.ProductionLines < 0 || area.TestFiles < 0 || area.TestLines < 0 {
			return fmt.Errorf("invalid source area %q", name)
		}
		totals.ProductionFiles += area.ProductionFiles
		totals.ProductionLines += area.ProductionLines
		totals.TestFiles += area.TestFiles
		totals.TestLines += area.TestLines
	}
	if totals != baseline.Source.Totals {
		return fmt.Errorf("source totals are %+v, want %+v", baseline.Source.Totals, totals)
	}
	for name, api := range map[string]baselineAPI{"root": baseline.ExportedAPI.Root, "simd": baseline.ExportedAPI.SIMD} {
		want := api.VariablesAndConstants + api.FunctionsAndConstructors + api.Types + api.Methods
		if api.DeclarationHeads != want {
			return fmt.Errorf("%s API has %d declaration heads, components sum to %d", name, api.DeclarationHeads, want)
		}
	}
	if baseline.Unsafe.GeneratedScopes != 240 || baseline.Unsafe.ProductionFiles != 51 || baseline.Unsafe.FirstPassTargetMaxScopes != 156 {
		return fmt.Errorf("unsafe baseline changed: %+v", baseline.Unsafe)
	}
	if baseline.Fuzz.Targets != len(baseline.Fuzz.TargetNames) || !slices.IsSorted(baseline.Fuzz.TargetNames) {
		return fmt.Errorf("fuzz target count or ordering is invalid")
	}
	for i := 1; i < len(baseline.Fuzz.TargetNames); i++ {
		if baseline.Fuzz.TargetNames[i] == baseline.Fuzz.TargetNames[i-1] {
			return fmt.Errorf("duplicate fuzz target %q", baseline.Fuzz.TargetNames[i])
		}
	}
	if baseline.Fuzz.DiskCorpus.Files != len(baseline.Fuzz.DiskCorpus.Entries) {
		return fmt.Errorf("disk corpus file count is %d, want %d", baseline.Fuzz.DiskCorpus.Files, len(baseline.Fuzz.DiskCorpus.Entries))
	}
	var corpusBytes int64
	previousPath := ""
	for _, entry := range baseline.Fuzz.DiskCorpus.Entries {
		if entry.Path <= previousPath || entry.Bytes < 0 {
			return fmt.Errorf("invalid or unsorted baseline corpus path %q", entry.Path)
		}
		previousPath = entry.Path
		digest, err := hex.DecodeString(entry.SHA256)
		if err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("%s has invalid sha256 %q", entry.Path, entry.SHA256)
		}
		corpusBytes += entry.Bytes
	}
	if corpusBytes != baseline.Fuzz.DiskCorpus.Bytes {
		return fmt.Errorf("disk corpus bytes are %d, want %d", baseline.Fuzz.DiskCorpus.Bytes, corpusBytes)
	}
	if baseline.Performance.PublicationFile != "benchmarks/results/latest.json" || baseline.Performance.PublicationCommit != "b05b7ce145bb9a3c53301beb2619241180c786ce" {
		return fmt.Errorf("starting performance publication changed")
	}
	return nil
}

func trackedFiles(root string) ([]string, error) {
	// Include untracked, non-ignored files so the check works before staging and
	// rejects newly added tests or corpus seeds that have no ownership record.
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list tracked files: %w", err)
	}
	items := bytes.Split(out, []byte{0})
	files := make([]string, 0, len(items))
	for _, item := range items {
		if len(item) != 0 {
			files = append(files, filepath.ToSlash(string(item)))
		}
	}
	slices.Sort(files)
	return files, nil
}

func filterTrackedTests(tracked []string) []string {
	var tests []string
	for _, path := range tracked {
		if strings.HasSuffix(path, "_test.go") {
			tests = append(tests, path)
		}
	}
	return tests
}

func parsePrimaryFileMap(data []byte) (map[string]string, error) {
	section, err := markdownSection(data, "## Primary file map", "## Mixed files to separate")
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	contract := ""
	inFence := false
	for _, raw := range strings.Split(string(section), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "### `") && strings.HasSuffix(line, "`") {
			contract = strings.TrimSuffix(strings.TrimPrefix(line, "### `"), "`")
			if !knownContracts[contract] {
				return nil, fmt.Errorf("unknown contract %q", contract)
			}
			continue
		}
		if line == "```text" {
			if contract == "" {
				return nil, fmt.Errorf("file block has no contract heading")
			}
			inFence = true
			continue
		}
		if line == "```" {
			inFence = false
			continue
		}
		if !inFence || line == "" {
			continue
		}
		if !strings.HasSuffix(line, "_test.go") {
			return nil, fmt.Errorf("invalid test path %q", line)
		}
		if previous, exists := result[line]; exists {
			return nil, fmt.Errorf("%s is listed under both %s and %s", line, previous, contract)
		}
		result[line] = contract
	}
	return result, nil
}

func reconcilePrimaryMap(tracked []string, mapped map[string]string) error {
	trackedSet := make(map[string]bool, len(tracked))
	for _, path := range tracked {
		trackedSet[path] = true
	}
	var missing, stale []string
	for _, path := range tracked {
		if _, ok := mapped[path]; !ok {
			missing = append(missing, path)
		}
	}
	for path := range mapped {
		if !trackedSet[path] {
			stale = append(stale, path)
		}
	}
	slices.Sort(missing)
	slices.Sort(stale)
	if len(missing) != 0 || len(stale) != 0 {
		return fmt.Errorf("tracked set mismatch: missing=%v stale=%v", missing, stale)
	}
	return nil
}

func discoverFuzzTargets(root string, testFiles []string) ([]fuzzTarget, error) {
	var targets []fuzzTarget
	for _, path := range testFiles {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(path)), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		pkg := packageQualifier(filepath.ToSlash(filepath.Dir(path)))
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isFuzzTarget(fn) {
				continue
			}
			targets = append(targets, fuzzTarget{Package: pkg, Name: fn.Name.Name})
		}
	}
	slices.SortFunc(targets, func(a, b fuzzTarget) int {
		return strings.Compare(a.key(), b.key())
	})
	return targets, nil
}

func isFuzzTarget(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Fuzz") || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	param := fn.Type.Params.List[0]
	if len(param.Names) > 1 {
		return false
	}
	pointer, ok := param.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := pointer.X.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "F" && (fn.Type.Results == nil || len(fn.Type.Results.List) == 0)
}

func packageQualifier(dir string) string {
	if dir == "." || dir == "" {
		return "./"
	}
	return "./" + strings.TrimPrefix(dir, "./")
}

func parseFuzzOwnership(data []byte) (map[string]int, error) {
	section, err := markdownSection(data, "## Fuzz target ownership", "## Corpus migration ledger")
	if err != nil {
		return nil, err
	}
	result := make(map[string]int)
	for _, raw := range strings.Split(string(section), "\n") {
		cells, ok := markdownRow(raw)
		if !ok || len(cells) != 3 || cells[0] == "Package" || strings.HasPrefix(cells[0], "---") {
			continue
		}
		pkg := strings.Trim(cells[0], "`")
		name := strings.Trim(cells[1], "`")
		campaignText := strings.Trim(cells[2], "`")
		campaign, err := strconv.Atoi(campaignText)
		if err != nil || campaign < 1 || campaign > 10 {
			return nil, fmt.Errorf("%s::%s has unknown campaign %q", pkg, name, campaignText)
		}
		if !strings.HasPrefix(pkg, "./") || !strings.HasPrefix(name, "Fuzz") {
			return nil, fmt.Errorf("invalid target %s::%s", pkg, name)
		}
		key := fuzzTarget{Package: pkg, Name: name}.key()
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate target %s", key)
		}
		result[key] = campaign
	}
	return result, nil
}

func reconcileFuzzOwnership(targets []fuzzTarget, ownership map[string]int) error {
	want := make(map[string]bool, len(targets))
	for _, target := range targets {
		want[target.key()] = true
	}
	var missing, stale []string
	for key := range want {
		if _, ok := ownership[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range ownership {
		if !want[key] {
			stale = append(stale, key)
		}
	}
	slices.Sort(missing)
	slices.Sort(stale)
	if len(missing) != 0 || len(stale) != 0 {
		return fmt.Errorf("target set mismatch: missing=%v stale=%v", missing, stale)
	}
	return nil
}

func loadCorpusManifest(path string) (corpusManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return corpusManifest{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var manifest corpusManifest
	if err := decoder.Decode(&manifest); err != nil {
		return corpusManifest{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return corpusManifest{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return manifest, nil
}

func filterTrackedCorpus(tracked []string) ([]string, error) {
	var corpus []string
	for _, path := range tracked {
		_, _, found, err := corpusOwner(path)
		if err != nil {
			return nil, err
		}
		if found {
			corpus = append(corpus, path)
		}
	}
	return corpus, nil
}

func corpusOwner(path string) (pkg, target string, found bool, err error) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] != "testdata" || parts[i+1] != "fuzz" {
			continue
		}
		if len(parts) != i+4 || parts[i+2] == "" || parts[i+3] == "" {
			return "", "", false, fmt.Errorf("malformed fuzz corpus path %q", path)
		}
		return packageQualifier(strings.Join(parts[:i], "/")), parts[i+2], true, nil
	}
	return "", "", false, nil
}

func validateCorpusManifest(root string, manifest corpusManifest, corpusFiles []string, targets []fuzzTarget) error {
	if manifest.Version != 1 {
		return fmt.Errorf("unsupported version %d", manifest.Version)
	}
	targetSet := make(map[string]bool, len(targets))
	for _, target := range targets {
		targetSet[target.key()] = true
	}
	corpusSet := make(map[string]bool, len(corpusFiles))
	for _, path := range corpusFiles {
		corpusSet[path] = true
	}

	seen := make(map[string]bool, len(manifest.Entries))
	previous := ""
	for _, entry := range manifest.Entries {
		if entry.Path <= previous && previous != "" {
			return fmt.Errorf("entries are not sorted by path at %q", entry.Path)
		}
		previous = entry.Path
		if seen[entry.Path] {
			return fmt.Errorf("duplicate path %q", entry.Path)
		}
		seen[entry.Path] = true
		if !corpusSet[entry.Path] {
			return fmt.Errorf("untracked or missing seed %q", entry.Path)
		}

		pkg, target, found, err := corpusOwner(entry.Path)
		if err != nil || !found {
			if err == nil {
				err = fmt.Errorf("not a fuzz corpus path")
			}
			return fmt.Errorf("%s: %w", entry.Path, err)
		}
		if entry.OwnerPackage != pkg || entry.OwnerTarget != target {
			return fmt.Errorf("%s owner is %s::%s, want %s::%s", entry.Path, entry.OwnerPackage, entry.OwnerTarget, pkg, target)
		}
		if !targetSet[fuzzTarget{Package: pkg, Name: target}.key()] {
			return fmt.Errorf("%s has unknown owner target %s::%s", entry.Path, pkg, target)
		}
		if !strings.HasPrefix(entry.OriginPackage, "./") || !strings.HasPrefix(entry.OriginTarget, "Fuzz") {
			return fmt.Errorf("%s has invalid origin %s::%s", entry.Path, entry.OriginPackage, entry.OriginTarget)
		}
		if entry.Status != "retained" && entry.Status != "migrated" {
			return fmt.Errorf("%s has invalid status %q", entry.Path, entry.Status)
		}

		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(data, []byte("go test fuzz v1\n")) {
			return fmt.Errorf("%s has invalid fuzz corpus header", entry.Path)
		}
		if int64(len(data)) != entry.Bytes {
			return fmt.Errorf("%s byte count is %d, want %d", entry.Path, len(data), entry.Bytes)
		}
		digest := sha256.Sum256(data)
		gotDigest := hex.EncodeToString(digest[:])
		if gotDigest != entry.SHA256 {
			return fmt.Errorf("%s sha256 is %s, want %s", entry.Path, gotDigest, entry.SHA256)
		}
	}
	var missing []string
	for _, path := range corpusFiles {
		if !seen[path] {
			missing = append(missing, path)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("seeds missing from manifest: %v", missing)
	}
	return nil
}

func renderCorpusLedger(entries []corpusEntry) []byte {
	entries = slices.Clone(entries)
	slices.SortFunc(entries, func(a, b corpusEntry) int {
		return strings.Compare(a.Path, b.Path)
	})
	var out bytes.Buffer
	fmt.Fprintln(&out, corpusBeginMarker)
	fmt.Fprintln(&out, "<!-- Generated from testdata/FUZZ_CORPUS.json by internal/cmd/testcontracts. -->")
	fmt.Fprintln(&out, "| Origin | Current owner | Corpus file | Bytes | SHA-256 | Status |")
	fmt.Fprintln(&out, "| --- | --- | --- | ---: | --- | --- |")
	for _, entry := range entries {
		fmt.Fprintf(&out, "| `%s::%s` | `%s::%s` | `%s` | %d | `%s` | %s |\n",
			entry.OriginPackage, entry.OriginTarget, entry.OwnerPackage, entry.OwnerTarget,
			entry.Path, entry.Bytes, entry.SHA256, entry.Status)
	}
	fmt.Fprintln(&out, corpusEndMarker)
	return out.Bytes()
}

func markdownSection(data []byte, startHeading, endHeading string) ([]byte, error) {
	start := bytes.Index(data, []byte(startHeading+"\n"))
	if start < 0 {
		return nil, fmt.Errorf("missing %s", startHeading)
	}
	start += len(startHeading) + 1
	end := bytes.Index(data[start:], []byte(endHeading+"\n"))
	if end < 0 {
		return nil, fmt.Errorf("missing %s", endHeading)
	}
	return data[start : start+end], nil
}

func markdownRow(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil, false
	}
	parts := strings.Split(strings.Trim(line, "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, true
}

func generatedBlock(data []byte, begin, end string) ([]byte, error) {
	start := bytes.Index(data, []byte(begin))
	if start < 0 {
		return nil, fmt.Errorf("missing %s", begin)
	}
	finish := bytes.Index(data[start:], []byte(end))
	if finish < 0 {
		return nil, fmt.Errorf("missing %s", end)
	}
	finish += start + len(end)
	if finish < len(data) && data[finish] == '\n' {
		finish++
	}
	return data[start:finish], nil
}
