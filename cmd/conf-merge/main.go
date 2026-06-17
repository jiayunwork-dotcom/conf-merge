package main

import (
	"conf-merge/pkg/color"
	"conf-merge/pkg/diff"
	"conf-merge/pkg/env"
	mergepkg "conf-merge/pkg/merge"
	"conf-merge/pkg/model"
	"conf-merge/pkg/parser"
	"conf-merge/pkg/schema"
	"conf-merge/pkg/writer"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

const (
	ExitOK          = 0
	ExitHasConflict = 1
	ExitParseError  = 2
)

type GlobalOptions struct {
	Format       parser.Format
	Color        bool
	Quiet        bool
	Verbose      bool
	ExpandEnv    bool
	ArrayKeys    map[string]string
	SortKeys     bool
	OutputFormat parser.Format
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(ExitParseError)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	globals, remaining := parseGlobalOptions(args)
	color.Enable = globals.Color

	var exitCode int
	var err error
	switch cmd {
	case "diff":
		exitCode, err = runDiff(remaining, globals)
	case "merge":
		exitCode, err = runMerge(remaining, globals)
	case "convert":
		exitCode, err = runConvert(remaining, globals)
	case "validate":
		exitCode, err = runValidate(remaining, globals)
	case "-h", "--help", "help":
		printUsage()
		exitCode = ExitOK
	case "-v", "--version":
		fmt.Println("conf-merge 1.0.0")
		exitCode = ExitOK
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		printUsage()
		exitCode = ExitParseError
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}
	os.Exit(exitCode)
}

func parseGlobalOptions(args []string) (*GlobalOptions, []string) {
	g := &GlobalOptions{
		Color:     true,
		ExpandEnv: true,
		ArrayKeys: make(map[string]string),
	}
	remaining := []string{}
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--no-color":
			g.Color = false
			i++
		case arg == "--quiet" || arg == "-q":
			g.Quiet = true
			i++
		case arg == "--verbose" || arg == "-v":
			g.Verbose = true
			i++
		case arg == "--no-expand-env":
			g.ExpandEnv = false
			i++
		case arg == "--sort-keys":
			g.SortKeys = true
			i++
		case strings.HasPrefix(arg, "--format="):
			name := strings.TrimPrefix(arg, "--format=")
			g.Format = parser.FormatFromName(name)
			i++
		case strings.HasPrefix(arg, "--output-format="):
			name := strings.TrimPrefix(arg, "--output-format=")
			g.OutputFormat = parser.FormatFromName(name)
			i++
		case strings.HasPrefix(arg, "--array-key="):
			val := strings.TrimPrefix(arg, "--array-key=")
			parts := strings.SplitN(val, "=", 2)
			if len(parts) == 2 {
				g.ArrayKeys[parts[0]] = parts[1]
			}
			i++
		default:
			remaining = append(remaining, arg)
			i++
		}
	}
	return g, remaining
}

func runDiff(args []string, g *GlobalOptions) (int, error) {
	if len(args) < 2 {
		return ExitParseError, fmt.Errorf("diff requires 2 files: file-a file-b")
	}
	fileA, fileB := args[0], args[1]

	isDirA := isDirectory(fileA)
	isDirB := isDirectory(fileB)
	if isDirA || isDirB {
		if !isDirA || !isDirB {
			return ExitParseError, fmt.Errorf("both paths must be files or both must be directories")
		}
		return runBatchDiff(fileA, fileB, g)
	}

	valA, fmtA, err := loadAndParse(fileA, g)
	if err != nil {
		return ExitParseError, err
	}
	valB, fmtB, err := loadAndParse(fileB, g)
	if err != nil {
		return ExitParseError, err
	}
	_ = fmtB

	if g.ExpandEnv {
		valA, _ = env.ExpandTree(valA)
		valB, _ = env.ExpandTree(valB)
	}
	files := map[string]*model.Value{
		fileA: valA,
		fileB: valB,
	}
	refs := env.DetectCrossReferences(files)
	if len(refs) > 0 && g.Verbose {
		printCrossReferences(refs)
	}
	opts := &diff.DiffOptions{ArrayKeyMap: g.ArrayKeys}
	if len(args) >= 3 {
		for _, extra := range args[2:] {
			if strings.HasPrefix(extra, "--array-key=") {
				val := strings.TrimPrefix(extra, "--array-key=")
				parts := strings.SplitN(val, "=", 2)
				if len(parts) == 2 {
					opts.ArrayKeyMap[parts[0]] = parts[1]
				}
			}
		}
	}
	result := diff.ComputeDiff(valA, valB, opts)
	if !g.Quiet {
		fmt.Printf("Diff between %s (%s) and %s (%s):\n", fileA, fmtA, fileB, fmtA)
		fmt.Println(result.Format(g.Verbose))
		fmt.Printf("Summary: %d total (%d added, %d removed, %d modified, %d type-changed)\n",
			result.Total, result.Added, result.Removed, result.Modified, result.TypeChg)
	}
	if result.HasDifferences() {
		return ExitHasConflict, nil
	}
	return ExitOK, nil
}

