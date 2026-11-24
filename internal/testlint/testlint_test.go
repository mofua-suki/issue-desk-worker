// Package testlint flags marks of tests that tend not to protect behavior.
// A mark is a hint, not a verdict: allow.txt lists the justified exceptions.
package testlint

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	markProduct = "a:製品ファイルの文字列照合"
	markSyntax  = "b:"
	markEnvSkip = "c:環境変数がないと省略されるテスト"
	markLegacy  = "d:旧実装との比較"
	markSkipped = "e:常に省略されるテスト"
	wholeFile   = "(ファイル単位: テスト名を特定不可)"
	guidance    = "挙動を検証するテストに直すか、理由を付けて許可一覧に足す"
)

type finding struct{ file, test, mark string }

func (f finding) key() string    { return f.file + "|" + f.test + "|" + f.mark }
func (f finding) String() string { return f.key() + ": " + guidance }

// scan parses each Go test file under root once and returns the marks and the file count.
func scan(root string) ([]finding, int, error) {
	var out []finding
	files := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "testdata" || name == "fixtures" || name == "vendor" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		out = append(out, scanFile(filepath.ToSlash(rel), file)...)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out, files, err
}

func scanFile(rel string, file *ast.File) []finding {
	var out []finding
	seen := map[string]bool{}
	add := func(name, mark string) {
		f := finding{rel, name, mark}
		if !seen[f.key()] {
			seen[f.key()] = true
			out = append(out, f)
		}
	}
	for _, imp := range file.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p == "go/parser" || p == "go/ast" || p == "go/format" {
			add(wholeFile, markSyntax+p)
		}
	}
	// Helpers are scanned too: a check moved out of a Test function is the same check.
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		values := assignedValues(fn.Body)
		// In a test, returning early is the same as skipping, except in a helper process
		// that the test re-executes and that calls os.Exit.
		returns := isTest(fn) && !callsExit(fn.Body)
		var product, compare, legacy bool
		envSkip := envSkips(fn.Body, values, returns)
		if skipsAtTop(fn.Body) {
			add(fn.Name.Name, markSkipped)
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				// A subtest whose body skips unconditionally never runs either.
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" && len(x.Args) > 0 {
					if lit, ok := x.Args[len(x.Args)-1].(*ast.FuncLit); ok && skipsAtTop(lit.Body) {
						add(fn.Name.Name, markSkipped)
					}
				}
				switch fun := x.Fun.(type) {
				case *ast.Ident:
					legacy = legacy || strings.HasPrefix(strings.ToLower(fun.Name), "legacy")
				case *ast.SelectorExpr:
					legacy = legacy || strings.Contains(strings.ToLower(fun.Sel.Name), "legacy")
					switch fun.Sel.Name {
					case "ReadFile", "Open", "OpenFile", "ParseFile":
						for _, arg := range x.Args {
							product = product || productPath(arg, values)
						}
					}
				}
			case *ast.SelectorExpr:
				switch x.Sel.Name {
				case "Contains", "ContainsAny", "Count", "Equal", "DeepEqual", "Match", "MatchString", "MustCompile", "Compile", "Index", "HasPrefix", "HasSuffix":
					compare = true
				}
			case *ast.BinaryExpr:
				// An error check alone is not a comparison of the content.
				if (x.Op == token.EQL || x.Op == token.NEQ) && !isNil(x.X) && !isNil(x.Y) {
					compare = true
				}
			}
			return true
		})
		name := fn.Name.Name
		if product && compare {
			add(name, markProduct)
		}
		if envSkip {
			add(name, markEnvSkip)
		}
		if legacy && compare {
			add(name, markLegacy)
		}
	}
	return out
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

func skipCall(n ast.Node) bool {
	stmt, ok := n.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && (sel.Sel.Name == "Skip" || sel.Sel.Name == "Skipf" || sel.Sel.Name == "SkipNow")
}

func skipsAtTop(body *ast.BlockStmt) bool {
	for _, stmt := range body.List {
		if skipCall(stmt) {
			return true
		}
	}
	return false
}

