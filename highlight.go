package main

import (
	"log"

	"github.com/cansyan/ke/lsp"
	"github.com/cansyan/kero"
)

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

func GetTokenTypeStyle(tokenType string) kero.Style {
	switch tokenType {
	case "keyword":
		return kero.NewStyle().Foreground(kero.ColorMagenta)
	case "type", "struct", "interface", "class", "enum", "typeParameter":
		return kero.NewStyle().Foreground(kero.ColorBlue)
	case "function", "method", "macro":
		// return kero.NewStyle().Foreground(kero.ColorCyan)
		// use plain color to avoid noise
		return kero.NewStyle()
	case "string", "regexp":
		return kero.NewStyle().Foreground(kero.ColorGreen)
	case "number":
		return kero.NewStyle().Foreground(kero.ColorYellow)
	case "comment":
		return kero.NewStyle().Dim()
	case "operator":
		return kero.NewStyle().Foreground(kero.ColorMagenta)
	case "parameter", "variable", "property", "enumMember", "event", "namespace":
		fallthrough
	default:
		return kero.NewStyle()
	}
}

type HighlightToken struct {
	Line      int
	StartCol  int // Byte column
	EndCol    int // Byte column
	TokenType string
}

// DecodeSemanticTokens expands delta-encoded LSP 5-tuples into absolute tokens.
func DecodeSemanticTokens(data []uint32, legend []string, lineBytes [][]byte) []HighlightToken {
	if len(data)%5 != 0 {
		return nil
	}

	tokens := make([]HighlightToken, 0, len(data)/5)
	var currentLine int
	var currentStartChar int

	for i := 0; i < len(data); i += 5 {
		deltaLine := int(data[i])
		deltaStartChar := int(data[i+1])
		lengthUtf16 := int(data[i+2])
		typeIdx := int(data[i+3])
		// _ = data[i+4] // tokenModifiers bitmask (can be used for bold/italic/readonly)

		if deltaLine > 0 {
			currentLine += deltaLine
			currentStartChar = deltaStartChar
		} else {
			currentStartChar += deltaStartChar
		}

		tokenType := "default"
		if typeIdx < len(legend) {
			tokenType = legend[typeIdx]
		}

		if currentLine >= len(lineBytes) {
			continue
		}

		// Convert LSP UTF-16 character offsets back to byte offsets for Buffer
		line := lineBytes[currentLine]
		startByte := lsp.CharToByteOffset(line, currentStartChar)
		endByte := lsp.CharToByteOffset(line, currentStartChar+lengthUtf16)

		tokens = append(tokens, HighlightToken{
			Line:      currentLine,
			StartCol:  startByte,
			EndCol:    endByte,
			TokenType: tokenType,
		})
	}

	return tokens
}

// Request and update highlights asynchronously
func (e *Editor) RefreshHighlights(buf *Buffer) {
	if e.lspClient == nil || buf == nil || buf.Path == "" {
		return
	}

	go func() {
		uri := pathToURI(buf.Path)
		res, err := e.lspClient.GetSemanticTokens(uri)
		if err != nil || res == nil {
			log.Printf("GetSemanticTokens: %s", err)
			return
		}

		tokens := DecodeSemanticTokens(res.Data, lsp.DefaultGoplsLegend, buf.Lines)

		// Group tokens by line number for fast rendering lookup O(1)
		lineHighlights := make(map[int][]HighlightToken)
		for _, tok := range tokens {
			lineHighlights[tok.Line] = append(lineHighlights[tok.Line], tok)
		}

		buf.Mu.Lock()
		buf.LineHighlights = lineHighlights
		buf.Mu.Unlock()
	}()
}