func runMerge(args []string, g *GlobalOptions) (int, error) {
	base, ours, theirs, output, strategy, err := parseMergeArgs(args)
	if err != nil {
		return ExitParseError, err
	}
	valBase, fmtBase, err := loadAndParse(base, g)
	if err != nil {
		return ExitParseError, err
	}
	valOurs, _, err := loadAndParse(ours, g)
	if err != nil {
		return ExitParseError, err
	}
	valTheirs, _, err := loadAndParse(theirs, g)
	if err != nil {
		return ExitParseError, err
	}
	if g.ExpandEnv {
		valBase, _ = env.ExpandTree(valBase)
		valOurs, _ = env.ExpandTree(valOurs)
		valTheirs, _ = env.ExpandTree(valTheirs)
	}
	result := mergepkg.ThreeWayMerge(valBase, valOurs, valTheirs, strategy)
	outFormat := fmtBase
	if g.OutputFormat != parser.FormatUnknown {
		outFormat = g.OutputFormat
	}
	wopts := &writer.WriterOptions{
		SortKeys:       g.SortKeys,
		IndentSize:     detectIndentSize(ours),
		OriginalFormat: fmtBase,
		HasConflicts:   result.HasConflict,
	}
	outputStr, err := writer.Write(result.Value, outFormat, wopts)
	if err != nil {
		return ExitParseError, err
	}
	if output != "" {
		err = ioutil.WriteFile(output, []byte(outputStr), 0644)
		if err != nil {
			return ExitParseError, err
		}
		if !g.Quiet {
			fmt.Printf("Merged output written to %s\n", output)
		}
	} else {
		fmt.Print(outputStr)
	}
	if result.HasConflict {
		fmt.Fprintf(os.Stderr, "\n%s", mergepkg.FormatConflictList(result.Conflicts))
		return ExitHasConflict, nil
	}
	return ExitOK, nil
}

func parseMergeArgs(args []string) (base, ours, theirs, output string, strategy mergepkg.ConflictStrategy, err error) {
	strategy = mergepkg.StrategyManual
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "-o" || arg == "--output":
			if i+1 >= len(args) {
				err = fmt.Errorf("-o requires a value")
				return
			}
			output = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--output="):
			output = strings.TrimPrefix(arg, "--output=")
			i++
		case arg == "--strategy" || arg == "-s":
			if i+1 >= len(args) {
				err = fmt.Errorf("--strategy requires a value")
				return
			}
			strategy = mergepkg.StrategyFromString(args[i+1])
			i += 2
		case strings.HasPrefix(arg, "--strategy="):
			strategy = mergepkg.StrategyFromString(strings.TrimPrefix(arg, "--strategy="))
			i++
		default:
			switch {
			case base == "":
				base = arg
			case ours == "":
				ours = arg
			case theirs == "":
				theirs = arg
			}
			i++
		}
	}
	if base == "" || ours == "" || theirs == "" {
		err = fmt.Errorf("merge requires 3 files: base ours theirs [-o output]")
	}
	return
}

