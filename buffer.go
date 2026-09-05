package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/cansyan/ke/lsp"
	"github.com/mattn/go-runewidth"
)

// Buffer holds raw text and file metadata (Shared between views).
type Buffer struct {
	Path           string
	Lines          [][]byte
	Dirty          bool
	Mu             sync.Mutex
	LineHighlights map[int][]HighlightToken
}

func NewBuffer(path string, content []byte) *Buffer {
	return &Buffer{
		Path:  path,
		Lines: bytes.Split(content, []byte{'\n'}),
	}
}

// Insert inserts text at the given position and returns the new cursor position.
func (b *Buffer) Insert(p Position, text string) Position {
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return p
	}

	line := b.Lines[p.Row]
	if p.Col < 0 {
		p.Col = 0
	}
	if p.Col > len(line) {
		p.Col = len(line)
	}

	prefix := line[:p.Col]
	suffix := line[p.Col:]
	newLines := bytes.Split(slices.Concat(prefix, []byte(text), suffix), []byte{'\n'})

	// Single-line insertion fast path
	if len(newLines) == 1 {
		b.Lines[p.Row] = newLines[0]
		return Position{
			Row: p.Row,
			Col: p.Col + len(text),
		}
	}

	updated := make([][]byte, 0, len(b.Lines)+len(newLines)-1)
	updated = append(updated, b.Lines[:p.Row]...)
	for i := range newLines {
		updated = append(updated, newLines[i])
	}
	updated = append(updated, b.Lines[p.Row+1:]...)

	b.Lines = updated

	return Position{
		Row: p.Row + len(newLines) - 1,
		Col: len(newLines[len(newLines)-1]) - len(suffix),
	}
}

func (b *Buffer) ReplaceRange(start, end Position, newText string) Position {
	// Boundary safety checks
	if start.Row < 0 || start.Row >= len(b.Lines) {
		return start
	}
	if end.Row >= len(b.Lines) {
		end.Row = len(b.Lines) - 1
		end.Col = len(b.Lines[end.Row])
	}

	// 1. Extract prefix before startCol and suffix after endCol
	prefix := b.Lines[start.Row][:start.Col]
	suffix := b.Lines[end.Row][end.Col:]

	// 2. Split replacement text into lines
	newLines := bytes.Split(slices.Concat(prefix, []byte(newText), suffix), []byte{'\n'})

	// 3. inline replace, return early
	if start.Row == end.Row && len(newLines) == 1 {
		b.Lines[start.Row] = newLines[0]
		return Position{Row: start.Row, Col: start.Col + len([]rune(newText))}
	}

	// 4. Splice newLines into b.Lines slice, replacing range [startLine : endLine+1]
	updated := make([][]byte, 0, len(b.Lines)-(end.Row-start.Row+1)+len(newLines))
	updated = append(updated, b.Lines[:start.Row]...)
	for i := range newLines {
		updated = append(updated, newLines[i])
	}
	updated = append(updated, b.Lines[end.Row+1:]...)

	b.Lines = updated
	return Position{
		Row: start.Row + len(newLines) - 1,
		Col: len(newLines[len(newLines)-1]) - len(suffix),
	}
}

// orderPos guarantees start <= end (top-to-bottom, left-to-right)
func orderPos(p1, p2 Position) (start, end Position) {
	if p1.Row < p2.Row || (p1.Row == p2.Row && p1.Col <= p2.Col) {
		return p1, p2
	}
	return p2, p1
}

// Clamp ensures p falls within valid buffer bounds.
func (b *Buffer) Clamp(p Position) Position {
	if len(b.Lines) == 0 {
		return Position{Row: 0, Col: 0}
	}

	// Clamp Row
	row := p.Row
	if row < 0 {
		row = 0
	} else if row >= len(b.Lines) {
		row = len(b.Lines) - 1
	}

	// Clamp Col within the valid row
	lineLen := len(b.Lines[row])
	col := p.Col
	if col < 0 {
		col = 0
	} else if col > lineLen {
		col = lineLen
	}

	return Position{Row: row, Col: col}
}