// envSkips reports whether a body skips, or returns when returns is set, in a branch
// taken on an environment variable. A t.Run body is a test body; another function literal
// (a helper closure) may still skip, but its return does not leave the test.
func envSkips(body *ast.BlockStmt, values map[string][]ast.Expr, returns bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" && len(x.Args) > 0 {
				if lit, ok := x.Args[len(x.Args)-1].(*ast.FuncLit); ok {
					for _, arg := range x.Args[:len(x.Args)-1] {
						found = found || envSkips(&ast.BlockStmt{List: []ast.Stmt{&ast.ExprStmt{X: arg}}}, values, false)
					}
					found = found || envSkips(lit.Body, values, returns)
					return false
				}
			}
		case *ast.FuncLit:
			found = found || envSkips(x.Body, values, false)
			return false
		case *ast.IfStmt:
			// A test that skips unless a variable is set never runs in the usual verification.
			found = found || (envRead(x.Init, values, true) || envRead(x.Cond, values, true)) && (leaves(x.Body, returns) || leaves(x.Else, returns))
		case *ast.SwitchStmt:
			if envRead(x.Init, values, true) || envRead(x.Tag, values, true) {
				for _, clause := range x.Body.List {
					found = found || leaves(&ast.BlockStmt{List: clause.(*ast.CaseClause).Body}, returns)
				}
			}
		}
		return !found
	})
	return found
}

// leaves reports whether a branch (a block, or an else-if chain) calls Skip outside nested
// function literals, or, when returns is set, only returns (after logging at most). A
// branch that does work before returning, such as a crash helper run by the test itself,
// is not an early exit.
func leaves(n ast.Node, returns bool) bool {
	if n == nil {
		return false
	}
	if chain, ok := n.(*ast.IfStmt); ok {
		return leaves(chain.Body, returns) || leaves(chain.Else, returns)
	}
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		found = found || skipCall(n)
		return !found
	})
	if found || !returns {
		return found
	}
	block, ok := n.(*ast.BlockStmt)
	if !ok || len(block.List) == 0 {
		return false
	}
	for _, stmt := range block.List[:len(block.List)-1] {
		if !logCall(stmt) {
			return false
		}
	}
	_, last := block.List[len(block.List)-1].(*ast.ReturnStmt)
	return last
}

func logCall(stmt ast.Stmt) bool {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && (sel.Sel.Name == "Log" || sel.Sel.Name == "Logf")
}

func isTest(fn *ast.FuncDecl) bool {
	if !strings.HasPrefix(fn.Name.Name, "Test") || fn.Recv != nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "T"
}

func callsExit(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Exit" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// envRead reports whether a node reads an environment variable directly or through a
// local name assigned straight from such a read. Names are followed one step only:
// values are collected per function, so deeper chains would mix unrelated assignments.
func envRead(n ast.Node, values map[string][]ast.Expr, follow bool) bool {
	if n == nil {
		return false
	}
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			found = found || x.Sel.Name == "Getenv" || x.Sel.Name == "LookupEnv"
		case *ast.Ident:
			if follow {
				for _, value := range values[x.Name] {
					found = found || envRead(value, values, false)
				}
			}
		}
		return !found
	})
	return found
}

// assignedValues maps each local name to the expressions assigned to it, including
// the elements of a literal list it ranges over.
func assignedValues(body *ast.BlockStmt) map[string][]ast.Expr {
	values := map[string][]ast.Expr{}
	set := func(lhs ast.Expr, rhs ast.Expr) {
		if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
			values[id.Name] = append(values[id.Name], rhs)
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i := range x.Lhs {
					set(x.Lhs[i], x.Rhs[i])
				}
			} else if len(x.Rhs) == 1 {
				set(x.Lhs[0], x.Rhs[0])
			}
		case *ast.ValueSpec:
			for i, name := range x.Names {
				if i < len(x.Values) {
					set(name, x.Values[i])
				}
			}
		case *ast.RangeStmt:
			if list, ok := x.X.(*ast.CompositeLit); ok && x.Value != nil {
				for _, element := range list.Elts {
					set(x.Value, element)
				}
			}
		}
		return true
	})
	return values
}

// productPath reports whether a read argument names a product file by a path fixed in
// the test source. A path built on a runtime value (a temporary tree, a fixture's
// directory) is a file the test created, so it is not a product file.
func productPath(arg ast.Expr, values map[string][]ast.Expr) bool {
	parts, fixed := pathParts(arg, values, 0)
	if !fixed {
		return false
	}
	product := false
	for _, part := range parts {
		lower := strings.ToLower(part)
		if strings.Contains(lower, "testdata") || strings.Contains(lower, "fixtures") {
			return false
		}
		for _, suffix := range []string{".go", ".md", ".sh"} {
			product = product || strings.HasSuffix(lower, suffix)
		}
		product = product || strings.Contains(lower, "openapi") || strings.Contains(lower, "readme")
	}
	return product
}