func runConvert(args []string, g *GlobalOptions) (int, error) {
	if len(args) < 2 {
		return ExitParseError, fmt.Errorf("convert requires: input -f format [-o output]")
	}
	input := ""
	formatName := ""
	output := ""
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "-f" || arg == "--format":
			if i+1 >= len(args) {
				return ExitParseError, fmt.Errorf("-f requires a value")
			}
			formatName = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--format="):
			formatName = strings.TrimPrefix(arg, "--format=")
			i++
		case arg == "-o" || arg == "--output":
			if i+1 >= len(args) {
				return ExitParseError, fmt.Errorf("-o requires a value")
			}
			output = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--output="):
			output = strings.TrimPrefix(arg, "--output=")
			i++
		default:
			input = arg
			i++
		}
	}
	if input == "" || formatName == "" {
		return ExitParseError, fmt.Errorf("convert requires: input -f format [-o output]")
	}
	targetFormat := parser.FormatFromName(formatName)
	if targetFormat == parser.FormatUnknown {
		return ExitParseError, fmt.Errorf("unknown output format: %s", formatName)
	}
	val, srcFormat, err := loadAndParse(input, g)
	if err != nil {
		return ExitParseError, err
	}
	_ = srcFormat
	if g.ExpandEnv {
		val, _ = env.ExpandTree(val)
	}
	wopts := &writer.WriterOptions{
		SortKeys:   g.SortKeys,
		IndentSize: detectIndentSize(input),
	}
	outputStr, err := writer.Write(val, targetFormat, wopts)
	if err != nil {
		return ExitParseError, err
	}
	if output != "" {
		err = ioutil.WriteFile(output, []byte(outputStr), 0644)
		if err != nil {
			return ExitParseError, err
		}
		if !g.Quiet {
			fmt.Printf("Converted output written to %s\n", output)
		}
	} else {
		fmt.Print(outputStr)
	}
	return ExitOK, nil
}

func runValidate(args []string, g *GlobalOptions) (int, error) {
	if len(args) < 1 {
		return ExitParseError, fmt.Errorf("validate requires: file --schema=schema.json")
	}
	file := ""
	schemaFile := ""
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case strings.HasPrefix(arg, "--schema="):
			schemaFile = strings.TrimPrefix(arg, "--schema=")
			i++
		default:
			file = arg
			i++
		}
	}
	if file == "" || schemaFile == "" {
		return ExitParseError, fmt.Errorf("validate requires: file --schema=schema.json")
	}
	val, _, err := loadAndParse(file, g)
	if err != nil {
		return ExitParseError, err
	}
	schemaContent, err := ioutil.ReadFile(schemaFile)
	if err != nil {
		return ExitParseError, fmt.Errorf("cannot read schema: %v", err)
	}
	schemaVal, _, err := parser.ParseFile(string(schemaContent), schemaFile, parser.FormatJSON)
	if err != nil {
		return ExitParseError, fmt.Errorf("cannot parse schema: %v", err)
	}
	s, err := schema.NewSchema(schemaVal)
	if err != nil {
		return ExitParseError, fmt.Errorf("invalid schema: %v", err)
	}
	errs := s.Validate(val)
	if len(errs) > 0 {
		if !g.Quiet {
			fmt.Printf("Validation failed with %d error(s):\n", len(errs))
			for i, e := range errs {
				fmt.Printf("  %d. %s: %s\n", i+1, e.Path, e.Message)
			}
		}
		return ExitHasConflict, nil
	}
	if !g.Quiet {
		fmt.Println("Validation passed!")
	}
	return ExitOK, nil
}

func printCrossReferences(refs map[string][]env.Reference) {
	fmt.Println("\nCross-file references detected:")
	for src, list := range refs {
		for _, r := range list {
			fmt.Printf("  %s:%s references ${%s} defined in %s\n",
				src, r.Path, r.Variable, r.TargetFile)
		}
	}
	fmt.Println()
}

