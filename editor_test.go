package main

import (
	"strings"
	"testing"

	"github.com/cansyan/kero"
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

func TestCheckGoSemantics(t *testing.T) {
	diagnostics := CheckSemantics("example.go", []byte("package main\n\nfunc main( {\n"))
	if len(diagnostics) == 0 {
		t.Fatal("CheckSemantics returned no diagnostic for invalid Go")
	}
	if diagnostics[0].Row != 2 {
		t.Errorf("vet line = %d, want 2", diagnostics[0].Row)
	}
	if diagnostics[0].Message == "" {
		t.Error("vet message is empty")
	}
}

func TestEnsureCursorVisible_WithTabs(t *testing.T) {
	buf := NewBuffer("\thello world")
	buf.Cursor = Position{Row: 0, Col: 0}
	ed := &Editor{
		Buffer: buf,
	}

	// Mock context with width = 10 (marker + line number + space leaves textW = 7)
	ctx := &kero.Context{Width: 10, Height: 10}

	ed.showCursorCenter(ctx)
	if ed.LeftCol != 0 {
		t.Fatalf("expected colOffset = 0, got %d", ed.LeftCol)
	}

	// Move cursor to 'w' in "world" (rune index 7: '\t', h, e, l, l, o, ' ') -> display column 4 + 6 = 10
	ed.Cursor.Col = 7
	ed.showCursorCenter(ctx)
	// textW = 10 - 1 - 2 = 7. cursorDisplay = 10.
	// 10 >= colOffset + 7 => colOffset = 10 - 7 + 1 = 4.
	if ed.LeftCol != 4 {
		t.Fatalf("expected colOffset = 4, got %d", ed.LeftCol)
	}

	// Move cursor back to index 0 ('\t', display column 0)
	ed.Cursor.Col = 0
	ed.showCursorCenter(ctx)
	if ed.LeftCol != 0 {
		t.Fatalf("expected colOffset = 0 when returning to start, got %d", ed.LeftCol)
	}
}

func TestMoveUpMoveDown_WithTabs(t *testing.T) {
	buf := NewBuffer("\thello\nabcdefg")
	buf.Cursor = Position{Row: 0, Col: 1} // on 'h' (display col 4)
	ed := &Editor{
		Buffer: buf,
	}

	ed.moveDown()
	if ed.Cursor.Row != 1 {
		t.Fatalf("expected row 1, got %d", ed.Cursor.Row)
	}
	// display col 4 on "abcdefg" corresponds to rune index 4 ('e')
	if ed.Cursor.Col != 4 {
		t.Fatalf("expected col 4, got %d", ed.Cursor.Col)
	}

	ed.moveUp()
	if ed.Cursor.Row != 0 {
		t.Fatalf("expected row 0, got %d", ed.Cursor.Row)
	}
	// display col 4 on "\thello" corresponds to rune index 1 ('h')
	if ed.Cursor.Col != 1 {
		t.Fatalf("expected col 1, got %d", ed.Cursor.Col)
	}
}

func TestTab_MultiLineSelection(t *testing.T) {
	buf := NewBuffer("first line\nsecond line\nthird line")
	buf.Selecting = true
	buf.SelAnchor = Position{Row: 0, Col: 2}
	buf.Cursor = Position{Row: 1, Col: 6}
	ed := &Editor{
		Buffer: buf,
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

	for i := range len(ed.Buffer.Lines) {
		if line := string(ed.Buffer.Line(i)); line != expectedLines[i] {
			t.Errorf("line %d = %q, want %q", i, line, expectedLines[i])
		}
	}

	if ed.SelAnchor.Col != 3 {
		t.Errorf("seletion start col = %d, want 3", ed.SelAnchor.Col)
	}
	if ed.Cursor.Col != 7 {
		t.Errorf("selection end col = %d, want 7", ed.Cursor.Col)
	}
}

func TestTab_SingleLineSelection(t *testing.T) {
	buf := NewBuffer("hello world")
	buf.Selecting = true
	buf.SelAnchor = Position{Row: 0, Col: 0}
	buf.Cursor = Position{Row: 0, Col: 5}
	ed := &Editor{
		Buffer: buf,
	}

	ctx := &kero.Context{Width: 80, Height: 24}
	ev := kero.KeyEvent{Key: kero.KeyTab}

	err := ed.Update(ctx, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Single line selection should be deleted and replaced with a tab character
	if string(ed.Buffer.Line(0)) != "\t world" {
		t.Errorf("lines[0] = %q, want %q", string(ed.Buffer.Line(0)), "\t world")
	}
	if ed.Selecting {
		t.Errorf("expected selecting to be false")
	}
}

func TestShiftTab_UnindentSelection(t *testing.T) {
	buf := NewBuffer("\tfirst line\n    second line\nthird line")
	buf.Cursor = Position{Row: 1, Col: 7}
	buf.Selecting = true
	buf.SelAnchor = Position{Row: 0, Col: 3}
	ed := &Editor{
		Buffer: buf,
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

	for i := range len(ed.Buffer.Lines) {
		if line := string(ed.Buffer.Line(i)); line != expectedLines[i] {
			t.Errorf("line %d = %q, want %q", i, line, expectedLines[i])
		}
	}

	if ed.SelAnchor.Col != 2 {
		t.Errorf("selStartCol = %d, want 2", ed.SelAnchor.Col)
	}
	if ed.Cursor.Col != 3 {
		t.Errorf("col = %d, want 3", ed.Cursor.Col)
	}
}

func TestShiftTab_UnindentLineWithoutSelection(t *testing.T) {
	buf := NewBuffer("\thello world")
	buf.Cursor = Position{Row: 0, Col: 6}
	ed := &Editor{
		Buffer: buf,
	}

	ctx := &kero.Context{Width: 80, Height: 24}
	ev := kero.KeyEvent{Key: kero.KeyTab, Mod: kero.ModShift}

	err := ed.Update(ctx, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(ed.Buffer.Line(0)) != "hello world" {
		t.Errorf("lines[0] = %q, want %q", ed.Buffer.Line(0), "hello world")
	}
	if ed.Cursor.Col != 5 {
		t.Errorf("col = %d, want 5", ed.Cursor.Col)
	}
}

func TestStartSelectLine(t *testing.T) {
	buf := NewBuffer("first line\nsecond line\nthird line")
	buf.Cursor = Position{Row: 0, Col: 3}
	ed := &Editor{
		Buffer: buf,
	}

	// 1st Ctrl+L: selects line 0 down to line 1 col 0
	ed.selectLine()
	if !ed.Selecting {
		t.Errorf("expected selecting to be true")
	}
	if ed.SelAnchor.Row != 0 || ed.SelAnchor.Col != 0 {
		t.Errorf("selStart = (%d, %d), want (0, 0)", ed.SelAnchor.Row, ed.SelAnchor.Col)
	}
	if ed.Cursor.Row != 1 || ed.Cursor.Col != 0 {
		t.Errorf("cursor = (%d, %d), want (1, 0)", ed.Cursor.Row, ed.Cursor.Col)
	}

	// 2nd Ctrl+L: extends selection to line 2 col 0
	ed.selectLine()
	if ed.Cursor.Row != 2 || ed.Cursor.Col != 0 {
		t.Errorf("cursor = (%d, %d), want (2, 0)", ed.Cursor.Row, ed.Cursor.Col)
	}

	// 3rd Ctrl+L: extends selection to line 2 end
	ed.selectLine()
	if ed.Cursor.Row != 2 || ed.Cursor.Col != len("third line") {
		t.Errorf("cursor = (%d, %d), want (2, %d)", ed.Cursor.Row, ed.Cursor.Col, len("third line"))
	}
}

func TestWordUnderCursor(t *testing.T) {
	buf := NewBuffer("func (e *Editor) finishCommand() error {")
	buf.Cursor = Position{Row: 0, Col: 20} // on 'f' in finishCommand
	ed := &Editor{
		Buffer: buf,
	}
	start, end := ed.Buffer.WordBounds(ed.Cursor)
	if start == end {
		t.Fatalf("wordAt(%+v, %+v) returned empty range", ed.Cursor.Row, ed.Cursor.Col)
	}
	word := ed.Buffer.GetRange(start, end)
	if word != "finishCommand" {
		t.Fatalf("wordAt(%+v, %+v) = %q, want %q", ed.Cursor.Row, ed.Cursor.Col, word, "finishCommand")
	}
}

func TestGotoDefinition(t *testing.T) {
	buf := NewBuffer(strings.Join([]string{
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
	}, "\n"))
	buf.Cursor = Position{Row: 11, Col: 7} // line with e.finishCommand(), on finishCommand
	ed := &Editor{
		Buffer: buf,
	}

	ctx := &kero.Context{Width: 80, Height: 24}

	ev := kero.KeyEvent{Key: kero.KeyRune, Rune: 'g', Mod: kero.ModCtrl}
	if err := ed.Update(ctx, ev); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ed.Cursor.Row != 5 {
		t.Errorf("ed.pos.Row = %d, want 5 (line of func (e *Editor) finishCommand)", ed.Cursor.Row)
	}
}

func TestReplaceCurrentAndSkip(t *testing.T) {
	buf := NewBuffer("one two one three one")
	buf.Cursor = Position{Row: 0, Col: 0}
	ed := &Editor{
		Buffer: buf,
	}

	ed.startFind()
	ed.findInput.Value = "one"
	ed.findInput.Cursor = 3
	ed.updateFind(kero.KeyEvent{Key: kero.KeyEnter})
	ed.updateFind(kero.KeyEvent{Key: kero.KeyRune, Rune: 'r', Mod: kero.ModCtrl})
	ed.replaceInput.Value = "1"
	ed.replaceInput.Cursor = 1
	ed.updateFind(kero.KeyEvent{Key: kero.KeyEnter})

	if got := ed.Buffer.String(); got != "1 two one three one" {
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
		Buffer: NewBuffer("Cat\ncatapult\nDOG"),
	}
	ed.startFind()
	ed.findInput.Value = "cat"
	ed.findInput.Cursor = 3
	ed.updateFind(kero.KeyEvent{Key: kero.KeyRune, Rune: 'r', Mod: kero.ModCtrl})
	ed.replaceInput.Value = "fox"
	ed.replaceInput.Cursor = 3
	ctrlEnter := kero.KeyEvent{Key: kero.KeyEnter, Mod: kero.ModCtrl}
	ed.updateFind(ctrlEnter)

	if got := ed.Buffer.String(); got != "fox\nfoxapult\nDOG" {
		t.Fatalf("after replace all = %q, want %q", got, "fox\nfoxapult\nDOG")
	}
	if ed.message != "replaced 2 matches" {
		t.Fatalf("message = %q, want replacement count", ed.message)
	}
}