// TextRange extracts the text between two positions [p1, p2).
func (b *Buffer) TextRange(p1, p2 Position) string {
	start, end := orderPos(b.Clamp(p1), b.Clamp(p2))

	if start == end {
		return ""
	}

	// Single-line range fast path
	if start.Row == end.Row {
		return string(b.Lines[start.Row][start.Col:end.Col])
	}

	// Multi-line range path
	var sb strings.Builder

	// First line fragment
	sb.Write(b.Lines[start.Row][start.Col:])
	sb.WriteByte('\n')

	// Intermediate full lines
	for l := start.Row + 1; l < end.Row; l++ {
		sb.Write(b.Lines[l])
		sb.WriteByte('\n')
	}

	// Final line fragment
	sb.Write(b.Lines[end.Row][:end.Col])

	return sb.String()
}

// Delete removes text between two positions [p1, p2) and returns the new cursor position.
func (b *Buffer) Delete(p1, p2 Position) Position {
	start, end := orderPos(b.Clamp(p1), b.Clamp(p2))
	if start == end {
		return start
	}

	// Single-line deletion
	if start.Row == end.Row {
		b.Lines[start.Row] = slices.Delete(b.Lines[start.Row], start.Col, end.Col)
		return start
	}

	// Multi-line deletion
	startLinePrefix := append([]byte(nil), b.Lines[start.Row][:start.Col]...)
	endLineSuffix := b.Lines[end.Row][end.Col:]

	// Stitch start prefix and end suffix into one merged line
	mergedLine := slices.Concat(startLinePrefix, endLineSuffix)

	b.Lines[start.Row] = mergedLine
	b.Lines = slices.Delete(b.Lines, start.Row+1, end.Row+1)
	return start
}

// WordBounds finds the start and end Positions of the word surrounding pos on its line.
func (b *Buffer) WordBounds(p Position) (start, end Position) {
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return p, p
	}

	line := b.Lines[p.Row]
	lineLen := len(line)

	if lineLen == 0 {
		return Position{Row: p.Row, Col: 0}, Position{Row: p.Row, Col: 0}
	}

	// Clamp p.Col to valid slice boundary [0, lineLen]
	col := min(max(0, p.Col), lineLen-1)

	// Determine the character classification under the target position
	targetRune, _ := utf8.DecodeRune(line[col:])
	targetIsWord := isWordChar(targetRune)

	// 1. Scan Backward to find the start of the word
	startCol := col
	for startCol > 0 {
		r, size := utf8.DecodeLastRune(line[:startCol])
		if isWordChar(r) != targetIsWord {
			break
		}
		startCol -= size
	}

	// 2. Scan Forward to find the end of the word
	endCol := col
	for endCol < lineLen {
		r, size := utf8.DecodeRune(line[endCol:])
		if isWordChar(r) != targetIsWord {
			break
		}
		endCol += size
	}

	return Position{Row: p.Row, Col: startCol}, Position{Row: p.Row, Col: endCol}
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

