package main

import (
	"go/token"

	"github.com/cansyan/kero"
)

var (
	Theme = DefaultTheme

	// default theme adapted to terminal
	DefaultTheme = map[string]string{
		"cursorFg":     "#000000", // black
		"cursorBg":     "#fac863", // orange
		"selectionBg":  "#4e5a65", // blue
		"searchFg":     "#000000", // Dark text for contrast against bright yellow
		"searchBg":     "#E5C07B", // One Dark Gold / Warm Amber
		"activeLineBg": "#65737e", // blue
	}

	// Mariana theme
	Mariana = map[string]string{
		"foreground":   "#d8dee9", // white
		"background":   "#303841", // blue
		"cursorFg":     "#000000", // black
		"cursorBg":     "#fac863", // orange
		"selectionBg":  "#4e5a65", // blue
		"searchFg":     "#000000", // Dark text for contrast against bright yellow
		"searchBg":     "#E5C07B", // One Dark Gold / Warm Amber
		"activeLineBg": "#65737e", // blue

		// syntax highlight
		"keyword": "#c594c5", // soft Purple
		"type":    "#6598CA", // blue
		"string":  "#99c794", // green
		"comment": "#a7adba", // blue
		"number":  "#fac863", // orange
		"builtin": "#F97B58", // red
	}
)

type TokenType int

const (
	TokKeyword TokenType = iota // package, func, var, go, return, etc.
	TokType                     // int, string, struct, bool, error, etc.
	TokString                   // "...", `...`, 'a'
	TokComment                  // // comment or /* comment */
	TokNumber                   // 123, 0x1F, 3.14
	TokBuiltin                  // nil, true, false, iota, make, len
)

// HLToken defines a highlight token range within a single line (0-indexed byte offsets).
type HLToken struct {
	StartCol int       // Byte offset start (inclusive)
	EndCol   int       // Byte offset end (exclusive)
	Type     TokenType // token type
}

// HighlightStyle returns the highlight style for the given token type.
func HighlightStyle(t TokenType) kero.Style {
	switch t {
	case TokKeyword:
		return kero.Style{Fg: kero.ColorHex(Theme["keyword"])}.Italic()
	case TokType:
		return kero.Style{Fg: kero.ColorHex(Theme["type"])}
	case TokString:
		return kero.Style{Fg: kero.ColorHex(Theme["string"])}
	case TokComment:
		return kero.Style{Fg: kero.ColorHex(Theme["comment"])}
	case TokNumber:
		return kero.Style{Fg: kero.ColorHex(Theme["number"])}
	case TokBuiltin:
		return kero.Style{Fg: kero.ColorHex(Theme["builtin"])}.Italic()
	default:
		return kero.Style{
			Fg: kero.ColorHex(Theme["foreground"]),
			Bg: kero.ColorHex(Theme["background"]),
		}
	}
}

// Pre-built maps for Go standard types and builtins (since token doesn't classify these)
var goTypes = map[string]bool{
	"bool": true, "byte": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true,
	"int8": true, "int16": true, "int32": true, "int64": true,
	"rune": true, "string": true, "uint": true, "uint8": true,
	"uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"any": true,
}

var goBuiltins = map[string]bool{
	"true": true, "false": true, "iota": true, "nil": true,
	"append": true, "cap": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true,
	"make": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
}

// LineState tracks state continuation between consecutive lines.
type LineState int

const (
	StateNormal LineState = iota
	StateInBlockComment
	StateInRawString
)

