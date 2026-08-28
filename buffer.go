// more details see https://gemini.google.com/app/d142c077a211cd58
package main


/*
import (
	"unicode/utf8"
)

// Position represents a zero-indexed coordinate inside a buffer.
type Position struct {
	Row int // Line index (0-based)
	Col int // Byte offset within the line (0-based)
}

// Buffer holds text content and coordinate resolution logic.
type Buffer struct {
	id    string
	lines [][]byte // Simplest storage model; or replaced with a Rope/Piece Table
}

// NewBufferx creates a new buffer from raw bytes.
func NewBuffer(id string, content []byte) *Buffer {
	// Split by newline while retaining byte structure
	// (For production, a Rope or Piece Table avoids large slice reallocations)
	return &Buffer{
		id:    id,
		lines: bytesSplitLines(content),
	}
}

// Get the raw byte slice for a specific line (O(1) operation)
func (b *Buffer) LineBytes(row int) []byte {
	if row < 0 || row >= len(b.lines) {
		return nil
	}
	return b.lines[row]
}

// Convert (Row, Col Byte) -> Rune Index
// Used when you need to know how many unicode characters precede the cursor.
func (b *Buffer) ByteToRuneCol(pos Position) int {
	line := b.LineBytes(pos.Row)
	if pos.Col >= len(line) {
		return utf8.RuneCount(line)
	}
	return utf8.RuneCount(line[:pos.Col])
}

// Convert (Row, Rune Index) -> Position (Byte Col)
// Used when moving the cursor horizontally by 'N' runes (e.g., arrow keys).
func (b *Buffer) RuneToByteCol(row int, runeCol int) Position {
	line := b.LineBytes(row)
	if len(line) == 0 || runeCol <= 0 {
		return Position{Row: row, Col: 0}
	}

	byteIdx := 0
	runesSeen := 0
	for byteIdx < len(line) && runesSeen < runeCol {
		_, size := utf8.DecodeRune(line[byteIdx:])
		byteIdx += size
		runesSeen++
	}

	return Position{Row: row, Col: byteIdx}
}

// Range represents a span of text within a single document.
type Range struct {
	Start Position
	End   Position
}

// Safe Slicing using byte offsets (O(1) operation)
func (b *Buffer) TextAt(r Range) []byte {
	// Fast zero-copy slicing using byte offsets directly
	if r.Start.Row == r.End.Row {
		line := b.LineBytes(r.Start.Row)
		return line[r.Start.Col:r.End.Col]
	}
	// Multi-line extraction logic...
	return nil
}
*/
