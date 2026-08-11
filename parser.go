package main

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// DefinitionResult holds the target location for a definition jump.
type DefinitionResult struct {
	Found  bool
	Line   int // 1-based
	Column int // 1-based
}

// FindDefinitionLoc returns the target definition line/col for the identifier
// at the given cursor position (1-based line, 1-based col).
func FindDefinitionLoc(src any, cursorLine, cursorCol int) DefinitionResult {
	fset := token.NewFileSet()
	// src can be string, []byte, or io.Reader
	file, err := parser.ParseFile(fset, "buffer.go", src, parser.SkipObjectResolution)
	if err != nil && file == nil {
		return DefinitionResult{}
	}

	targetPos := positionToPos(fset, file, cursorLine, cursorCol)
	if !targetPos.IsValid() {
		return DefinitionResult{}
	}

	// 1. Find the *ast.Ident under the cursor
	var targetIdent *ast.Ident
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return true
		}
		if n.Pos() <= targetPos && targetPos <= n.End() {
			if ident, ok := n.(*ast.Ident); ok {
				targetIdent = ident
			}
			return true
		}
		return false
	})

	if targetIdent == nil {
		return DefinitionResult{}
	}

	// 2. Find declaration by inspecting the AST scopes explicitly
	declPos := findDeclarationPos(file, targetIdent)
	if !declPos.IsValid() {
		return DefinitionResult{}
	}

	pos := fset.Position(declPos)
	return DefinitionResult{
		Found:  true,
		Line:   pos.Line,
		Column: pos.Column,
	}
}

// findDeclarationPos replaces the deprecated Ident.Obj lookup by walking
// local function scopes and top-level declarations in the file.
func findDeclarationPos(file *ast.File, target *ast.Ident) token.Pos {
	name := target.Name
	var match token.Pos

	// Step A: Search for local variables / parameters inside the enclosing function
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}

		// Check if target is inside this function body
		if fn.Pos() <= target.Pos() && target.Pos() <= fn.End() {
			// Check function parameters and return values
			if fn.Type != nil && fn.Type.Params != nil {
				for _, param := range fn.Type.Params.List {
					for _, id := range param.Names {
						if id.Name == name {
							match = id.Pos()
							return false
						}
					}
				}
			}

			// Check local variable assignments inside function body
			ast.Inspect(fn.Body, func(bodyNode ast.Node) bool {
				switch stmt := bodyNode.(type) {
				case *ast.AssignStmt: // e.g. x := 10 or x, y := 1, 2
					for _, lh := range stmt.Lhs {
						if id, ok := lh.(*ast.Ident); ok && id.Name == name {
							if id.Pos() <= target.Pos() { // Defined before or at target
								match = id.Pos()
								return false
							}
						}
					}
				case *ast.ValueSpec: // e.g. var x int
					for _, id := range stmt.Names {
						if id.Name == name {
							match = id.Pos()
							return false
						}
					}
				}
				return match == token.NoPos
			})

			return false
		}
		return true
	})

	if match.IsValid() {
		return match
	}

	// Step B: Search top-level file declarations (funcs, structs, types, consts, vars)
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == name {
				return d.Name.Pos()
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.Name == name {
						return s.Name.Pos()
					}
					// If searching for a struct field name
					if st, ok := s.Type.(*ast.StructType); ok {
						for _, field := range st.Fields.List {
							for _, id := range field.Names {
								if id.Name == name {
									return id.Pos()
								}
							}
						}
					}
				case *ast.ValueSpec:
					for _, id := range s.Names {
						if id.Name == name {
							return id.Pos()
						}
					}
				}
			}
		}
	}

	return token.NoPos
}

func positionToPos(fset *token.FileSet, file *ast.File, line, col int) token.Pos {
	tf := fset.File(file.Pos())
	if tf == nil || line < 1 || line > tf.LineCount() {
		return token.NoPos
	}
	return tf.LineStart(line) + token.Pos(col-1)
}
