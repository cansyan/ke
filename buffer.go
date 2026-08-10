package main

import (
	"iter"
	"slices"
	"strings"
	"unicode"
)

type Position struct {
	Row int
	Col int
}

type Buffer struct {
	lines [][]rune // Using [][]rune handles multi-byte UTF-8 correctly
}

func NewBuffer(content string) *Buffer {
	rawLines := strings.Split(content, "\n")
	lines := make([][]rune, len(rawLines))
	for i, l := range rawLines {
		lines[i] = []rune(l)
	}
	return &Buffer{lines: lines}
}

// Line returns a copy of the line at the specified row.
// Returns nil if the row index is out of bounds.
func (b *Buffer) Line(row int) []rune {
	if row < 0 || row >= len(b.lines) {
		return nil
	}
	src := b.lines[row]
	if src == nil {
		return nil
	}

	dst := make([]rune, len(src))
	copy(dst, src)
	return dst
}

func (b *Buffer) SetLine(row int, line []rune) {
	if row < 0 || row >= len(b.lines) {
		return
	}

	// Copy input slice to prevent external modification
	dst := make([]rune, len(line))
	copy(dst, line)
	b.lines[row] = dst
}

func (b *Buffer) LenLines() int {
	return len(b.lines)
}

func (b *Buffer) Lines() iter.Seq2[int, []rune] {
	return slices.All(b.lines)
}

func (b *Buffer) String() string {
	var sb strings.Builder
	for i := 0; i < len(b.lines)-1; i++ {
		sb.WriteString(string(b.lines[i]))
		sb.WriteRune('\n')
	}
	sb.WriteString(string(b.lines[len(b.lines)-1]))
	return sb.String()
}

// InsertAt inserts text at the given position and returns the new cursor position.
func (b *Buffer) Insert(p Position, text string) Position {
	if p.Row < 0 || p.Row >= len(b.lines) {
		return p
	}

	line := b.lines[p.Row]
	if p.Col < 0 {
		p.Col = 0
	}
	if p.Col > len(line) {
		p.Col = len(line)
	}

	insertLines := strings.Split(text, "\n")

	// Single-line insertion fast path
	if len(insertLines) == 1 {
		runesToInsert := []rune(insertLines[0])
		newLine := make([]rune, 0, len(line)+len(runesToInsert))
		newLine = append(newLine, line[:p.Col]...)
		newLine = append(newLine, runesToInsert...)
		newLine = append(newLine, line[p.Col:]...)

		b.lines[p.Row] = newLine

		return Position{
			Row: p.Row,
			Col: p.Col + len(runesToInsert),
		}
	}

	// Multi-line insertion path
	prefix := line[:p.Col]
	suffix := line[p.Col:]

	firstInsert := []rune(insertLines[0])
	lastInsert := []rune(insertLines[len(insertLines)-1])

	// First line gets prefix + first line of inserted text
	firstLine := append([]rune{}, prefix...)
	firstLine = append(firstLine, firstInsert...)

	// Last line gets last line of inserted text + suffix
	lastLine := append([]rune{}, lastInsert...)
	lastLine = append(lastLine, suffix...)

	// Prepare middle lines (if any)
	newSegment := make([][]rune, 0, len(insertLines))
	newSegment = append(newSegment, firstLine)

	for i := 1; i < len(insertLines)-1; i++ {
		newSegment = append(newSegment, []rune(insertLines[i]))
	}
	newSegment = append(newSegment, lastLine)

	// Replace target line with the expanded multi-line segment
	finalLines := make([][]rune, 0, len(b.lines)+len(insertLines)-1)
	finalLines = append(finalLines, b.lines[:p.Row]...)
	finalLines = append(finalLines, newSegment...)
	finalLines = append(finalLines, b.lines[p.Row+1:]...)

	b.lines = finalLines

	return Position{
		Row: p.Row + len(insertLines) - 1,
		Col: len(lastInsert),
	}
}

// orderPos guarantees start <= end (top-to-bottom, left-to-right)
func orderPos(p1, p2 Position) (start, end Position) {
	if p1.Row < p2.Row || (p1.Row == p2.Row && p1.Col <= p2.Col) {
		return p1, p2
	}
	return p2, p1
}

// ClampPos ensures p falls within valid buffer bounds.
func (b *Buffer) ClampPos(p Position) Position {
	if len(b.lines) == 0 {
		return Position{Row: 0, Col: 0}
	}

	// Clamp Row
	row := p.Row
	if row < 0 {
		row = 0
	} else if row >= len(b.lines) {
		row = len(b.lines) - 1
	}

	// Clamp Col within the valid row
	lineLen := len(b.lines[row])
	col := p.Col
	if col < 0 {
		col = 0
	} else if col > lineLen {
		col = lineLen
	}

	return Position{Row: row, Col: col}
}

