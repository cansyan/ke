package main

import (
	"go/token"

	"github.com/cansyan/kero"
)

// HighlightToken defines a styled range within a single line (0-indexed byte offsets).
type HighlightToken struct {
	StartCol int        // Byte offset start (inclusive)
	EndCol   int        // Byte offset end (exclusive)
	Style    kero.Style // Theme style
}

// GoSyntaxTheme holds styles for language constructs.
type GoSyntaxTheme struct {
	Keyword kero.Style // package, func, var, go, return, etc.
	Type    kero.Style // int, string, struct, bool, error, etc.
	String  kero.Style // "...", `...`, 'a'
	Comment kero.Style // // comment or /* comment */
	Number  kero.Style // 123, 0x1F, 3.14
	Builtin kero.Style // nil, true, false, iota, make, len
}

func DefaultGoTheme() GoSyntaxTheme {
	return GoSyntaxTheme{
		Keyword: kero.NewStyle().Foreground(kero.ColorMagenta).Bold(),
		Type:    kero.NewStyle().Foreground(kero.ColorCyan),
		String:  kero.NewStyle().Foreground(kero.ColorGreen),
		Comment: kero.NewStyle().Dim(),
		Number:  kero.NewStyle().Foreground(kero.ColorYellow),
		Builtin: kero.NewStyle().Foreground(kero.ColorBlue),
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

// HighlightGoLine tokenizes a single line of bytes.
// Returns the slice of tokens and the ending state to pass to the next line.
func HighlightGoLine(line []byte, startState LineState, theme GoSyntaxTheme) ([]HighlightToken, LineState) {
	var tokens []HighlightToken
	i := 0
	n := len(line)
	state := startState

	addToken := func(start, end int, style kero.Style) {
		if start < end {
			tokens = append(tokens, HighlightToken{
				StartCol: start,
				EndCol:   end,
				Style:    style,
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
			addToken(start, i, theme.Comment)
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
			addToken(start, i, theme.String)
			continue
		}

		// Skip whitespace
		if line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n' {
			i++
			continue
		}

		// 3. Line Comments (// ...)
		if i+1 < n && line[i] == '/' && line[i+1] == '/' {
			addToken(i, n, theme.Comment)
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
			addToken(start, i, theme.Comment)
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
			addToken(start, i, theme.String)
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
			addToken(start, i, theme.String)
			continue
		}

		// 7. Numbers (123, 0xAF, 3.1415, 1e9)
		if isDigit(line[i]) {
			start := i
			for i < n && (isHexOrDigit(line[i]) || line[i] == '.' || line[i] == '_') {
				i++
			}
			addToken(start, i, theme.Number)
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
				addToken(start, i, theme.Keyword)
			} else if goTypes[word] {
				addToken(start, i, theme.Type)
			} else if goBuiltins[word] {
				addToken(start, i, theme.Builtin)
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

// ScanLineState updates currentState without allocating token slices.
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
