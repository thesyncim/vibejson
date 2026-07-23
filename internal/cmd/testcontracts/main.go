// Command testcontracts verifies the repository's test ownership, fuzz-corpus
// and provenance ledgers, and fixed maintenance baseline.
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
	"go/scanner"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	contractsPath           = "TEST_CONTRACTS.md"
	manifestPath            = "testdata/FUZZ_CORPUS.json"
	maintenanceBaselinePath = "docs/maintenance-baseline.json"
	provenancePath          = "docs/provenance.md"
	identityMigrationPath   = "MIGRATION.md"
	currentModulePath       = "github.com/thesyncim/slopjson"
	formerProjectName       = "simd" + "json"
	formerModulePath        = "github.com/thesyncim/" + formerProjectName
	maintenanceBaselineRef  = "d779a8165638da22d7c10b149e04ac637b9603cf"
	maintenanceBaselineSHA  = "cf6c6b1b8ccafff89f0334a423a52c0fdc5be996abf5f1a7a641dc317ea95e80"

	corpusBeginMarker = "<!-- BEGIN GENERATED FUZZ CORPUS LEDGER -->"
	corpusEndMarker   = "<!-- END GENERATED FUZZ CORPUS LEDGER -->"
)

var provenanceIDPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:-[A-Z][A-Z0-9]*)*-[0-9]{3}$`)

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

type provenanceMarker struct {
	ID   string
	Path string
	Line int
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
	OriginPath    string `json:"origin_path"`
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
		BaselineCommit string `json:"baseline_commit"`
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
	if err := validateRepositoryIdentity(root, tracked); err != nil {
		return fmt.Errorf("repository identity: %w", err)
	}
	if err := validateProvenance(root, tracked); err != nil {
		return fmt.Errorf("provenance: %w", err)
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

	baseline, err := loadMaintenanceBaseline(filepath.Join(root, maintenanceBaselinePath))
	if err != nil {
		return fmt.Errorf("maintenance baseline: %w", err)
	}

	manifest, err := loadCorpusManifest(filepath.Join(root, manifestPath))
	if err != nil {
		return err
	}
	corpusFiles, err := filterTrackedCorpus(tracked)
	if err != nil {
		return err
	}
	if err := validateCorpusManifest(root, manifest, corpusFiles, targets, baseline.Fuzz.DiskCorpus.Entries); err != nil {
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
	return nil
}

func validateRepositoryIdentity(root string, tracked []string) error {
	modules := map[string]string{
		"go.mod":              currentModulePath,
		"benchmarks/go.mod":   currentModulePath + "/benchmarks",
		"tests/stdlib/go.mod": currentModulePath + "/tests/stdlib",
	}
	for path, module := range modules {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		line, _, _ := bytes.Cut(data, []byte{'\n'})
		if got, want := string(line), "module "+module; got != want {
			return fmt.Errorf("%s starts with %q, want %q", path, got, want)
		}
	}

	migration, err := os.ReadFile(filepath.Join(root, identityMigrationPath))
	if err != nil {
		return fmt.Errorf("read %s: %w", identityMigrationPath, err)
	}
	if count := bytes.Count(migration, []byte(formerModulePath)); count != 1 {
		return fmt.Errorf("%s contains the former module path %d times, want 1", identityMigrationPath, count)
	}

	forbidden := [...]string{
		"SIMD" + "JSON_",
		formerProjectName + "-",
		formerProjectName + "_",
		formerProjectName + ".",
		formerProjectName + ":",
	}
	for _, path := range tracked {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if path != identityMigrationPath {
			if bytes.Contains(data, []byte(formerModulePath)) {
				return fmt.Errorf("%s retains the former module path", path)
			}
			for _, fragment := range forbidden {
				if bytes.Contains(data, []byte(fragment)) {
					return fmt.Errorf("%s retains repository identity fragment %q", path, fragment)
				}
			}
		}
		if filepath.Ext(path) != ".go" {
			continue
		}
		fset := token.NewFileSet()
		file := fset.AddFile(path, fset.Base(), len(data))
		var scan scanner.Scanner
		scan.Init(file, data, nil, 0)
		for {
			pos, tok, literal := scan.Scan()
			if tok == token.EOF {
				break
			}
			if tok == token.IDENT && (literal == formerProjectName || literal == formerProjectName+"_test") {
				return fmt.Errorf("%s:%d retains identifier %q", path, file.Line(pos), literal)
			}
		}
	}
	return nil
}

func validateProvenance(root string, tracked []string) error {
	data, err := os.ReadFile(filepath.Join(root, provenancePath))
	if err != nil {
		return err
	}
	ledger, err := parseProvenanceLedger(data)
	if err != nil {
		return err
	}
	markers, err := discoverProvenanceMarkers(root, tracked)
	if err != nil {
		return err
	}
	return reconcileProvenance(ledger, markers)
}

func parseProvenanceLedger(data []byte) (map[string]bool, error) {
	sections := [][2]string{
		{"## Source and algorithm ledger", "## Generated material and corpora"},
		{"## Generated material and corpora", "## Unresolved origins"},
	}
	ledger := make(map[string]bool)
	for _, headings := range sections {
		section, err := markdownSection(data, headings[0], headings[1])
		if err != nil {
			return nil, err
		}
		for _, raw := range strings.Split(string(section), "\n") {
			cells, ok := markdownRow(raw)
			if !ok || len(cells) == 0 || cells[0] == "ID" || strings.HasPrefix(cells[0], "---") {
				continue
			}
			cell := cells[0]
			if len(cell) < 3 || cell[0] != '`' || cell[len(cell)-1] != '`' {
				return nil, fmt.Errorf("malformed provenance ledger ID %q", cell)
			}
			id := cell[1 : len(cell)-1]
			if !provenanceIDPattern.MatchString(id) {
				return nil, fmt.Errorf("malformed provenance ledger ID %q", id)
			}
			if ledger[id] {
				return nil, fmt.Errorf("duplicate provenance ledger ID %q", id)
			}
			ledger[id] = true
		}
	}
	if len(ledger) == 0 {
		return nil, fmt.Errorf("provenance ledger is empty")
	}
	return ledger, nil
}

func discoverProvenanceMarkers(root string, tracked []string) ([]provenanceMarker, error) {
	// Keep the label split in this command so its parser fixtures are not
	// themselves mistaken for material-site markers.
	label := "Provenance" + ":"
	var markers []provenanceMarker
	for _, path := range tracked {
		if path == provenancePath {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		for lineNumber, line := range bytes.Split(data, []byte{'\n'}) {
			rest := string(line)
			for {
				at := strings.Index(rest, label)
				if at < 0 {
					break
				}
				rest = strings.TrimSpace(rest[at+len(label):])
				end := 0
				for end < len(rest) && (rest[end] == '-' || rest[end] >= '0' && rest[end] <= '9' || rest[end] >= 'A' && rest[end] <= 'Z' || rest[end] >= 'a' && rest[end] <= 'z') {
					end++
				}
				id := rest[:end]
				if !provenanceIDPattern.MatchString(id) {
					return nil, fmt.Errorf("%s:%d has malformed provenance marker %q", path, lineNumber+1, id)
				}
				markers = append(markers, provenanceMarker{ID: id, Path: path, Line: lineNumber + 1})
				rest = rest[end:]
			}
		}
	}
	return markers, nil
}

func reconcileProvenance(ledger map[string]bool, markers []provenanceMarker) error {
	marked := make(map[string]bool, len(markers))
	var orphan []string
	for _, marker := range markers {
		if !ledger[marker.ID] {
			orphan = append(orphan, fmt.Sprintf("%s:%d=%s", marker.Path, marker.Line, marker.ID))
			continue
		}
		marked[marker.ID] = true
	}
	var missing []string
	for id := range ledger {
		if !marked[id] {
			missing = append(missing, id)
		}
	}
	slices.Sort(missing)
	slices.Sort(orphan)
	if len(missing) != 0 || len(orphan) != 0 {
		return fmt.Errorf("ledger/marker mismatch: missing=%v orphan=%v", missing, orphan)
	}
	return nil
}

func validateMaintenanceBaseline(path string) error {
	_, err := loadMaintenanceBaseline(path)
	return err
}

func loadMaintenanceBaseline(path string) (maintenanceBaseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return maintenanceBaseline{}, err
	}
	digest := sha256.Sum256(data)
	gotSHA := hex.EncodeToString(digest[:])
	if gotSHA != maintenanceBaselineSHA {
		return maintenanceBaseline{}, fmt.Errorf("%s sha256 is %s, want %s", path, gotSHA, maintenanceBaselineSHA)
	}
	baseline, err := decodeMaintenanceBaseline(path, data)
	if err != nil {
		return maintenanceBaseline{}, err
	}
	return baseline, nil
}

func validateMaintenanceBaselineData(path string, data []byte) error {
	_, err := decodeMaintenanceBaseline(path, data)
	return err
}

func decodeMaintenanceBaseline(path string, data []byte) (maintenanceBaseline, error) {
	var baseline maintenanceBaseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return maintenanceBaseline{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if baseline.SchemaVersion != 1 || baseline.Purpose != "fixed pre-v1 simplification baseline" || !baseline.Immutable {
		return maintenanceBaseline{}, fmt.Errorf("invalid identity or schema")
	}
	if baseline.Repository.Commit != maintenanceBaselineRef {
		return maintenanceBaseline{}, fmt.Errorf("repository commit is %q, want %s", baseline.Repository.Commit, maintenanceBaselineRef)
	}

	var totals baselineSourceArea
	for name, area := range baseline.Source.Areas {
		if name == "" || area.ProductionFiles < 0 || area.ProductionLines < 0 || area.TestFiles < 0 || area.TestLines < 0 {
			return maintenanceBaseline{}, fmt.Errorf("invalid source area %q", name)
		}
		totals.ProductionFiles += area.ProductionFiles
		totals.ProductionLines += area.ProductionLines
		totals.TestFiles += area.TestFiles
		totals.TestLines += area.TestLines
	}
	if totals != baseline.Source.Totals {
		return maintenanceBaseline{}, fmt.Errorf("source totals are %+v, want %+v", baseline.Source.Totals, totals)
	}
	for name, api := range map[string]baselineAPI{"root": baseline.ExportedAPI.Root, "simd": baseline.ExportedAPI.SIMD} {
		want := api.VariablesAndConstants + api.FunctionsAndConstructors + api.Types + api.Methods
		if api.DeclarationHeads != want {
			return maintenanceBaseline{}, fmt.Errorf("%s API has %d declaration heads, components sum to %d", name, api.DeclarationHeads, want)
		}
	}
	if baseline.Unsafe.GeneratedScopes != 240 || baseline.Unsafe.ProductionFiles != 51 || baseline.Unsafe.FirstPassTargetMaxScopes != 156 {
		return maintenanceBaseline{}, fmt.Errorf("unsafe baseline changed: %+v", baseline.Unsafe)
	}
	if baseline.Fuzz.Targets != len(baseline.Fuzz.TargetNames) || !slices.IsSorted(baseline.Fuzz.TargetNames) {
		return maintenanceBaseline{}, fmt.Errorf("fuzz target count or ordering is invalid")
	}
	for i := 1; i < len(baseline.Fuzz.TargetNames); i++ {
		if baseline.Fuzz.TargetNames[i] == baseline.Fuzz.TargetNames[i-1] {
			return maintenanceBaseline{}, fmt.Errorf("duplicate fuzz target %q", baseline.Fuzz.TargetNames[i])
		}
	}
	if baseline.Fuzz.DiskCorpus.Files != len(baseline.Fuzz.DiskCorpus.Entries) {
		return maintenanceBaseline{}, fmt.Errorf("disk corpus file count is %d, want %d", baseline.Fuzz.DiskCorpus.Files, len(baseline.Fuzz.DiskCorpus.Entries))
	}
	var corpusBytes int64
	previousPath := ""
	for _, entry := range baseline.Fuzz.DiskCorpus.Entries {
		if entry.Path <= previousPath || entry.Bytes < 0 {
			return maintenanceBaseline{}, fmt.Errorf("invalid or unsorted baseline corpus path %q", entry.Path)
		}
		previousPath = entry.Path
		digest, err := hex.DecodeString(entry.SHA256)
		if err != nil || len(digest) != sha256.Size {
			return maintenanceBaseline{}, fmt.Errorf("%s has invalid sha256 %q", entry.Path, entry.SHA256)
		}
		corpusBytes += entry.Bytes
	}
	if corpusBytes != baseline.Fuzz.DiskCorpus.Bytes {
		return maintenanceBaseline{}, fmt.Errorf("disk corpus bytes are %d, want %d", baseline.Fuzz.DiskCorpus.Bytes, corpusBytes)
	}
	if baseline.Performance.BaselineCommit != "b05b7ce145bb9a3c53301beb2619241180c786ce" {
		return maintenanceBaseline{}, fmt.Errorf("starting performance baseline changed")
	}
	return baseline, nil
}

func trackedFiles(root string) ([]string, error) {
	// Include untracked, non-ignored files so the check works before staging and
	// rejects newly added tests or corpus seeds that have no ownership record.
	// Exclude tracked paths deleted from the working tree: repository checks
	// must validate a deletion before it is staged.
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list tracked files: %w", err)
	}
	items := bytes.Split(out, []byte{0})
	files := make([]string, 0, len(items))
	for _, item := range items {
		if len(item) == 0 {
			continue
		}
		path := filepath.ToSlash(string(item))
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		files = append(files, path)
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

func validateCorpusManifest(root string, manifest corpusManifest, corpusFiles []string, targets []fuzzTarget, baselineEntries []baselineCorpusEntry) error {
	if manifest.Version != 2 {
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
	baselineSet := make(map[string]baselineCorpusEntry, len(baselineEntries))
	for _, entry := range baselineEntries {
		if _, exists := baselineSet[entry.Path]; exists {
			return fmt.Errorf("duplicate baseline origin %q", entry.Path)
		}
		baselineSet[entry.Path] = entry
	}

	seenCurrent := make(map[string]bool, len(manifest.Entries))
	seenOrigin := make(map[string]bool, len(manifest.Entries))
	previous := ""
	for _, entry := range manifest.Entries {
		if entry.Path <= previous && previous != "" {
			return fmt.Errorf("entries are not sorted by path at %q", entry.Path)
		}
		previous = entry.Path
		if seenCurrent[entry.Path] {
			return fmt.Errorf("duplicate path %q", entry.Path)
		}
		seenCurrent[entry.Path] = true
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

		originPackage, originTarget, found, err := corpusOwner(entry.OriginPath)
		if err != nil || !found {
			if err == nil {
				err = fmt.Errorf("not a fuzz corpus path")
			}
			return fmt.Errorf("%s origin %q: %w", entry.Path, entry.OriginPath, err)
		}
		if entry.OriginPackage != originPackage || entry.OriginTarget != originTarget {
			return fmt.Errorf("%s origin owner is %s::%s, want %s::%s from %s", entry.Path,
				entry.OriginPackage, entry.OriginTarget, originPackage, originTarget, entry.OriginPath)
		}
		baseline, exists := baselineSet[entry.OriginPath]
		if !exists {
			return fmt.Errorf("%s has fabricated baseline origin %q", entry.Path, entry.OriginPath)
		}
		if seenOrigin[entry.OriginPath] {
			return fmt.Errorf("baseline origin %q has multiple current descendants", entry.OriginPath)
		}
		seenOrigin[entry.OriginPath] = true
		switch entry.Status {
		case "retained":
			if entry.Path != entry.OriginPath || entry.OwnerPackage != entry.OriginPackage || entry.OwnerTarget != entry.OriginTarget ||
				entry.Bytes != baseline.Bytes || entry.SHA256 != baseline.SHA256 {
				return fmt.Errorf("%s retained seed differs from immutable origin %s", entry.Path, entry.OriginPath)
			}
		case "migrated":
			if entry.Path == entry.OriginPath || entry.OwnerPackage == entry.OriginPackage && entry.OwnerTarget == entry.OriginTarget {
				return fmt.Errorf("%s migrated seed did not move to a different target from %s", entry.Path, entry.OriginPath)
			}
		default:
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
	var missingCurrent []string
	for _, path := range corpusFiles {
		if !seenCurrent[path] {
			missingCurrent = append(missingCurrent, path)
		}
	}
	if len(missingCurrent) != 0 {
		return fmt.Errorf("seeds missing from manifest: %v", missingCurrent)
	}
	var missingOrigins []string
	for _, entry := range baselineEntries {
		if !seenOrigin[entry.Path] {
			missingOrigins = append(missingOrigins, entry.Path)
		}
	}
	if len(missingOrigins) != 0 {
		return fmt.Errorf("baseline seeds missing current descendants: %v", missingOrigins)
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
	fmt.Fprintln(&out, "| Origin | Baseline corpus file | Current owner | Current corpus file | Bytes | SHA-256 | Status |")
	fmt.Fprintln(&out, "| --- | --- | --- | --- | ---: | --- | --- |")
	for _, entry := range entries {
		fmt.Fprintf(&out, "| `%s::%s` | `%s` | `%s::%s` | `%s` | %d | `%s` | %s |\n",
			entry.OriginPackage, entry.OriginTarget, entry.OriginPath,
			entry.OwnerPackage, entry.OwnerTarget, entry.Path, entry.Bytes, entry.SHA256, entry.Status)
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
