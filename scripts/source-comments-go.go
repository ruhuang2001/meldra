//go:build ignore

package main

import (
	"encoding/json"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"log"
	"os"
	"slices"
	"strings"
)

func semanticGroups(path string, file *ast.File) map[*ast.CommentGroup]bool {
	groups := make(map[*ast.CommentGroup]bool)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			continue
		}
		for _, spec := range gen.Specs {
			imp := spec.(*ast.ImportSpec)
			if imp.Path.Value == `"C"` {
				groups[imp.Doc] = true
				if len(gen.Specs) == 1 {
					groups[gen.Doc] = true
				}
			}
		}
	}
	if strings.HasSuffix(path, "_test.go") {
		for _, example := range doc.Examples(file) {
			if example.Output == "" && !example.EmptyOutput {
				continue
			}
			for _, decl := range file.Decls {
				f, ok := decl.(*ast.FuncDecl)
				if !ok || f.Name.Name != "Example"+example.Name {
					continue
				}
				for _, group := range slices.Backward(file.Comments) {
					if group.Pos() >= f.Body.Pos() && group.End() <= f.Body.End() {
						groups[group] = true
						break
					}
				}
			}
		}
	}
	return groups
}

type span struct {
	Path     string `json:"path"`
	Offset   int    `json:"offset"`
	Semantic bool   `json:"semantic"`
}

func main() {
	var paths []string
	if err := json.NewDecoder(os.Stdin).Decode(&paths); err != nil {
		log.Fatal(err)
	}
	out := json.NewEncoder(os.Stdout)
	for _, path := range paths {
		fs := token.NewFileSet()
		file, err := parser.ParseFile(fs, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			log.Fatal(err)
		}
		groups := semanticGroups(path, file)
		for _, group := range file.Comments {
			for _, comment := range group.List {
				if err := out.Encode(span{path, fs.PositionFor(comment.Slash, false).Offset, groups[group]}); err != nil {
					log.Fatal(err)
				}
			}
		}
	}
}