func pathParts(e ast.Expr, values map[string][]ast.Expr, depth int) ([]string, bool) {
	if depth > 4 {
		return nil, false
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return nil, false
		}
		s, _ := strconv.Unquote(x.Value)
		return []string{s}, true
	case *ast.ParenExpr:
		return pathParts(x.X, values, depth+1)
	case *ast.BinaryExpr:
		parts, fixed := pathParts(x.X, values, depth+1)
		if !fixed || x.Op != token.ADD {
			return nil, false
		}
		rest, _ := pathParts(x.Y, values, depth+1)
		return append(parts, rest...), true
	case *ast.Ident:
		var parts []string
		fixed := false
		for _, value := range values[x.Name] {
			if p, ok := pathParts(value, values, depth+1); ok {
				parts, fixed = append(parts, p...), true
			}
		}
		return parts, fixed
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok || len(x.Args) == 0 {
			return nil, false
		}
		switch sel.Sel.Name {
		case "Join":
			parts, fixed := pathParts(x.Args[0], values, depth+1)
			if !fixed {
				return nil, false
			}
			for _, arg := range x.Args[1:] {
				rest, _ := pathParts(arg, values, depth+1)
				parts = append(parts, rest...)
			}
			return parts, true
		case "Abs", "Clean", "FromSlash":
			return pathParts(x.Args[0], values, depth+1)
		}
	}
	return nil, false
}

// check compares the marks with the allow list: `file|test|mark<TAB>reason` per line.
func check(findings []finding, allow string) []string {
	entries := map[string]bool{}
	var problems []string
	s := bufio.NewScanner(strings.NewReader(allow))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
			problems = append(problems, "理由のない許可項目: "+line)
			continue
		}
		if entries[parts[0]] {
			problems = append(problems, "重複した許可項目: "+parts[0])
		}
		entries[parts[0]] = true
	}
	if err := s.Err(); err != nil {
		problems = append(problems, err.Error())
	}
	seen := map[string]bool{}
	for _, f := range findings {
		seen[f.key()] = true
		if !entries[f.key()] {
			problems = append(problems, f.String())
		}
	}
	for k := range entries {
		if !seen[k] {
			problems = append(problems, "検出されない許可項目（削除する）: "+k)
		}
	}
	sort.Strings(problems)
	return problems
}

// unrunScripts lists test scripts outside Go that neither a Go test runs nor a
// Dockerfile RUN instruction executes. Comments, COPY lines and the samples of this
// package do not count as running a script.
func unrunScripts(root string) ([]string, error) {
	var scripts, runs []string
	var dockerSources []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == "testdata" || name == "fixtures" || name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		switch {
		case isTestScript(name):
			scripts = append(scripts, filepath.ToSlash(rel))
		case strings.HasSuffix(name, "_test.go") && filepath.ToSlash(filepath.Dir(rel)) != "internal/testlint":
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			runs = append(runs, commandStrings(file)...)
		case strings.HasPrefix(name, "Dockerfile"):
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sources, directRuns := dockerRunSources(string(raw))
			runs = append(runs, directRuns...)
			dockerSources = append(dockerSources, sources...)
		}
		return nil
	})
	var out []string
	for _, script := range scripts {
		named := false
		unique := true
		for _, other := range scripts {
			if other != script && filepath.Base(other) == filepath.Base(script) {
				unique = false
				break
			}
		}
		for _, run := range runs {
			named = named || containsScriptPath(run, script) || unique && containsScriptPath(run, filepath.Base(script))
		}
		for _, source := range dockerSources {
			named = named || source == script
		}
		if !named {
			out = append(out, "どの検証からも実行されないテスト: "+script+": go testかイメージ構築から実行する")
		}
	}
	sort.Strings(out)
	return out, err
}

