// Command sdkenglish fails when the open-source trees contain Chinese outside of data: the plugin SDK in pluginapi/,
// the plugin authoring framework in plugins/export/, and the official plugins that are published as open source
// (every official plugin except nameparse). They are published to an international audience, so their comments and
// documents are written in English. The closed-source nameparse plugin is not checked.
//
// Checked: comments in Go files, and every line of the other text files (Markdown, proto, shell, YAML…).
// Not checked: Go string literals (translation tables, parsing rules and test inputs are data), AGENTS.md
// (internal to this repo, never exported), CATALOG.json (localized catalog entries), testdata/ and locales/
// directories, and binary files.
//
// Generated from the Kryspal Media main repository (scripts/sdkenglish) with the root list removed.
// Usage (from the repository root): go run ./.github/scripts/english
package main

import (
	"bytes"
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path"
	"strings"
	"unicode"
)

func main() {
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sdkenglish: git ls-files:", err)
		os.Exit(2)
	}
	var findings []string
	for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if name == "" || skipped(name) {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sdkenglish:", err)
			os.Exit(2)
		}
		findings = append(findings, check(name, src)...)
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "sdkenglish: %d line(s) with Chinese in the open-source trees; write comments and documents in English\n", len(findings))
		os.Exit(1)
	}
}

// skipped reports whether a tracked file is outside the rule.
func skipped(name string) bool {
	base := path.Base(name)
	if base == "AGENTS.md" || base == "CATALOG.json" {
		return true
	}
	for _, dir := range strings.Split(path.Dir(name), "/") {
		if dir == "testdata" || dir == "locales" {
			return true
		}
	}
	return false
}

// check returns "file:line: text" for each line of src that breaks the rule.
func check(name string, src []byte) []string {
	if bytes.IndexByte(src, 0) >= 0 {
		return nil // binary
	}
	var lines []int
	if strings.HasSuffix(name, ".go") {
		lines = goCommentLines(src)
	} else {
		for i, line := range bytes.Split(src, []byte("\n")) {
			if hasHan(string(line)) {
				lines = append(lines, i+1)
			}
		}
	}
	all := bytes.Split(src, []byte("\n"))
	out := make([]string, 0, len(lines))
	for _, n := range lines {
		out = append(out, fmt.Sprintf("%s:%d: %s", name, n, strings.TrimSpace(string(all[n-1]))))
	}
	return out
}

// goCommentLines returns the lines of Go comments that contain Chinese (a block comment can span several).
func goCommentLines(src []byte) []int {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, scanner.ScanComments)
	var out []int
	seen := map[int]bool{}
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok != token.COMMENT {
			continue
		}
		start := file.Line(pos)
		for i, line := range strings.Split(lit, "\n") {
			if n := start + i; hasHan(line) && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
}

func hasHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}
