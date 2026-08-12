package main

import (
	"strings"
	"testing"

	"kero"
)

func TestDisplayColumnAndRuneIndex(t *testing.T) {
	line := []rune("a\tbc")

	if got := runeIndexToDisplayColumn(line, 2); got != 4 {
		t.Fatalf("runeIndexToDisplayColumn(line, 2) = %d, want 4", got)
	}
	if got := runeIndexToDisplayColumn(line, 3); got != 5 {
		t.Fatalf("runeIndexToDisplayColumn(line, 3) = %d, want 5", got)
	}
	if got := displayColumnToRuneIndex(line, 4); got != 2 {
		t.Fatalf("displayColumnToRuneIndex(line, 4) = %d, want 2", got)
	}
	if got := displayColumnToRuneIndex(line, 5); got != 3 {
		t.Fatalf("displayColumnToRuneIndex(line, 5) = %d, want 3", got)
	}
}

func TestCheckGoSyntax(t *testing.T) {
	diagnostics := CheckGoSyntax("example.go", []byte("package main\n\nfunc main( {\n"))
	if len(diagnostics) == 0 {
		t.Fatal("CheckGoSyntax returned no diagnostics for invalid Go")
	}
	if diagnostics[0].Line != 2 {
		t.Errorf("diagnostic line = %d, want 2", diagnostics[0].Line)
	}
	if diagnostics[0].Message == "" {
		t.Error("diagnostic message is empty")
	}
	if diagnostics := CheckGoSyntax("example.txt", []byte("func main( {\n")); diagnostics != nil {
		t.Fatalf("CheckGoSyntax returned diagnostics for non-Go file: %+v", diagnostics)
	}
}

func TestParseVetDiagnostics(t *testing.T) {
	output := "vet-example.go:2:5: undefined: missing\nother.go:10:3: some vet issue\n"
	diagnostics := parseVetOutput(output)
	if len(diagnostics) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d", len(diagnostics))
	}
	if diagnostics[0].Line != 1 || diagnostics[0].Col != 4 {
		t.Fatalf("first diagnostic pos = (%d,%d), want (1,4)", diagnostics[0].Line, diagnostics[0].Col)
	}
	if !strings.Contains(diagnostics[0].Message, "undefined") {
		t.Fatalf("first diagnostic message = %q", diagnostics[0].Message)
	}
}

func TestNextDiagnostic(t *testing.T) {
	ed := &Editor{
		buf:         NewBuffer("package main\nfunc main( {\n}\nvar x = ("),
		cursor:      Position{Row: 0, Col: 0},
		diagnostics: []Diagnostic{{Line: 1, Col: 10}, {Line: 3, Col: 8}},
	}

	ed.nextDiagnostic()
	if ed.cursor != (Position{Row: 1, Col: 10}) {
		t.Fatalf("first diagnostic cursor = %+v, want (1, 10)", ed.cursor)
	}
	ed.nextDiagnostic()
	if ed.cursor != (Position{Row: 3, Col: 8}) {
		t.Fatalf("second diagnostic cursor = %+v, want (3, 8)", ed.cursor)
	}
	ed.nextDiagnostic()
	if ed.cursor != (Position{Row: 1, Col: 10}) {
		t.Fatalf("wrapped diagnostic cursor = %+v, want (1, 10)", ed.cursor)
	}
}

func TestGotoDiagnosticCommands(t *testing.T) {
	ed := &Editor{
		buf:         NewBuffer("package main\nfunc main( {\n}\nvar x = ("),
		cursor:      Position{Row: 3, Col: 20},
		diagnostics: []Diagnostic{{Line: 1, Col: 10}, {Line: 3, Col: 8}},
	}

	ed.cmdInput.Value = ">dprev"
	ed.cmdMode = true
	if err := ed.finishCmdPalette(); err != nil {
		t.Fatal(err)
	}
	if ed.cursor != (Position{Row: 3, Col: 8}) {
		t.Fatalf("prev-error cursor = %+v, want (3, 8)", ed.cursor)
	}

	ed.cmdInput.Value = ">dnext"
	ed.cmdMode = true
	if err := ed.finishCmdPalette(); err != nil {
		t.Fatal(err)
	}
	if ed.cursor != (Position{Row: 1, Col: 10}) {
		t.Fatalf("next-error cursor = %+v, want (1, 10)", ed.cursor)
	}

}