// containsScriptPath matches whole path components, not a filename embedded in another path.
func containsScriptPath(text, path string) bool {
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-", r))
	}) {
		field = strings.TrimLeft(field, "/")
		for strings.HasPrefix(field, "../") || strings.HasPrefix(field, "./") {
			if strings.HasPrefix(field, "../") {
				field = strings.TrimPrefix(field, "../")
			} else {
				field = strings.TrimPrefix(field, "./")
			}
		}
		if field == strings.TrimLeft(path, "/") || !strings.Contains(path, "/") && strings.HasSuffix(field, "/"+path) {
			return true
		}
	}
	return false
}

// dockerRunSources maps explicitly copied files back to their source when RUN names
// the copy. This also disambiguates files with the same basename in different directories.
func dockerRunSources(dockerfile string) ([]string, []string) {
	var out, direct []string
	copies := make(map[string]string)
	for _, instruction := range dockerInstructions(dockerfile) {
		fields := strings.Fields(instruction)
		if len(fields) == 0 {
			continue
		}
		switch {
		case strings.EqualFold(fields[0], "FROM"):
			clear(copies)
		case strings.EqualFold(fields[0], "COPY") && len(fields) >= 3:
			dest := fields[len(fields)-1]
			for _, source := range fields[1 : len(fields)-1] {
				if strings.HasPrefix(source, "--") {
					continue
				}
				target := dest
				if strings.HasSuffix(dest, "/") {
					target += filepath.Base(source)
				}
				copies[target] = strings.TrimPrefix(source, "./")
			}
		case strings.EqualFold(fields[0], "RUN"):
			for _, run := range executableCommands(strings.TrimSpace(instruction[len(fields[0]):])) {
				for target, source := range copies {
					if containsScriptPath(run, target) {
						out = append(out, source)
						run = strings.ReplaceAll(run, target, " ")
					}
				}
				direct = append(direct, run)
			}
		}
	}
	return out, direct
}

// executableCommands returns the script operands of a Docker RUN instruction.
func executableCommands(run string) []string {
	if strings.HasPrefix(run, "[") {
		var fields []string
		if json.Unmarshal([]byte(run), &fields) != nil || len(fields) == 0 {
			return nil
		}
		return scriptOperands(fields[0], literalArguments(fields[1:]))
	}
	var out []string
	for _, part := range strings.FieldsFunc(run, func(r rune) bool { return r == '&' || r == '|' || r == ';' || r == '\n' }) {
		fields := strings.Fields(part)
		for len(fields) > 0 && strings.Contains(fields[0], "=") {
			fields = fields[1:]
		}
		if len(fields) > 0 {
			out = append(out, scriptOperands(fields[0], literalArguments(fields[1:]))...)
		}
	}
	return out
}

func literalArguments(values []string) [][]string {
	args := make([][]string, len(values))
	for i, value := range values {
		args[i] = []string{value}
	}
	return args
}

func isTestScript(name string) bool {
	return strings.HasSuffix(name, ".test.sh") || strings.HasSuffix(name, ".test.mjs") || strings.HasSuffix(name, ".test.mts")
}

// scriptOperands identifies the program being run. Only the node test runner
// accepts multiple script paths; arguments of a shell or Node program are data.
func scriptOperands(executable string, args [][]string) []string {
	exe := filepath.Base(executable)
	if isTestScript(exe) {
		return []string{executable}
	}
	if exe != "sh" && exe != "bash" && exe != "node" {
		return nil
	}
	nodeTest := false
	positionalOnly := false
	option := ""
	var files []string
	for _, choices := range args {
		if len(choices) == 0 {
			return nil
		}
		if option != "" {
			if len(choices) != 1 {
				return nil
			}
			if option == "-o" {
				switch choices[0] {
				case "errexit", "nounset", "pipefail", "xtrace":
				default:
					return nil
				}
			} else if !validNodeTestOption(option + "=" + choices[0]) {
				return nil
			}
			option = ""
			continue
		}
		if positionalOnly || len(choices) != 1 || !strings.HasPrefix(choices[0], "-") {
			if !nodeTest {
				return choices
			}
			files = append(files, choices...)
			continue
		}
		flag := choices[0]
		switch {
		case flag == "--":
			positionalOnly = true
		case exe == "node" && flag == "--test" && len(files) == 0:
			nodeTest = true
		case exe == "node" && nodeTest && (flag == "--test-concurrency" || flag == "--test-timeout" || flag == "--test-reporter"):
			option = flag
		case exe == "node" && nodeTest && validNodeTestOption(flag):
		case exe != "node" && flag == "-o":
			option = flag
		case exe != "node" && len(flag) > 1 && strings.Trim(flag[1:], "EeuVvx") == "":
		default:
			// Shell -c/-n/-s and Node --check/--eval do not run an operand.
			return nil
		}
	}
	if option != "" {
		return nil
	}
	return files
}