// GetRange extracts the text between two positions (inclusive start, exclusive end).
func (b *Buffer) GetRange(p1, p2 Position) string {
	start, end := orderPos(b.ClampPos(p1), b.ClampPos(p2))

	if start == end {
		return ""
	}

	// Single-line range fast path
	if start.Row == end.Row {
		return string(b.lines[start.Row][start.Col:end.Col])
	}

	// Multi-line range path
	var sb strings.Builder

	// First line fragment
	sb.WriteString(string(b.lines[start.Row][start.Col:]))
	sb.WriteRune('\n')

	// Intermediate full lines
	for l := start.Row + 1; l < end.Row; l++ {
		sb.WriteString(string(b.lines[l]))
		sb.WriteRune('\n')
	}

	// Final line fragment
	sb.WriteString(string(b.lines[end.Row][:end.Col]))

	return sb.String()
}

// DeleteRange removes text between two positions [p1, p2) and returns the new cursor position.
func (b *Buffer) DeleteRange(p1, p2 Position) Position {
	start, end := orderPos(b.ClampPos(p1), b.ClampPos(p2))

	if start == end {
		return start
	}

	// Single-line deletion fast path
	if start.Row == end.Row {
		line := b.lines[start.Row]
		newLine := make([]rune, 0, len(line)-(end.Col-start.Col))
		newLine = append(newLine, line[:start.Col]...)
		newLine = append(newLine, line[end.Col:]...)

		b.lines[start.Row] = newLine
		return start
	}

	// Multi-line deletion path
	startLinePrefix := b.lines[start.Row][:start.Col]
	endLineSuffix := b.lines[end.Row][end.Col:]

	// Stitch start prefix and end suffix into one merged line
	mergedLine := make([]rune, 0, len(startLinePrefix)+len(endLineSuffix))
	mergedLine = append(mergedLine, startLinePrefix...)
	mergedLine = append(mergedLine, endLineSuffix...)

	// Rebuild line slice removing deleted lines
	newLines := make([][]rune, 0, len(b.lines)-(end.Row-start.Row))
	newLines = append(newLines, b.lines[:start.Row]...)
	newLines = append(newLines, mergedLine)
	newLines = append(newLines, b.lines[end.Row+1:]...)

	b.lines = newLines

	return start
}

// WordBounds finds the start and end of the word surrounding pos on its line.
func (b *Buffer) WordBounds(p Position) (start, end Position) {
	if p.Row < 0 || p.Row >= len(b.lines) {
		return
	}

	line := b.lines[p.Row]
	if len(line) == 0 {
		return
	}

	col := p.Col
	if col >= len(line) {
		col = len(line) - 1
	}

	// Determine matching mode based on target character under cursor
	targetIsWord := isWordChar(line[col])

	// Scan left for start
	startCol := col
	for startCol > 0 && isWordChar(line[startCol-1]) == targetIsWord {
		startCol--
	}

	// Scan right for end
	endCol := col
	for endCol < len(line) && isWordChar(line[endCol]) == targetIsWord {
		endCol++
	}

	start = Position{Row: p.Row, Col: startCol}
	end = Position{Row: p.Row, Col: endCol}
	return start, end
}

// FindNext searches forward starting after "from" for an exact query.
// Returns the position matching query, and true if found.
func (b *Buffer) FindNext(query string, from Position) (start, end Position, ok bool) {
	return b.findNext(query, from, false)
}

// FindNextIgnoreCase searches forward starting after "from" for query text,
// ignoring case.
func (b *Buffer) FindNextIgnoreCase(query string, from Position) (start, end Position, ok bool) {
	return b.findNext(query, from, true)
}

func (b *Buffer) findNext(query string, from Position, ignoreCase bool) (start, end Position, ok bool) {
	if query == "" || len(b.lines) == 0 {
		return Position{}, Position{}, false
	}

	queryRunes := []rune(query)
	if len(queryRunes) == 0 {
		return Position{}, Position{}, false
	}

	row := from.Row
	col := from.Col
	for rowsSearched := 0; rowsSearched <= len(b.lines); rowsSearched++ {
		line := b.lines[row]
		for i := col; i <= len(line)-len(queryRunes); i++ {
			match := true
			for j := range queryRunes {
				if !runeMatches(line[i+j], queryRunes[j], ignoreCase) {
					match = false
					break
				}
			}
			if match {
				start = Position{Row: row, Col: i}
				end = Position{Row: row, Col: i + len(queryRunes)}
				return start, end, true
			}
		}

		if row+1 < len(b.lines) {
			row++
		} else {
			row = 0
		}
		col = 0
	}

	return Position{}, Position{}, false
}