func TestEnsureCursorVisible_WithTabs(t *testing.T) {
	ed := &Editor{
		buf:    NewBuffer("\thello world"),
		cursor: Position{Row: 0, Col: 0},
	}

	// Mock context with width = 10 (marker + line number + space leaves textW = 7)
	ctx := &kero.Context{Width: 10, Height: 10}

	ed.ensureCursorVisible(ctx)
	if ed.colOffset != 0 {
		t.Fatalf("expected colOffset = 0, got %d", ed.colOffset)
	}

	// Move cursor to 'w' in "world" (rune index 7: '\t', h, e, l, l, o, ' ') -> display column 4 + 6 = 10
	ed.cursor.Col = 7
	ed.ensureCursorVisible(ctx)
	// textW = 10 - 1 - 2 = 7. cursorDisplay = 10.
	// 10 >= colOffset + 7 => colOffset = 10 - 7 + 1 = 4.
	if ed.colOffset != 4 {
		t.Fatalf("expected colOffset = 4, got %d", ed.colOffset)
	}

	// Move cursor back to index 0 ('\t', display column 0)
	ed.cursor.Col = 0
	ed.ensureCursorVisible(ctx)
	if ed.colOffset != 0 {
		t.Fatalf("expected colOffset = 0 when returning to start, got %d", ed.colOffset)
	}
}

func TestMoveUpMoveDown_WithTabs(t *testing.T) {
	ed := &Editor{
		buf:    NewBuffer("\thello\nabcdefg"),
		cursor: Position{Row: 0, Col: 1}, // on 'h' (display col 4)
	}

	ed.moveDown()
	if ed.cursor.Row != 1 {
		t.Fatalf("expected row 1, got %d", ed.cursor.Row)
	}
	// display col 4 on "abcdefg" corresponds to rune index 4 ('e')
	if ed.cursor.Col != 4 {
		t.Fatalf("expected col 4, got %d", ed.cursor.Col)
	}

	ed.moveUp()
	if ed.cursor.Row != 0 {
		t.Fatalf("expected row 0, got %d", ed.cursor.Row)
	}
	// display col 4 on "\thello" corresponds to rune index 1 ('h')
	if ed.cursor.Col != 1 {
		t.Fatalf("expected col 1, got %d", ed.cursor.Col)
	}
}