func validNodeTestOption(flag string) bool {
	switch flag {
	case "--test-reporter=spec", "--test-reporter=tap", "--test-reporter=dot":
		return true
	}
	name, value, ok := strings.Cut(flag, "=")
	if !ok || name != "--test-concurrency" && name != "--test-timeout" {
		return false
	}
	amount, err := strconv.Atoi(value)
	return err == nil && amount > 0
}

func argumentVectors(expr ast.Expr, values map[string][]ast.Expr, depth int) [][]ast.Expr {
	if depth > 4 {
		return nil
	}
	switch arg := expr.(type) {
	case *ast.CompositeLit:
		return [][]ast.Expr{arg.Elts}
	case *ast.Ident:
		var vectors [][]ast.Expr
		for _, value := range values[arg.Name] {
			vectors = append(vectors, argumentVectors(value, values, depth+1)...)
		}
		return vectors
	}
	return nil
}

// commandStrings collects arguments of started commands and process.Run calls.
func commandStrings(file *ast.File) []string {
	var out []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		values := assignedValues(fn.Body)
		var alternatives func(ast.Expr, int) []string
		alternatives = func(expr ast.Expr, depth int) []string {
			if depth > 4 {
				return nil
			}
			switch x := expr.(type) {
			case *ast.Ident:
				var result []string
				for _, value := range values[x.Name] {
					result = append(result, alternatives(value, depth+1)...)
				}
				return result
			case *ast.KeyValueExpr:
				return alternatives(x.Value, depth+1)
			case *ast.CallExpr:
				parts, fixed := pathParts(x, values, 0)
				if fixed {
					return []string{filepath.Join(parts...)}
				}
				return nil
			default:
				parts, fixed := pathParts(expr, values, 0)
				if fixed {
					return []string{strings.Join(parts, "")}
				}
				return nil
			}
		}
		var command func(ast.Expr)
		seen := map[string]bool{}
		command = func(expr ast.Expr) {
			if id, ok := expr.(*ast.Ident); ok {
				if seen[id.Name] {
					return
				}
				seen[id.Name] = true
				for _, value := range values[id.Name] {
					command(value)
				}
				return
			}
			call, ok := expr.(*ast.CallExpr)
			if !ok {
				return
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return
			}
			offset := 0
			if pkg.Name == "exec" && sel.Sel.Name == "CommandContext" || pkg.Name == "process" && sel.Sel.Name == "Run" {
				offset = 1
			} else if pkg.Name != "exec" || sel.Sel.Name != "Command" {
				return
			}
			if len(call.Args) <= offset {
				return
			}
			executables := alternatives(call.Args[offset], 0)
			args := call.Args[offset+1:]
			vectors := [][]ast.Expr{args}
			if pkg.Name == "process" || call.Ellipsis.IsValid() {
				if len(args) == 0 {
					return
				}
				if pkg.Name == "process" {
					vectors = argumentVectors(args[0], values, 0)
				} else {
					vectors = argumentVectors(args[len(args)-1], values, 0)
					for i, vector := range vectors {
						prefix := append([]ast.Expr(nil), args[:len(args)-1]...)
						vectors[i] = append(prefix, vector...)
					}
				}
			}
			for _, args := range vectors {
				choices := make([][]string, len(args))
				for i, arg := range args {
					choices[i] = alternatives(arg, 0)
				}
				for _, exe := range executables {
					out = append(out, scriptOperands(exe, choices)...)
				}
			}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "process" && sel.Sel.Name == "Run" {
				command(call)
			} else if sel.Sel.Name == "Run" || sel.Sel.Name == "CombinedOutput" || sel.Sel.Name == "Output" || sel.Sel.Name == "Start" {
				command(sel.X)
			}
			return true
		})
	}
	return out
}