// FindPrev searches backward starting before fromPos for an exact query.
// Returns the start and end positions matching query, and true if found.
func (b *Buffer) FindPrev(query string, from Position) (start, end Position, ok bool) {
	return b.findPrev(query, from, false)
}

// FindPrevIgnoreCase searches backward starting before fromPos for query text,
// ignoring case.
func (b *Buffer) FindPrevIgnoreCase(query string, from Position) (start, end Position, ok bool) {
	return b.findPrev(query, from, true)
}

func (b *Buffer) findPrev(query string, from Position, ignoreCase bool) (start, end Position, ok bool) {
	if query == "" || len(b.lines) == 0 {
		return Position{}, Position{}, false
	}

	queryRunes := []rune(query)
	if len(queryRunes) == 0 {
		return Position{}, Position{}, false
	}
	qLen := len(queryRunes)

	row := from.Row
	for rowsSearched := 0; rowsSearched <= len(b.lines); rowsSearched++ {
		line := b.lines[row]
		// Determine the rightmost starting column index for search on this row
		maxCol := len(line) - qLen
		if row == from.Row {
			maxCol = from.Col - qLen
		}

		// Search backward within current line
		if maxCol >= 0 {
			for c := maxCol; c >= 0; c-- {
				matched := true
				for i := range queryRunes {
					if !runeMatches(line[c+i], queryRunes[i], ignoreCase) {
						matched = false
						break
					}
				}
				if matched {
					start = Position{Row: row, Col: c}
					end = Position{Row: row, Col: c + qLen}
					return start, end, true
				}
			}
		}

		if row > 0 {
			row--
		} else {
			row = len(b.lines) - 1
		}
	}

	return Position{}, Position{}, false
}

// ReplaceAll replaces every exact occurrence of query in the buffer.
func (b *Buffer) ReplaceAll(query, replacement string) (count int) {
	return b.replaceAll(query, replacement, false)
}

// ReplaceAllIgnoreCase replaces every case-insensitive occurrence of query.
func (b *Buffer) ReplaceAllIgnoreCase(query, replacement string) (count int) {
	return b.replaceAll(query, replacement, true)
}

func (b *Buffer) replaceAll(query, replacement string, ignoreCase bool) (count int) {
	queryRunes := []rune(query)
	if len(queryRunes) == 0 {
		return 0
	}

	for row, line := range b.lines {
		result := make([]rune, 0, len(line))
		for col := 0; col < len(line); {
			if col+len(queryRunes) <= len(line) {
				matched := true
				for i, queryRune := range queryRunes {
					if !runeMatches(line[col+i], queryRune, ignoreCase) {
						matched = false
						break
					}
				}
				if matched {
					result = append(result, []rune(replacement)...)
					count++
					col += len(queryRunes)
					continue
				}
			}
			result = append(result, line[col])
			col++
		}
		if count > 0 {
			b.lines[row] = result
		}
	}
	return count
}

func runeMatches(left, right rune, ignoreCase bool) bool {
	if ignoreCase {
		return unicode.ToLower(left) == unicode.ToLower(right)
	}
	return left == right
}

// MoveWordRight moves the cursor to the end of the current word,
// or across whitespace/punctuation to the end of the next word.
func (b *Buffer) MoveWordRight(p Position) Position {
	if p.Row >= len(b.lines) {
		return p
	}

	line := b.lines[p.Row]

	// If at or past line end, wrap to the start of the next line
	if p.Col >= len(line) {
		if p.Row+1 < len(b.lines) {
			return Position{Row: p.Row + 1, Col: 0}
		}
		return p // End of document
	}

	col := p.Col

	// Skip leading non-word characters (whitespace, punctuation)
	for col < len(line) && !isWordChar(line[col]) {
		col++
	}

	// Consume the word characters until the end of word
	for col < len(line) && isWordChar(line[col]) {
		col++
	}

	return Position{Row: p.Row, Col: col}
}

// MoveWordLeft moves the cursor to the start of the current word,
// or across whitespace/punctuation to the start of the previous word.
func (b *Buffer) MoveWordLeft(p Position) Position {
	if p.Row < 0 || p.Row >= len(b.lines) {
		return p
	}

	line := b.lines[p.Row]
	col := p.Col
	if col > len(line) {
		col = len(line)
	}

	// If at start of line, move to end of previous line
	if col == 0 {
		if p.Row == 0 {
			return Position{Row: 0, Col: 0}
		}
		prevLine := b.lines[p.Row-1]
		return Position{Row: p.Row - 1, Col: len(prevLine)}
	}

	i := col
	// skip non-word characters (whitespace/punctuation)
	for i > 0 && !isWordChar(line[i-1]) {
		i--
	}
	// skip word characters to the start of the word
	for i > 0 && isWordChar(line[i-1]) {
		i--
	}
	return Position{Row: p.Row, Col: i}
}