func TestTab_MultiLineSelection(t *testing.T) {
	ed := &Editor{
		buf:       NewBuffer("first line\nsecond line\nthird line"),
		selecting: true,
		selAnchor: Position{Row: 0, Col: 2},
		cursor:    Position{Row: 1, Col: 6},
	}

	ctx := &kero.Context{Width: 80, Height: 24}
	ev := kero.KeyEvent{Key: kero.KeyTab}

	err := ed.Update(ctx, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedLines := []string{
		"\tfirst line",
		"\tsecond line",
		"third line",
	}

	for i, line := range ed.buf.Lines() {
		if string(line) != expectedLines[i] {
			t.Errorf("line %d = %q, want %q", i, string(line), expectedLines[i])
		}
	}

	if ed.selAnchor.Col != 3 {
		t.Errorf("seletion start col = %d, want 3", ed.selAnchor.Col)
	}
	if ed.cursor.Col != 7 {
		t.Errorf("selection end col = %d, want 7", ed.cursor.Col)
	}
}

func TestTab_SingleLineSelection(t *testing.T) {
	ed := &Editor{
		buf:       NewBuffer("hello world"),
		selecting: true,
		selAnchor: Position{Row: 0, Col: 0},
		cursor:    Position{Row: 0, Col: 5},
	}

	ctx := &kero.Context{Width: 80, Height: 24}
	ev := kero.KeyEvent{Key: kero.KeyTab}

	err := ed.Update(ctx, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Single line selection should be deleted and replaced with a tab character
	if string(ed.buf.Line(0)) != "\t world" {
		t.Errorf("lines[0] = %q, want %q", string(ed.buf.Line(0)), "\t world")
	}
	if ed.selecting {
		t.Errorf("expected selecting to be false")
	}
}

func TestShiftTab_UnindentSelection(t *testing.T) {
	ed := &Editor{
		buf:       NewBuffer("\tfirst line\n    second line\nthird line"),
		cursor:    Position{Row: 1, Col: 7},
		selecting: true,
		selAnchor: Position{Row: 0, Col: 3},
	}

	ctx := &kero.Context{Width: 80, Height: 24}
	ev := kero.KeyEvent{Key: kero.KeyTab, Mod: kero.ModShift}

	err := ed.Update(ctx, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedLines := []string{
		"first line",
		"second line",
		"third line",
	}

	for i, line := range ed.buf.Lines() {
		if string(line) != expectedLines[i] {
			t.Errorf("line %d = %q, want %q", i, string(line), expectedLines[i])
		}
	}

	if ed.selAnchor.Col != 2 {
		t.Errorf("selStartCol = %d, want 2", ed.selAnchor.Col)
	}
	if ed.cursor.Col != 3 {
		t.Errorf("col = %d, want 3", ed.cursor.Col)
	}
}

func TestShiftTab_UnindentLineWithoutSelection(t *testing.T) {
	ed := &Editor{
		buf:    NewBuffer("\thello world"),
		cursor: Position{Row: 0, Col: 6},
	}

	ctx := &kero.Context{Width: 80, Height: 24}
	ev := kero.KeyEvent{Key: kero.KeyTab, Mod: kero.ModShift}

	err := ed.Update(ctx, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(ed.buf.Line(0)) != "hello world" {
		t.Errorf("lines[0] = %q, want %q", ed.buf.Line(0), "hello world")
	}
	if ed.cursor.Col != 5 {
		t.Errorf("col = %d, want 5", ed.cursor.Col)
	}
}

func TestStartSelectLine(t *testing.T) {
	ed := &Editor{
		buf:    NewBuffer("first line\nsecond line\nthird line"),
		cursor: Position{Row: 0, Col: 3},
	}

	// 1st Ctrl+L: selects line 0 down to line 1 col 0
	ed.selectLine()
	if !ed.selecting {
		t.Errorf("expected selecting to be true")
	}
	if ed.selAnchor.Row != 0 || ed.selAnchor.Col != 0 {
		t.Errorf("selStart = (%d, %d), want (0, 0)", ed.selAnchor.Row, ed.selAnchor.Col)
	}
	if ed.cursor.Row != 1 || ed.cursor.Col != 0 {
		t.Errorf("cursor = (%d, %d), want (1, 0)", ed.cursor.Row, ed.cursor.Col)
	}

	// 2nd Ctrl+L: extends selection to line 2 col 0
	ed.selectLine()
	if ed.cursor.Row != 2 || ed.cursor.Col != 0 {
		t.Errorf("cursor = (%d, %d), want (2, 0)", ed.cursor.Row, ed.cursor.Col)
	}

	// 3rd Ctrl+L: extends selection to line 2 end
	ed.selectLine()
	if ed.cursor.Row != 2 || ed.cursor.Col != len("third line") {
		t.Errorf("cursor = (%d, %d), want (2, %d)", ed.cursor.Row, ed.cursor.Col, len("third line"))
	}
}

func TestWordUnderCursor(t *testing.T) {
	ed := &Editor{
		buf:    NewBuffer("func (e *Editor) finishCommand() error {"),
		cursor: Position{Row: 0, Col: 20}, // on 'f' in finishCommand
	}
	start, end := ed.buf.WordBounds(ed.cursor)
	if start == end {
		t.Fatalf("wordAt(%+v, %+v) returned empty range", ed.cursor.Row, ed.cursor.Col)
	}
	word := ed.buf.GetRange(start, end)
	if word != "finishCommand" {
		t.Fatalf("wordAt(%+v, %+v) = %q, want %q", ed.cursor.Row, ed.cursor.Col, word, "finishCommand")
	}
}

func TestGotoDefinition(t *testing.T) {
	ed := &Editor{
		buf: NewBuffer(strings.Join([]string{
			"package main",
			"",
			"type Editor struct {",
			"}",
			"",
			"func (e *Editor) finishCommand() error {",
			"    return nil",
			"}",
			"",
			"func main() {",
			"    e := &Editor{}",
			"    e.finishCommand()",
			"}",
		}, "\n")),
		cursor: Position{Row: 11, Col: 7}, // line with e.finishCommand(), on finishCommand
	}

	ctx := &kero.Context{Width: 80, Height: 24}

	ev := kero.KeyEvent{Key: kero.KeyRune, Rune: 'g', Mod: kero.ModCtrl}
	if err := ed.Update(ctx, ev); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ed.cursor.Row != 5 {
		t.Errorf("ed.pos.Row = %d, want 5 (line of func (e *Editor) finishCommand)", ed.cursor.Row)
	}
}

func TestReplaceCurrentAndSkip(t *testing.T) {
	ed := &Editor{
		buf:    NewBuffer("one two one three one"),
		cursor: Position{Row: 0, Col: 0},
	}

	ed.startFind()
	ed.findInput.Value = "one"
	ed.findInput.Cursor = 3
	ed.updateFind(kero.KeyEvent{Key: kero.KeyEnter})
	ed.updateFind(kero.KeyEvent{Key: kero.KeyRune, Rune: 'r', Mod: kero.ModCtrl})
	ed.replaceInput.Value = "1"
	ed.replaceInput.Cursor = 1
	ed.updateFind(kero.KeyEvent{Key: kero.KeyEnter})

	if got := ed.buf.String(); got != "1 two one three one" {
		t.Fatalf("after replace current = %q, want %q", got, "1 two one three one")
	}
	if !ed.findMatch || ed.findMatchStart.Col != 6 {
		t.Fatalf("next match = (%+v, %+v, %v), want start at column 6", ed.findMatchStart, ed.findMatchEnd, ed.findMatch)
	}

	ed.updateFind(kero.KeyEvent{Key: kero.KeyTab})
	if !ed.findMatch {
		t.Fatal("expected skip to wrap to the first remaining match")
	}
	if ed.findMatchStart.Col != 16 {
		t.Fatalf("skipped match start = %d, want 16", ed.findMatchStart.Col)
	}
}

func TestReplaceAll(t *testing.T) {
	ed := &Editor{
		buf: NewBuffer("Cat\ncatapult\nDOG"),
	}
	ed.startFind()
	ed.findInput.Value = "cat"
	ed.findInput.Cursor = 3
	ed.updateFind(kero.KeyEvent{Key: kero.KeyRune, Rune: 'r', Mod: kero.ModCtrl})
	ed.replaceInput.Value = "fox"
	ed.replaceInput.Cursor = 3
	ctrlEnter := kero.KeyEvent{Key: kero.KeyEnter, Mod: kero.ModCtrl}
	ed.updateFind(ctrlEnter)

	if got := ed.buf.String(); got != "fox\nfoxapult\nDOG" {
		t.Fatalf("after replace all = %q, want %q", got, "fox\nfoxapult\nDOG")
	}
	if ed.message != "replaced 2 matches" {
		t.Fatalf("message = %q, want replacement count", ed.message)
	}
}