func loadAndParse(path string, g *GlobalOptions) (*model.Value, parser.Format, error) {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, parser.FormatUnknown, fmt.Errorf("cannot read %s: %v", path, err)
	}
	v, f, err := parser.ParseFile(string(content), path, g.Format)
	if err != nil {
		if pe, ok := err.(*parser.ParseError); ok {
			return nil, f, fmt.Errorf("%s", pe.Error())
		}
		return nil, f, err
	}
	return v, f, nil
}

func isDirectory(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.IsDir()
}

func detectIndentSize(path string) int {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return 2
	}
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		indent := 0
		for indent < len(line) && line[indent] == ' ' {
			indent++
		}
		if indent > 0 && indent < len(line) {
			if indent%4 == 0 {
				return 4
			}
			return 2
		}
	}
	return 2
}

func runBatchDiff(dirA, dirB string, g *GlobalOptions) (int, error) {
	include := []string{"*.yaml", "*.yml", "*.json", "*.toml", "*.ini", "*.properties", ".env*", "*.env"}
	exclude := []string{}
	batchOpts := &BatchOptions{
		Include: include,
		Exclude: exclude,
		Workers: 4,
	}
	report, err := runBatchDiffInternal(dirA, dirB, g, batchOpts)
	if err != nil {
		return ExitParseError, err
	}
	if !g.Quiet {
		printBatchReport(report)
	}
	if report.FilesWithDiff > 0 || report.FilesOnlyInA > 0 || report.FilesOnlyInB > 0 {
		return ExitHasConflict, nil
	}
	return ExitOK, nil
}

func printBatchReport(r *BatchReport) {
	fmt.Println("\n======== Batch Diff Report ========")
	fmt.Printf("Total files processed: %d\n", r.TotalFiles)
	fmt.Printf("  Files with differences: %d\n", r.FilesWithDiff)
	fmt.Printf("  Files identical: %d\n", r.TotalFiles-r.FilesWithDiff-r.FilesOnlyInA-r.FilesOnlyInB)
	fmt.Printf("  Only in A: %d\n", r.FilesOnlyInA)
	fmt.Printf("  Only in B: %d\n", r.FilesOnlyInB)
	fmt.Printf("Total conflicts: %d\n", r.TotalConflicts)
	if len(r.OnlyInA) > 0 {
		fmt.Println("\nFiles only in A:")
		for _, f := range r.OnlyInA {
			fmt.Printf("  - %s\n", f)
		}
	}
	if len(r.OnlyInB) > 0 {
		fmt.Println("\nFiles only in B:")
		for _, f := range r.OnlyInB {
			fmt.Printf("  + %s\n", f)
		}
	}
	if len(r.FileResults) > 0 {
		fmt.Println("\nFiles with differences:")
		for _, fr := range r.FileResults {
			if fr.HasDiff {
				fmt.Printf("  ~ %s (%d added, %d removed, %d modified)\n",
					fr.RelPath, fr.Added, fr.Removed, fr.Modified)
			}
		}
	}
	fmt.Println("===================================")
}

func printUsage() {
	fmt.Println(`conf-merge - Multi-format config file diff & merge tool

USAGE:
  conf-merge <command> [options] <arguments>

COMMANDS:
  diff     <file-a> <file-b> [options]
           Two-way semantic diff of two files (or directories).

  merge    <base> <ours> <theirs> [-o output] [--strategy=ours|theirs|interactive|manual]
           Three-way merge with conflict resolution.

  convert  <input> -f <format> [-o output]
           Convert between formats (json/yaml/toml/ini/properties/env).

  validate <file> --schema=<schema.json>
           Validate config file against JSON Schema.

GLOBAL OPTIONS:
  --format=<name>           Force input format (json/yaml/toml/ini/properties/env)
  --output-format=<name>    Force output format
  --sort-keys               Sort keys alphabetically in output
  --array-key=<path>=<key>  Array matching key for semantic diff (e.g. users=name)
  --no-color                Disable colored output
  --no-expand-env           Disable $VAR and ${VAR} expansion
  -q, --quiet               Suppress all non-error output
  -v, --verbose             Enable detailed output

EXIT CODES:
  0  Success, no conflicts
  1  Success, but has conflicts/differences
  2  Parse error or usage error`)
}