// HighlightToken tokenizes a single line of bytes.
// Returns the slice of tokens and the ending state to pass to the next line.
func HighlightToken(line []byte, startState LineState) ([]HLToken, LineState) {
	var tokens []HLToken
	i := 0
	n := len(line)
	state := startState

	addToken := func(start, end int, tokType TokenType) {
		if start < end {
			tokens = append(tokens, HLToken{
				StartCol: start,
				EndCol:   end,
				Type:     tokType,
			})
		}
	}

	for i < n {
		// 1. Continue Multi-line Block Comment
		if state == StateInBlockComment {
			start := i
			for i < n {
				if i+1 < n && line[i] == '*' && line[i+1] == '/' {
					i += 2
					state = StateNormal
					break
				}
				i++
			}
			addToken(start, i, TokComment)
			continue
		}

		// 2. Continue Multi-line Raw String (`...`)
		if state == StateInRawString {
			start := i
			for i < n {
				if line[i] == '`' {
					i++
					state = StateNormal
					break
				}
				i++
			}
			addToken(start, i, TokString)
			continue
		}

		// Skip whitespace
		if line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n' {
			i++
			continue
		}

		// 3. Line Comments (// ...)
		if i+1 < n && line[i] == '/' && line[i+1] == '/' {
			addToken(i, n, TokComment)
			i = n
			break
		}

		// 4. Start Block Comment (/* ...)
		if i+1 < n && line[i] == '/' && line[i+1] == '*' {
			start := i
			i += 2
			state = StateInBlockComment
			for i < n {
				if i+1 < n && line[i] == '*' && line[i+1] == '/' {
					i += 2
					state = StateNormal
					break
				}
				i++
			}
			addToken(start, i, TokComment)
			continue
		}

		// 5. Interpreted String ("...") & Char Literal ('...')
		if line[i] == '"' || line[i] == '\'' {
			quote := line[i]
			start := i
			i++
			for i < n {
				if line[i] == '\\' && i+1 < n {
					i += 2 // Skip escaped character
					continue
				}
				if line[i] == quote {
					i++
					break
				}
				i++
			}
			addToken(start, i, TokString)
			continue
		}

		// 6. Start Raw String (`...)
		if line[i] == '`' {
			start := i
			i++
			state = StateInRawString
			for i < n {
				if line[i] == '`' {
					i++
					state = StateNormal
					break
				}
				i++
			}
			addToken(start, i, TokString)
			continue
		}

		// 7. Numbers (123, 0xAF, 3.1415, 1e9)
		if isDigit(line[i]) {
			start := i
			for i < n && (isHexOrDigit(line[i]) || line[i] == '.' || line[i] == '_') {
				i++
			}
			addToken(start, i, TokNumber)
			continue
		}

		// 8. Identifiers (Keywords, Types, Builtins, Var names)
		if isLetter(line[i]) || line[i] == '_' {
			start := i
			for i < n && (isLetter(line[i]) || isDigit(line[i]) || line[i] == '_') {
				i++
			}
			word := string(line[start:i])
			if token.IsKeyword(word) {
				addToken(start, i, TokKeyword)
			} else if goTypes[word] {
				addToken(start, i, TokType)
			} else if goBuiltins[word] {
				addToken(start, i, TokBuiltin)
			}
			continue
		}

		// Operators, punctuation, or single non-identifier bytes
		i++
	}

	return tokens, state
}

// Helper predicates
func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isHexOrDigit(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F') || b == 'x' || b == 'X'
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// ScanLineState updates line state without allocating token slices.
func ScanLineState(line []byte, startState LineState) LineState {
	state := startState
	i := 0
	n := len(line)

	for i < n {
		if state == StateInBlockComment {
			for i < n {
				if i+1 < n && line[i] == '*' && line[i+1] == '/' {
					i += 2
					state = StateNormal
					break
				}
				i++
			}
			continue
		}

		if state == StateInRawString {
			for i < n {
				if line[i] == '`' {
					i++
					state = StateNormal
					break
				}
				i++
			}
			continue
		}

		// line comment
		if i+1 < n && line[i] == '/' && line[i+1] == '/' {
			state = StateNormal
			break
		}

		// Fast check for start of block comment / raw string
		if i+1 < n && line[i] == '/' && line[i+1] == '*' {
			state = StateInBlockComment
			i += 2
			continue
		}

		if line[i] == '`' {
			state = StateInRawString
			i++
			continue
		}

		// Skip standard string escapes to avoid false '`' triggers inside "..."
		if line[i] == '"' || line[i] == '\'' {
			quote := line[i]
			i++
			for i < n {
				if line[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if line[i] == quote {
					i++
					break
				}
				i++
			}
			continue
		}

		i++
	}

	return state
}
