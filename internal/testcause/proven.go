package testcause

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var goOKPackage = regexp.MustCompile(`^ok\s+(\S+)\s+(?:[\d.]+s|\(cached\))`)

var skippedTreeDirs = map[string]bool{".git": true, "vendor": true, "node_modules": true, ".venv": true, "venv": true, "__pycache__": true, "dist": true, "build": true}

// ProvenGoPackages counts the distinct Go packages that the output reports as passed
// ("ok  <import path>") and that exist in tree with _test.go files. The import path is
// resolved through the go.mod module lines found in tree, so a summary printed without
// running the tree's tests (for example a bare printf of a node summary) proves nothing.
func ProvenGoPackages(tree, output string) int {
	if tree == "" || !strings.Contains(output, "ok") {
		return 0
	}
	modules := goModules(tree)
	if len(modules) == 0 {
		return 0
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		match := goOKPackage.FindStringSubmatch(strings.TrimRight(line, " \t"))
		if match == nil || strings.Contains(line, "[no tests to run]") || seen[match[1]] {
			continue
		}
		if dir := packageDir(modules, match[1]); dir != "" && hasGoTests(dir) {
			seen[match[1]] = true
		}
	}
	return len(seen)
}

// goModules maps module paths declared by go.mod files in tree to their directories.
func goModules(tree string) map[string]string {
	modules := map[string]string{}
	_ = filepath.WalkDir(tree, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if path != tree && skippedTreeDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "go.mod" {
			return nil
		}
		if module := moduleLine(tree, path); module != "" {
			modules[module] = filepath.Dir(path)
		}
		return nil
	})
	return modules
}

func moduleLine(tree, path string) string {
	name, err := filepath.Rel(tree, path)
	if err != nil {
		return ""
	}
	data, err := readTreeFile(tree, name, 1<<20)
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`)
		}
	}
	return ""
}

// packageDir returns the directory of importPath under the longest matching module.
func packageDir(modules map[string]string, importPath string) string {
	best, dir := "", ""
	for module, root := range modules {
		if importPath != module && !strings.HasPrefix(importPath, module+"/") {
			continue
		}
		if len(module) > len(best) {
			best, dir = module, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(importPath, module)))
		}
	}
	return dir
}

func hasGoTests(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), "_test.go") {
			return true
		}
	}
	return false
}

// ProvenGradleResults counts current, successful JUnit result files. Every
// current candidate must be complete; a good suite cannot hide a broken one.
func ProvenGradleResults(tree string, started time.Time) int {
	if tree == "" || started.IsZero() {
		return 0
	}
	root, err := os.OpenRoot(tree)
	if err != nil {
		return 0
	}
	defer root.Close()
	results, err := root.OpenRoot("build/test-results/test")
	if err != nil {
		return 0
	}
	defer results.Close()
	dir, err := results.Open(".")
	if err != nil {
		return 0
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "TEST-") || !strings.HasSuffix(entry.Name(), ".xml") {
			continue
		}
		info, err := results.Lstat(entry.Name())
		if err != nil || !info.Mode().IsRegular() {
			return 0
		}
		if !info.ModTime().After(started) {
			continue
		}
		file, err := results.Open(entry.Name())
		if err != nil {
			return 0
		}
		valid := successfulGradleXML(file)
		file.Close()
		if !valid {
			return 0
		}
		count++
	}
	return count
}

func successfulGradleXML(file *os.File) bool {
	// Bound untrusted reports, and never resolve references or external entities.
	const limit = 16 << 20
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return false
	}
	decoder := xml.NewDecoder(io.LimitReader(file, limit+1))
	depth, roots := 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return roots == 1 && depth == 0
		}
		if err != nil {
			return false
		}
		switch v := token.(type) {
		case xml.StartElement:
			seen := map[xml.Name]bool{}
			for _, attr := range v.Attr {
				if seen[attr.Name] {
					return false
				}
				seen[attr.Name] = true
			}
			if depth > 0 && v.Name.Local == "testsuite" {
				return false
			}
			if depth == 0 {
				roots++
				if roots != 1 || v.Name.Local != "testsuite" || !successfulSuiteCounts(v.Attr) {
					return false
				}
			}
			if v.Name.Local == "failure" || v.Name.Local == "error" {
				return false
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(v)) != "" {
				return false
			}
		case xml.Directive:
			return false
		}
	}
}

func successfulSuiteCounts(attrs []xml.Attr) bool {
	counts := map[string]int{}
	for _, attr := range attrs {
		if attr.Name.Local != "tests" && attr.Name.Local != "failures" && attr.Name.Local != "errors" {
			continue
		}
		if _, exists := counts[attr.Name.Local]; exists || attr.Name.Space != "" || attr.Value == "" || strings.Trim(attr.Value, "0123456789") != "" {
			return false
		}
		n, err := strconv.Atoi(attr.Value)
		if err != nil {
			return false
		}
		counts[attr.Name.Local] = n
	}
	return len(counts) == 3 && counts["tests"] > 0 && counts["failures"] == 0 && counts["errors"] == 0
}

var flutterPassed = regexp.MustCompile(`^(?:\d+:\d{2}(?::\d{2})? \+\d+(?: ~\d+)?: )?All tests passed!$`)
var flutterFailed = regexp.MustCompile(`^(?:\d+:\d{2}(?::\d{2})? \+\d+(?: ~\d+)? -[1-9]\d*(?: ~\d+)?: .*|(?:\d+:\d{2}(?::\d{2})? \+\d+(?: ~\d+)?: )?Some tests failed\.)$`)
var flutterANSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// ProvenFlutterResults requires a final runner success and regular test files
// inside tree/test. Progress updates separated by carriage returns are records too.
func ProvenFlutterResults(tree, output string) int {
	last := ""
	for _, line := range strings.Split(strings.ReplaceAll(flutterANSI.ReplaceAllString(output, ""), "\r", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if flutterFailed.MatchString(line) {
			return 0
		}
		if line != "" {
			last = line
		}
	}
	if tree == "" || !flutterPassed.MatchString(last) {
		return 0
	}
	root, err := os.OpenRoot(tree)
	if err != nil {
		return 0
	}
	defer root.Close()
	info, err := root.Lstat("test")
	if err != nil || !info.IsDir() {
		return 0
	}
	count := 0
	err = fs.WalkDir(root.FS(), "test", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), "_test.dart") {
			count++
		}
		return nil
	})
	if err != nil {
		return 0
	}
	return count
}
