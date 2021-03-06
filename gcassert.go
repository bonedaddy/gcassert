// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package gcassert

import (
	"bufio"
	"errors"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

type assertDirective int

const (
	noDirective assertDirective = iota
	inline
	bce
)

func stringToDirective(s string) (assertDirective, error) {
	switch s {
	case "inline":
		return inline, nil
	case "bce":
		return bce, nil
	}
	return noDirective, errors.New(fmt.Sprintf("no such directive %s", s))
}

type lineInfo struct {
	n          ast.Node
	directives []assertDirective
	// passedDirective is a map from index into the directives slice to a
	// boolean that says whether or not the directive succeeded, in the case
	// of directives like inlining that have compiler output if they passed.
	// For directives like bce that have compiler output if they failed, there's
	// no entry in this map.
	passedDirective map[int]bool
}

var gcAssertRegex = regexp.MustCompile(`//gcassert:(\w+)`)

type assertVisitor struct {
	commentMap ast.CommentMap

	directiveMap map[int]lineInfo
	fileSet      *token.FileSet
}

func newAssertVisitor(commentMap ast.CommentMap, fileSet *token.FileSet) assertVisitor {
	return assertVisitor{
		commentMap:   commentMap,
		fileSet:      fileSet,
		directiveMap: make(map[int]lineInfo),
	}
}

func (v assertVisitor) Visit(node ast.Node) (w ast.Visitor) {
	if node == nil {
		return w
	}
	m := v.commentMap[node]
COMMENTLOOP:
	for _, g := range m {
		for _, c := range g.List {
			matches := gcAssertRegex.FindStringSubmatch(c.Text)
			if len(matches) == 0 {
				continue COMMENTLOOP
			}
			// The 0th match is the whole string, and the 1st match is the
			// gcassert directive.

			directive, err := stringToDirective(matches[1])
			if err != nil {
				continue COMMENTLOOP
			}
			pos := node.Pos()
			lineNumber := v.fileSet.Position(pos).Line
			lineInfo := v.directiveMap[lineNumber]
			lineInfo.directives = append(lineInfo.directives, directive)
			lineInfo.n = node
			v.directiveMap[lineNumber] = lineInfo
		}
	}
	return v
}

// GCAssert searches through the packages at the input path and writes failures
// to comply with //gcassert directives to the given io.Writer.
func GCAssert(path string, w io.Writer) error {
	fileSet := token.NewFileSet()
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedCompiledGoFiles,
		Fset: fileSet,
	}, path)
	directiveMap, err := parseDirectives(pkgs, fileSet)
	if err != nil {
		return err
	}

	// Next: invoke Go compiler with -m flags to get the compiler to print
	// its optimization decisions.

	args := append([]string{"build", "-gcflags=all=-m -m -d=ssa/check_bce/debug=1"}, "./"+path)
	cmd := exec.Command("go", args...)
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cmd.Dir = cwd
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	cmdErr := make(chan error, 1)
	go func() {
		cmdErr <- cmd.Run()
		pw.Close()
	}()

	scanner := bufio.NewScanner(pr)
	optInfo := regexp.MustCompile(`([\.\/\w]+):(\d+):\d+: (.*)`)
	boundsCheck := "Found IsInBounds"
	sliceBoundsCheck := "Found SliceIsInBounds"

	for scanner.Scan() {
		line := scanner.Text()
		matches := optInfo.FindStringSubmatch(line)
		if len(matches) != 0 {
			path := matches[1]
			lineNo, err := strconv.Atoi(matches[2])
			if err != nil {
				return err
			}
			message := matches[3]

			if lineToDirectives := directiveMap[path]; lineToDirectives != nil {
				info := lineToDirectives[lineNo]
				if info.passedDirective == nil {
					info.passedDirective = make(map[int]bool)
					lineToDirectives[lineNo] = info
				}
				for i, d := range info.directives {
					switch d {
					case bce:
						if message == boundsCheck || message == sliceBoundsCheck {
							// Error! We found a bounds check where the user expected
							// there to be none.
							// Print out the user's code lineNo that failed the assertion,
							// the assertion itself, and the compiler output that
							// proved that the assertion failed.
							if err := printAssertionFailure(cwd, fileSet, info, w, message); err != nil {
								return err
							}
						}
					case inline:
						if strings.HasPrefix(message, "inlining call to") {
							info.passedDirective[i] = true
						}
					}
				}
			}
		}
	}

	for _, lineToDirectives := range directiveMap {
		for _, info := range lineToDirectives {
			for i, d := range info.directives {
				// An inlining directive passes if it has compiler output. For
				// each inlining directive, check if there was matching compiler
				// output and fail if not.
				if d == inline {
					if !info.passedDirective[i] {
						if err := printAssertionFailure(
							cwd, fileSet, info, w, "call was not inlined"); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

func printAssertionFailure(cwd string, fileSet *token.FileSet, info lineInfo, w io.Writer, message string) error {
	var buf strings.Builder
	_ = printer.Fprint(&buf, fileSet, info.n)
	pos := fileSet.Position(info.n.Pos())
	relPath, err := filepath.Rel(cwd, pos.Filename)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s:%d:\t%s: %s\n", relPath, pos.Line, buf.String(), message)
	return nil
}

type directiveMap map[string]map[int]lineInfo

func parseDirectives(pkgs []*packages.Package, fileSet *token.FileSet) (directiveMap, error) {
	fileDirectiveMap := make(directiveMap)
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for _, pkg := range pkgs {
		for i, file := range pkg.Syntax {
			commentMap := ast.NewCommentMap(fileSet, file, file.Comments)

			v := newAssertVisitor(commentMap, fileSet)
			// First: find all lines of code annotated with our gcassert directives.
			ast.Walk(v, file)

			if len(v.directiveMap) > 0 {
				absPath := pkg.CompiledGoFiles[i]
				relPath, err := filepath.Rel(cwd, absPath)
				if err != nil {
					return nil, err
				}
				fileDirectiveMap[relPath] = v.directiveMap
			}
		}
	}
	return fileDirectiveMap, nil
}