// dockerRuns returns the RUN instructions of a Dockerfile, joining continued lines and
// dropping comment lines.
func dockerInstructions(dockerfile string) []string {
	var out []string
	var current strings.Builder
	for _, line := range strings.Split(dockerfile, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		continued := strings.HasSuffix(trimmed, "\\")
		current.WriteString(strings.TrimSuffix(trimmed, "\\") + " ")
		if !continued {
			out = append(out, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

func TestRepository(t *testing.T) {
	start := time.Now()
	findings, files, err := scan("../..")
	if err != nil {
		t.Fatal(err)
	}
	allow, err := os.ReadFile("allow.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range check(findings, string(allow)) {
		t.Error(p)
	}
	unrun, err := unrunScripts("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range unrun {
		t.Error(p)
	}
	t.Logf("テストファイル %d 本、目印 %d 件、%s", files, len(findings), time.Since(start).Round(time.Millisecond))
}

const sampleTests = `package sample

import (
	"go/parser"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
	b, _ := os.ReadFile("../../README.md")
	if strings.Contains(string(b), "x") {
	}
}

func readShipped(t *testing.T) {
	for _, name := range []string{"run.sh", "compose.yaml"} {
		b, _ := os.ReadFile(filepath.Join("..", "deploy", name))
		if string(b) != "x" {
		}
	}
}

func TestFixture(t *testing.T) {
	b, _ := os.ReadFile("testdata/a.go")
	if strings.Contains(string(b), "x") {
	}
}

func TestCreatedFile(t *testing.T) {
	tree := t.TempDir()
	b, _ := os.ReadFile(filepath.Join(tree, "README.md"))
	if strings.Contains(string(b), "x") {
	}
}

func TestErrorOnly(t *testing.T) {
	if _, err := os.ReadFile("main.go"); err != nil {
	}
}

func TestMeasure(t *testing.T) {
	if os.Getenv("RUN") == "" {
		t.Skip("off")
	}
	measure()
}

func TestRealEnvironment(t *testing.T) {
	enabled := os.Getenv("RUN") == "1"
	if !enabled {
		t.Skip("off")
	}
	run()
}

func TestLookupElse(t *testing.T) {
	t.Run("case", func(t *testing.T) {
		if _, ok := os.LookupEnv("RUN"); ok {
			run()
		} else {
			t.Skipf("off")
		}
	})
}

func TestMissingTool(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	env := []string{"PATH=" + os.Getenv("PATH")}
	run(env)
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("HELPER") == "" {
		return
	}
	os.Exit(0)
}

func TestEnvSetsUpOnly(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Setenv("HOME", t.TempDir())
	}
	run()
}

func TestReturnUnlessSet(t *testing.T) {
	if os.Getenv("RUN") == "" {
		return
	}
	run()
}

func TestElseIfChain(t *testing.T) {
	if os.Getenv("MODE") == "a" {
		run()
	} else if ready() {
		run()
	} else {
		t.Skip("off")
	}
}

func TestSwitchEnv(t *testing.T) {
	switch os.Getenv("MODE") {
	case "full":
		run()
	default:
		t.SkipNow()
	}
}

func TestSwitchOther(t *testing.T) {
	switch mode() {
	case "a":
		return
	}
	run()
}

func helperReturns(t *testing.T) {
	if os.Getenv("CI") == "" {
		return
	}
	run()
}

func TestSubtestDisabled(t *testing.T) {
	t.Run("later", func(t *testing.T) {
		t.Skip("later")
	})
}

func TestSubtestConditional(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root")
		}
		run()
	})
}

func TestCrasher(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		crash()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestCrasher")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("no crash")
	}
}

func TestClosureDefault(t *testing.T) {
	get := func(k string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return "def"
	}
	run(get("MODE"))
}

func TestLogThenReturn(t *testing.T) {
	t.Run("case", func(t *testing.T) {
		if os.Getenv("RUN") == "" {
			t.Log("off")
			return
		}
		run()
	})
}

func TestClosureSkip(t *testing.T) {
	need := func() {
		if os.Getenv("RUN") == "" {
			t.Skip("off")
		}
	}
	need()
}

func TestDisabled(t *testing.T) {
	t.Skip("later")
	run()
}

func TestRootOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses modes")
	}
	run()
}

func TestCompare(t *testing.T) {
	if legacyResult() != result() {
	}
}
`

func TestDetectionAndAllowlist(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample_test.go"), []byte(sampleTests), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "fixtures"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fixtures", "ignored_test.go"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	found, files, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, f := range found {
		keys = append(keys, f.key())
	}
	want := []string{
		"sample_test.go|" + wholeFile + "|b:go/parser",
		"sample_test.go|TestClosureSkip|" + markEnvSkip,
		"sample_test.go|TestCompare|" + markLegacy,
		"sample_test.go|TestDisabled|" + markSkipped,
		"sample_test.go|TestElseIfChain|" + markEnvSkip,
		"sample_test.go|TestLogThenReturn|" + markEnvSkip,
		"sample_test.go|TestLookupElse|" + markEnvSkip,
		"sample_test.go|TestMeasure|" + markEnvSkip,
		"sample_test.go|TestRead|" + markProduct,
		"sample_test.go|TestRealEnvironment|" + markEnvSkip,
		"sample_test.go|TestReturnUnlessSet|" + markEnvSkip,
		"sample_test.go|TestSubtestDisabled|" + markSkipped,
		"sample_test.go|TestSwitchEnv|" + markEnvSkip,
		"sample_test.go|readShipped|" + markProduct,
	}
	if files != 1 || strings.Join(keys, "\n") != strings.Join(want, "\n") {
		t.Fatalf("files = %d, findings:\n%s", files, strings.Join(keys, "\n"))
	}
	var allow strings.Builder
	for _, f := range found {
		fmt.Fprintf(&allow, "%s\t理由\n", f.key())
	}
	if p := check(found, allow.String()); len(p) != 0 {
		t.Fatal(p)
	}
	if p := check(found[:len(found)-1], allow.String()); len(p) != 1 || p[0] != "検出されない許可項目（削除する）: "+want[len(want)-1] {
		t.Fatal(p)
	}
	if p := check(found, allow.String()+want[0]+"\t理由\n"); len(p) != 1 || p[0] != "重複した許可項目: "+want[0] {
		t.Fatal(p)
	}
	if p := check(found[:1], want[0]+"\t \n"); len(p) != 2 || !strings.HasPrefix(p[1], "理由のない許可項目: ") {
		t.Fatal(p)
	}
	p := check(found, "# 注記だけ\n")
	if len(p) != len(want) {
		t.Fatal(p)
	}
	for i, key := range want {
		if p[i] != key+": "+guidance {
			t.Fatalf("diagnostic = %q", p[i])
		}
	}
}

func TestUnrunScripts(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"a/check.test.sh":                                "",
		"nested/a/check.test.sh":                         "",
		"first/overwrite.test.sh":                        "",
		"second/overwrite.test.sh":                       "",
		"first/prior.test.sh":                            "",
		"second/prior.test.sh":                           "",
		"deploy/unstarted.test.sh":                       "",
		"deploy/echo.test.sh":                            "",
		"deploy/docker-echo.test.sh":                     "",
		"deploy/combined.test.sh":                        "",
		"deploy/argument.test.sh":                        "",
		"deploy/docker-argument.test.sh":                 "",
		"deploy/go-node.test.mts":                        "",
		"deploy/go-check.test.mjs":                       "",
		"deploy/docker-check.test.mjs":                   "",
		"deploy/go-long-noexec.test.sh":                  "",
		"deploy/go-noexec.test.sh":                       "",
		"deploy/go.test.sh":                              "echo ok",
		"deploy/variable.test.sh":                        "echo ok",
		"deploy/process-variable.test.sh":                "",
		"deploy/go-spread.test.sh":                       "",
		"deploy/process.test.sh":                         "echo ok",
		"deploy/go-errexit.test.sh":                      "",
		"deploy/docker-errexit.test.sh":                  "",
		"deploy/exec-image.test.mts":                     "",
		"deploy/go-eval.test.mjs":                        "",
		"deploy/docker-eval.test.mjs":                    "",
		"deploy/image.test.mjs":                          "",
		"deploy/continued.test.mjs":                      "",
		"deploy/comment.test.sh":                         "",
		"deploy/copied.test.mts":                         "",
		"deploy/string.test.sh":                          "",
		"deploy/sample.test.sh":                          "",
		"deploy/fixtures/x.test.sh":                      "",
		"deploy/plain.sh":                                "",
		"deploy/client/macos-live-check/install.test.sh": "",
		"other/install.test.sh":                          "",
		"deploy/image/duplicate.test.mjs":                "",
		"other/duplicate.test.mjs":                       "",
		"internal/a/runner_test.go": `package a

// exec.Command("bash", "../../deploy/comment.test.sh") は実行しない
func TestRun(t *testing.T) {
	exec.Command("bash", "../../nested/a/check.test.sh").Run()
    exec.Command("bash", "../../deploy/unstarted.test.sh")
    exec.Command("echo", "../../deploy/echo.test.sh").Run()
    exec.Command("bash", "../../deploy/combined.test.sh").CombinedOutput()
	exec.Command("bash", filepath.Join("..", "..", "deploy", "go.test.sh")).Run()
	script := "../../deploy/variable.test.sh"
	exec.CommandContext(ctx, "bash", script).Run()
	process.Run(ctx, "bash", []string{"../../deploy/process.test.sh"}, process.Options{})
	processArgs := []string{"../../deploy/process-variable.test.sh"}
	process.Run(ctx, "bash", processArgs, process.Options{})
	commandArgs := []string{"-ue", "../../deploy/go-spread.test.sh"}
	exec.Command("bash", commandArgs...).Run()
	exec.Command("bash", "-e", "-o", "pipefail", "../../deploy/go-errexit.test.sh").Run()
	exec.Command("sh", "../../deploy/plain.sh", "../../deploy/argument.test.sh").Run()
	exec.Command("node", "--test", "../../deploy/go-node.test.mts").Run()
	exec.Command("node", "--check", "../../deploy/go-check.test.mjs").Run()
	exec.Command("bash", "-n", "../../deploy/go-noexec.test.sh").Run()
	exec.Command("bash", "-o", "noexec", "../../deploy/go-long-noexec.test.sh").Run()
	exec.Command("node", "-e", "../../deploy/go-eval.test.mjs").Run()
	exec.Command("bash", "../../deploy/client/macos-live-check/install.test.sh").Run()
	name := "string.test.sh"
	_ = name
}
`,
		"internal/testlint/self_test.go": `package testlint

func TestSample(t *testing.T) { exec.Command("bash", "deploy/sample.test.sh") }
`,
		"Dockerfile.tools": `FROM node
COPY first/overwrite.test.sh /opt/overwrite.test.sh
COPY second/overwrite.test.sh /opt/overwrite.test.sh
RUN echo deploy/docker-echo.test.sh
RUN sh /opt/overwrite.test.sh
COPY first/prior.test.sh /opt/prior.test.sh
RUN sh /opt/prior.test.sh
COPY second/prior.test.sh /opt/prior.test.sh
COPY deploy/copied.test.mts /opt/
RUN echo /opt/copied.test.mts
COPY deploy/image/duplicate.test.mjs /opt/image-tools/
# RUN node --test /opt/comment.test.sh
ENV A=1 \
 B=continued.test.mjs
RUN true \
 && node --test --test-concurrency=1 /opt/image.test.mjs
RUN node --test /opt/image-tools/duplicate.test.mjs
RUN bash -e deploy/docker-errexit.test.sh
RUN sh deploy/plain.sh deploy/docker-argument.test.sh
RUN ["node", "--check", "deploy/docker-check.test.mjs"]
COPY deploy/exec-image.test.mts /opt/
RUN ["node", "--test", "--test-concurrency", "1", "/opt/exec-image.test.mts"]
RUN ["node", "--eval", "deploy/docker-eval.test.mjs"]
`,
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := unrunScripts(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, strings.SplitN(strings.TrimPrefix(p, "どの検証からも実行されないテスト: "), ":", 2)[0])
	}
	want := []string{"a/check.test.sh", "deploy/argument.test.sh", "deploy/comment.test.sh", "deploy/continued.test.mjs", "deploy/copied.test.mts", "deploy/docker-argument.test.sh", "deploy/docker-check.test.mjs", "deploy/docker-echo.test.sh", "deploy/docker-eval.test.mjs", "deploy/echo.test.sh", "deploy/go-check.test.mjs", "deploy/go-eval.test.mjs", "deploy/go-long-noexec.test.sh", "deploy/go-noexec.test.sh", "deploy/sample.test.sh", "deploy/string.test.sh", "deploy/unstarted.test.sh", "first/overwrite.test.sh", "other/duplicate.test.mjs", "other/install.test.sh", "second/prior.test.sh"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("got %q", got)
	}
}