// findNext searches forward for the next occurrence of query starting from 'from'.
// It wraps around to the top of the buffer if no match is found below 'from'.
// The returned range [start, end) is half-open (end.Col is after the last matched byte).
func (b *Buffer) findNext(query string, from Position, ignoreCase bool) (start, end Position, ok bool) {
	if len(query) == 0 || len(b.Lines) == 0 {
		return Position{}, Position{}, false
	}

	// Prepare target pattern bytes
	queryBytes := []byte(query)
	if ignoreCase {
		queryBytes = []byte(strings.ToLower(query))
	}
	queryLen := len(queryBytes)

	// Clamp starting row
	startRow := from.Row
	if startRow < 0 {
		startRow = 0
	} else if startRow >= len(b.Lines) {
		startRow = len(b.Lines) - 1
	}

	totalLines := len(b.Lines)

	// Iterate over all lines starting from 'startRow', wrapping around to cover the whole file
	for i := range totalLines + 1 {
		row := (startRow + i) % totalLines
		line := b.Lines[row]

		// Determine byte offset to start searching within this line
		searchFromCol := 0
		var targetLine []byte
		if i == 0 {
			// First line being searched: start from 'from.Col'
			searchFromCol = min(max(from.Col, 0), len(line))
			targetLine = line[searchFromCol:]
		} else if row == startRow {
			// come back to beginning , search only before 'from.Col'
			targetLine = line[searchFromCol:from.Col]
		} else {
			targetLine = line
		}

		if ignoreCase {
			targetLine = bytes.ToLower(targetLine)
		}

		// Perform fast byte search
		matchIdx := bytes.Index(targetLine, queryBytes)
		if matchIdx != -1 {
			matchStartCol := searchFromCol + matchIdx
			matchEndCol := matchStartCol + queryLen

			// Guard against full wrap-around returning a match before 'from' on the same line
			// when 'from' is already past the match.
			if i == totalLines-1 && row == startRow && searchFromCol > 0 && matchStartCol < from.Col {
				continue
			}

			return Position{Row: row, Col: matchStartCol},
				Position{Row: row, Col: matchEndCol},
				true
		}
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

// findPrev searches backward for the occurrence of query immediately preceding 'from'.
// It wraps around to the bottom of the buffer if no match is found above 'from'.
// The returned range [start, end) is half-open (end.Col is after the last matched byte).
func (b *Buffer) findPrev(query string, from Position, ignoreCase bool) (start, end Position, ok bool) {
	if len(query) == 0 || len(b.Lines) == 0 {
		return Position{}, Position{}, false
	}

	// Prepare target pattern bytes
	queryBytes := []byte(query)
	if ignoreCase {
		queryBytes = []byte(strings.ToLower(query))
	}
	queryLen := len(queryBytes)

	totalLines := len(b.Lines)

	// Clamp starting row
	startRow := from.Row
	if startRow < 0 {
		startRow = 0
	} else if startRow >= totalLines {
		startRow = totalLines - 1
	}

	// Iterate backward through all lines starting at 'startRow', wrapping around
	for i := range totalLines + 1 {
		// Decrement row index with modulo wrapping
		row := (startRow - i + totalLines) % totalLines
		line := b.Lines[row]

		// Determine upper bound column for searching within this line
		searchFromCol := 0
		searchToCol := len(line)
		var targetLine []byte
		if i == 0 {
			// First line being searched: search only BEFORE 'from.Col'
			searchToCol = min(max(from.Col, 0), len(line))
			targetLine = line[:searchToCol]
		} else if row == startRow {
			// come back to beginning , search only AFTER from.Col
			targetLine = line[from.Col:]
			searchFromCol = from.Col
		} else {
			targetLine = line
		}

		if ignoreCase {
			targetLine = bytes.ToLower(targetLine)
		}

		// Perform fast backward byte search
		matchIdx := bytes.LastIndex(targetLine, queryBytes)
		if matchIdx != -1 {
			matchStartCol := searchFromCol + matchIdx
			matchEndCol := matchStartCol + queryLen

			return Position{Row: row, Col: matchStartCol},
				Position{Row: row, Col: matchEndCol},
				true
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

// replaceAll replaces all occurrences of query with replacement in the buffer.
// It returns the total number of substitutions made.
func (b *Buffer) replaceAll(query, replacement string, ignoreCase bool) (count int) {
	if len(query) == 0 || len(b.Lines) == 0 {
		return 0
	}

	replacementBytes := []byte(replacement)

	// Case 1: Exact Case Search (Fast Path using bytes.Count and bytes.ReplaceAll)
	if !ignoreCase {
		queryBytes := []byte(query)

		for i, line := range b.Lines {
			matches := bytes.Count(line, queryBytes)
			if matches > 0 {
				b.Lines[i] = bytes.ReplaceAll(line, queryBytes, replacementBytes)
				count += matches
			}
		}
		return count
	}

	// Case 2: Case-Insensitive Search (Using compiled Regex)
	pattern := "(?i)" + regexp.QuoteMeta(query)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return 0
	}

	for i, line := range b.Lines {
		matches := re.FindAllIndex(line, -1)
		if len(matches) > 0 {
			b.Lines[i] = re.ReplaceAll(line, replacementBytes)
			count += len(matches)
		}
	}

	return count
}

// WordEnd returns the position at the end of the current or next word starting from p.
func (b *Buffer) WordEnd(p Position) Position {
	if p.Row >= len(b.Lines) {
		return p
	}

	line := b.Lines[p.Row]

	// If at or past line end, wrap to the start of the next line
	if p.Col >= len(line) {
		if p.Row+1 < len(b.Lines) {
			return Position{Row: p.Row + 1, Col: 0}
		}
		return p // End of document
	}

	col := p.Col

	// Skip leading non-word characters (whitespace, punctuation)
	for col < len(line) {
		r, size := utf8.DecodeRune(line[col:])
		if isWordChar(r) {
			break
		}
		col += size
	}

	// Consume the word characters until the end of word
	for col < len(line) {
		r, size := utf8.DecodeRune(line[col:])
		if !isWordChar(r) {
			break
		}
		col += size
	}

	return Position{Row: p.Row, Col: col}
}

// WordStart returns the position at the start of the current or previous word starting from p.
func (b *Buffer) WordStart(p Position) Position {
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return p
	}

	line := b.Lines[p.Row]
	col := min(p.Col, len(line))

	// If at start of line, move to end of previous line
	if col == 0 {
		if p.Row == 0 {
			return Position{Row: 0, Col: 0}
		}
		prevLine := b.Lines[p.Row-1]
		return Position{Row: p.Row - 1, Col: len(prevLine)}
	}

	i := col
	// skip non-word characters (whitespace/punctuation)
	for i > 0 {
		r, size := utf8.DecodeLastRune(line[:i])
		if isWordChar(r) {
			break
		}
		i -= size
	}
	// skip word characters to the start of the word
	for i > 0 {
		r, size := utf8.DecodeLastRune(line[:i])
		if !isWordChar(r) {
			break
		}
		i -= size
	}
	return Position{Row: p.Row, Col: i}
}

// NextPos returns the Position after stepping one rune right, wrapping lines if needed.
func (b *Buffer) NextRunePos(p Position) Position {
	line := b.Lines[p.Row]
	if p.Col < len(line) {
		_, size := utf8.DecodeRune(line[p.Col:])
		return Position{Row: p.Row, Col: p.Col + size}
	}
	if p.Row < len(b.Lines)-1 {
		return Position{Row: p.Row + 1, Col: 0}
	}
	return p
}

// PrevPos returns the Position after stepping one rune left, wrapping lines if needed.
func (b *Buffer) PrevRunePos(p Position) Position {
	if p.Col > 0 {
		_, size := utf8.DecodeLastRune(b.Lines[p.Row][:p.Col])
		return Position{Row: p.Row, Col: p.Col - size}
	}
	if p.Row > 0 {
		prevRow := p.Row - 1
		return Position{Row: prevRow, Col: len(b.Lines[prevRow])}
	}
	return p
}

// LineStartNonSpace returns the Position of the first non-whitespace character
// on the line specified by p.Row.
// If the line contains only whitespace or is empty, it returns the start of the line (col 0).
func (b *Buffer) LineStartNonSpace(p Position) Position {
	// Guard against out-of-bounds row
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return p
	}

	line := b.Lines[p.Row]
	col := 0

	for col < len(line) {
		r, size := utf8.DecodeRune(line[col:])
		if !unicode.IsSpace(r) {
			return Position{Row: p.Row, Col: col}
		}
		col += size
	}

	// Line is empty or entirely whitespace
	return Position{Row: p.Row, Col: 0}
}

func (b *Buffer) LineEnd(p Position) Position {
	if p.Row < 0 || p.Row >= len(b.Lines) {
		return p
	}
	return Position{Row: p.Row, Col: len(b.Lines[p.Row])}
}

// ByteOffsetToVisualCol converts a 0-indexed byte offset within a line
// to its rendered visual display column, accounting for tab width and character cell widths.
func ByteOffsetToVisualCol(line []byte, byteOffset int, tabWidth int) int {
	col := 0
	currByte := 0

	for currByte < byteOffset && currByte < len(line) {
		r, size := utf8.DecodeRune(line[currByte:])
		if r == '\t' {
			col += tabWidth - (col % tabWidth)
		} else {
			col += runewidth.RuneWidth(r)
		}
		currByte += size
	}
	return col
}

// VisualColToByteOffset converts a visual display column
// back to the closest 0-indexed byte offset on line.
func VisualColToByteOffset(line []byte, visualCol int, tabWidth int) int {
	if len(line) == 0 || visualCol <= 0 {
		return 0
	}

	vCol := 0
	byteIdx := 0

	for byteIdx < len(line) {
		r, size := utf8.DecodeRune(line[byteIdx:])

		var runeWidth int
		if r == '\t' {
			runeWidth = tabWidth - (vCol % tabWidth)
		} else {
			runeWidth = runewidth.RuneWidth(r)
		}

		// Stop if advancing past visualCol
		if vCol+runeWidth > visualCol {
			// Snap to whichever side is closer (or return current byteIdx)
			break
		}

		vCol += runeWidth
		byteIdx += size
	}

	return byteIdx
}

// VisualCol returns the visual display column for a given row and byte offset.
// It is a thin wrapper of ByteOffsetToVisualCol for convenience.
func (b *Buffer) VisualCol(row, byteOffset, tabWidth int) int {
	if row < 0 || row >= len(b.Lines) {
		return 0
	}

	line := b.Lines[row]
	if byteOffset <= 0 {
		return 0
	}
	if byteOffset > len(line) {
		byteOffset = len(line)
	}

	return ByteOffsetToVisualCol(line, byteOffset, tabWidth)
}

// ByteCol converts a visual display column back to the closest byte offset on line row.
// It is a thin wrapper of VisualColToByteOffset for convenience.
func (b *Buffer) ByteCol(row int, visualCol int, tabWidth int) int {
	if row < 0 || row >= len(b.Lines) || visualCol <= 0 {
		return 0
	}

	line := b.Lines[row]
	return VisualColToByteOffset(line, visualCol, 4)
}

// BufferReader implements io.Reader over a Buffer.
type BufferReader struct {
	buf  *Buffer
	row  int  // Current row index
	col  int  // Current byte column index within b.Lines[row]
	inNL bool // True if currently streaming the re-inserted '\n'
}

// NewReader returns an io.Reader that streams the full content of b,
// re-inserting '\n' between lines.
func (b *Buffer) NewReader() *BufferReader {
	return &BufferReader{
		buf: b,
	}
}

// Read implements the io.Reader interface.
func (r *BufferReader) Read(p []byte) (n int, err error) {
	if r.buf == nil || r.row >= len(r.buf.Lines) {
		return 0, io.EOF
	}

	for n < len(p) && r.row < len(r.buf.Lines) {
		// 1. Handle pending inter-line newline byte
		if r.inNL {
			p[n] = '\n'
			n++
			r.inNL = false
			continue
		}

		line := r.buf.Lines[r.row]

		// 2. Copy text bytes from current line into p
		if r.col < len(line) {
			copied := copy(p[n:], line[r.col:])
			n += copied
			r.col += copied
		}

		// 3. Once line text is fully read, advance row position
		if r.col == len(line) {
			if r.row < len(r.buf.Lines)-1 {
				// Intermediate line: queue a newline and advance row
				r.inNL = true
				r.row++
				r.col = 0
			} else {
				// Final line: finished reading, do NOT emit trailing newline
				r.row++
			}
		}
	}

	if n == 0 && r.row >= len(r.buf.Lines) {
		return 0, io.EOF
	}

	return n, nil
}

// BufferFromFile opens a file and prepares a Buffer struct.
// If the file does not exist, it creates a new empty buffer associated with that path.
func BufferFromFile(path string) (*Buffer, error) {
	if path == "" {
		return &Buffer{
			Path:  "",
			Lines: [][]byte{{}},
		}, nil
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(absPath)
	if os.IsNotExist(err) {
		// New/unsaved file: initialize with one empty line
		return &Buffer{
			Path:  absPath,
			Lines: [][]byte{{}},
		}, nil
	} else if err != nil {
		return nil, err
	}
	defer file.Close()

	lines, err := readLines(file)
	if err != nil {
		return nil, err
	}

	// Guarantee at least one line exists in memory
	if len(lines) == 0 {
		lines = [][]byte{{}}
	}

	return &Buffer{
		Path:  absPath,
		Lines: lines,
	}, nil
}

// ApplyTextEdits applies a slice of LSP TextEdits to a Buffer in-memory.
func (b *Buffer) ApplyTextEdits(edits []lsp.TextEdit) {
	if len(edits) == 0 {
		return
	}

	// 1. Sort edits in reverse order (highest line/character first)
	sort.Slice(edits, func(i, j int) bool {
		r1 := edits[i].Range.Start
		r2 := edits[j].Range.Start
		if r1.Line != r2.Line {
			return r1.Line > r2.Line
		}
		return r1.Character > r2.Character
	})

	// 2. Apply each edit from bottom to top
	for _, edit := range edits {
		b.ApplyTextEdit(edit)
	}

	b.Dirty = true
}

func (b *Buffer) ApplyTextEdit(edit lsp.TextEdit) Position {
	startLine := edit.Range.Start.Line
	endLine := edit.Range.End.Line

	if startLine >= len(b.Lines) {
		return Position{}
	}

	// Convert UTF-16 character offsets to byte offsets
	startByte := lsp.CharToByteOffset(b.Lines[startLine], edit.Range.Start.Character)

	if endLine >= len(b.Lines) {
		endLine = len(b.Lines) - 1
	}
	endByte := lsp.CharToByteOffset(b.Lines[endLine], edit.Range.End.Character)

	startPos := Position{
		Row: startLine,
		Col: startByte,
	}
	endPos := Position{
		Row: endLine,
		Col: endByte,
	}

	// Delegate range replacement directly to Buffer
	return b.ReplaceRange(startPos, endPos, edit.NewText)
}