type FileDiffResult struct {
	RelPath   string
	HasDiff   bool
	Added     int
	Removed   int
	Modified  int
	TypeChg   int
	Error     string
}

type BatchReport struct {
	TotalFiles      int
	FilesWithDiff   int
	FilesOnlyInA    int
	FilesOnlyInB    int
	TotalConflicts  int
	OnlyInA         []string
	OnlyInB         []string
	FileResults     []FileDiffResult
}

type BatchOptions struct {
	Include []string
	Exclude []string
	Workers int
}

func runBatchDiffInternal(dirA, dirB string, g *GlobalOptions, opts *BatchOptions) (*BatchReport, error) {
	report := &BatchReport{}

	filesA, err := collectFiles(dirA, opts.Include, opts.Exclude)
	if err != nil {
		return nil, err
	}
	filesB, err := collectFiles(dirB, opts.Include, opts.Exclude)
	if err != nil {
		return nil, err
	}

	mapA := make(map[string]string)
	mapB := make(map[string]string)

	for _, f := range filesA {
		rel, _ := filepath.Rel(dirA, f)
		mapA[filepath.ToSlash(rel)] = f
	}
	for _, f := range filesB {
		rel, _ := filepath.Rel(dirB, f)
		mapB[filepath.ToSlash(rel)] = f
	}

	allKeys := make(map[string]bool)
	for k := range mapA {
		allKeys[k] = true
	}
	for k := range mapB {
		allKeys[k] = true
	}

	commonKeys := []string{}
	for k := range allKeys {
		_, inA := mapA[k]
		_, inB := mapB[k]
		if inA && inB {
			commonKeys = append(commonKeys, k)
		} else if inA {
			report.OnlyInA = append(report.OnlyInA, k)
			report.FilesOnlyInA++
		} else {
			report.OnlyInB = append(report.OnlyInB, k)
			report.FilesOnlyInB++
		}
	}

	report.TotalFiles = len(allKeys)

	type job struct {
		rel  string
		fileA string
		fileB string
	}
	jobs := make(chan job)
	results := make(chan FileDiffResult)

	workers := opts.Workers
	if workers <= 0 {
		workers = 1
	}
	for w := 0; w < workers; w++ {
		go func() {
			for j := range jobs {
				res := FileDiffResult{RelPath: j.rel}
				valA, _, err := loadAndParse(j.fileA, g)
				if err != nil {
					res.Error = err.Error()
					results <- res
					continue
				}
				valB, _, err := loadAndParse(j.fileB, g)
				if err != nil {
					res.Error = err.Error()
					results <- res
					continue
				}
				if g.ExpandEnv {
					valA, _ = env.ExpandTree(valA)
					valB, _ = env.ExpandTree(valB)
				}
				dopts := &diff.DiffOptions{ArrayKeyMap: g.ArrayKeys}
				dres := diff.ComputeDiff(valA, valB, dopts)
				res.HasDiff = dres.HasDifferences()
				res.Added = dres.Added
				res.Removed = dres.Removed
				res.Modified = dres.Modified
				res.TypeChg = dres.TypeChg
				results <- res
			}
		}()
	}

	go func() {
		for _, k := range commonKeys {
			jobs <- job{rel: k, fileA: mapA[k], fileB: mapB[k]}
		}
		close(jobs)
	}()

	for i := 0; i < len(commonKeys); i++ {
		res := <-results
		report.FileResults = append(report.FileResults, res)
		if res.HasDiff {
			report.FilesWithDiff++
		}
	}

	return report, nil
}

func collectFiles(root string, include, exclude []string) ([]string, error) {
	var result []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		name := filepath.Base(rel)
		matched := false
		for _, pat := range include {
			if ok, _ := filepath.Match(pat, name); ok {
				matched = true
				break
			}
			if strings.HasPrefix(pat, ".env") && strings.HasPrefix(name, ".env") {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
		for _, pat := range exclude {
			if ok, _ := filepath.Match(pat, name); ok {
				return nil
			}
		}
		result = append(result, path)
		return nil
	})
	return result, err
}
