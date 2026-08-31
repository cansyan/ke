package main

import (
	"bytes"
	"testing"

	"github.com/cansyan/kero"
)

func TestDisplayColumn(t *testing.T) {
	b := NewBuffer("", []byte("a\tbc"))
	if got := b.ByteToVisualCol(Position{Col: 2}, 4); got != 4 {
		t.Fatalf("ByteToVisualCol(Position{Row: 0, Col: 2}) = %+v, want col 4", got)
	}
	if got := b.ByteToVisualCol(Position{Col: 3}, 4); got != 5 {
		t.Fatalf("ByteToVisualCol(Position{Row: 0, Col: 3}) = %+v, want col 5", got)
	}
	if got := b.VisualToByteCol(0, 4, 4); got != 2 {
		t.Fatalf("VisualToByteCol(Position{Col: 4}) = %+v, want col 2", got)
	}
	if got := b.VisualToByteCol(0, 5, 4); got != 3 {
		t.Fatalf("VisualToByteCol(Position{Col: 5}) = %+v, want col 3", got)
	}
}

func TestCheckGoSemantics(t *testing.T) {
	diagnostics := CheckSemantics("example.go", []byte("package main\n\nfunc main( {\n"))
	if len(diagnostics) == 0 {
		t.Fatal("CheckSemantics returned no diagnostic for invalid Go")
	}
	if diagnostics[0].Pos.Line != 3 {
		t.Errorf("vet line = %d, want 3", diagnostics[0].Pos.Line)
	}
	if diagnostics[0].Msg == "" {
		t.Error("vet message is empty")
	}
}

func TestEnsureCursorVisible_WithTabs(t *testing.T) {
	buf := NewBuffer("", []byte("\thello world"))
	// Mock width = 10 (marker + line number + space leaves textW = 7)
	v := &View{Buf: buf, Width: 10, Height: 10, Cursor: Position{Row: 0, Col: 0}}

	v.showCursorCenter()
	if v.ScrollCol != 0 {
		t.Fatalf("expected colOffset = 0, got %d", v.ScrollCol)
	}

	// Move cursor to 'w' in "world" (rune index 7: '\t', h, e, l, l, o, ' ') -> display column 4 + 6 = 10
	v.Cursor.Col = 7
	v.showCursorCenter()
	if v.ScrollCol != 5 {
		t.Fatalf("expected colOffset = 4, got %d", v.ScrollCol)
	}

	// Move cursor back to index 0 ('\t', display column 0)
	v.Cursor.Col = 0
	v.showCursorCenter()
	if v.ScrollCol != 0 {
		t.Fatalf("expected colOffset = 0 when returning to start, got %d", v.ScrollCol)
	}
}

func TestMoveUpMoveDown_WithTabs(t *testing.T) {
	buf := NewBuffer("", []byte("\thello\nabcdefg"))
	v := &View{
		Buf:    buf,
		Cursor: Position{Row: 0, Col: 1}, // on 'h' (display col 4)
	}

	v.moveDown()
	if v.Cursor.Row != 1 {
		t.Fatalf("expected row 1, got %d", v.Cursor.Row)
	}
	// display col 4 on "abcdefg" corresponds to rune index 4 ('e')
	if v.Cursor.Col != 4 {
		t.Fatalf("expected col 4, got %d", v.Cursor.Col)
	}

	v.moveUp()
	if v.Cursor.Row != 0 {
		t.Fatalf("expected row 0, got %d", v.Cursor.Row)
	}
	// display col 4 on "\thello" corresponds to rune index 1 ('h')
	if v.Cursor.Col != 1 {
		t.Fatalf("expected col 1, got %d", v.Cursor.Col)
	}
}

func TestTab_MultiLineSelection(t *testing.T) {
	buf := NewBuffer("", []byte("first line\nsecond line\nthird line"))
	v := &View{
		Buf:       buf,
		Selecting: true,
		SelAnchor: Position{Row: 0, Col: 2},
		Cursor:    Position{Row: 1, Col: 6},
	}
	e := &Editor{views: []*View{v}}

	ctx := &kero.Context{Width: 80, Height: 24}
	ev := kero.KeyEvent{Key: kero.KeyTab}

	err := e.Update(ctx, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedLines := []string{
		"\tfirst line",
		"\tsecond line",
		"third line",
	}

	for i := range len(v.Buf.Lines) {
		if line := string(v.Buf.Lines[i]); line != expectedLines[i] {
			t.Errorf("line %d = %q, want %q", i, line, expectedLines[i])
		}
	}

	if v.SelAnchor.Col != 3 {
		t.Errorf("seletion start col = %d, want 3", v.SelAnchor.Col)
	}
	if v.Cursor.Col != 7 {
		t.Errorf("selection end col = %d, want 7", v.Cursor.Col)
	}
}

func TestTab_SingleLineSelection(t *testing.T) {
	buf := NewBuffer("", []byte("hello world"))
	v := &View{Buf: buf}
	v.Selecting = true
	v.SelAnchor = Position{Row: 0, Col: 0}
	v.Cursor = Position{Row: 0, Col: 5}
	ed := &Editor{
		views: []*View{v},
	}

	ctx := &kero.Context{Width: 80, Height: 24}
	ev := kero.KeyEvent{Key: kero.KeyTab}

	err := ed.Update(ctx, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Single line selection should be deleted and replaced with a tab character
	if string(v.Buf.Lines[0]) != "\t world" {
		t.Errorf("lines[0] = %q, want %q", string(v.Buf.Lines[0]), "\t world")
	}
	if v.Selecting {
		t.Errorf("expected selecting to be false")
	}
}

func TestShiftTab_UnindentSelection(t *testing.T) {
	buf := NewBuffer("", []byte("\tfirst line\n    second line\nthird line"))
	v := &View{Buf: buf}
	v.Cursor = Position{Row: 1, Col: 7}
	v.Selecting = true
	v.SelAnchor = Position{Row: 0, Col: 3}
	ed := &Editor{
		views: []*View{v},
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

	for i := range len(v.Buf.Lines) {
		if line := string(v.Buf.Lines[i]); line != expectedLines[i] {
			t.Errorf("line %d = %q, want %q", i, line, expectedLines[i])
		}
	}

	if v.SelAnchor.Col != 2 {
		t.Errorf("selStartCol = %d, want 2", v.SelAnchor.Col)
	}
	if v.Cursor.Col != 3 {
		t.Errorf("col = %d, want 3", v.Cursor.Col)
	}
}

func TestShiftTab_UnindentLineWithoutSelection(t *testing.T) {
	buf := NewBuffer("", []byte("\thello world"))
	v := &View{Buf: buf}
	v.Cursor = Position{Row: 0, Col: 6}
	ed := &Editor{
		views: []*View{v},
	}

	ctx := &kero.Context{Width: 80, Height: 24}
	ev := kero.KeyEvent{Key: kero.KeyTab, Mod: kero.ModShift}

	err := ed.Update(ctx, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(v.Buf.Lines[0]) != "hello world" {
		t.Errorf("lines[0] = %q, want %q", v.Buf.Lines[0], "hello world")
	}
	if v.Cursor.Col != 5 {
		t.Errorf("col = %d, want 5", v.Cursor.Col)
	}
}

func TestStartSelectLine(t *testing.T) {
	buf := NewBuffer("", []byte("first line\nsecond line\nthird line"))
	v := &View{Buf: buf}
	v.Cursor = Position{Row: 0, Col: 3}
	ed := &Editor{
		views: []*View{v},
	}

	// 1st Ctrl+L: selects line 0 down to line 1 col 0
	ed.selectLine()
	if !v.Selecting {
		t.Errorf("expected selecting to be true")
	}
	if v.SelAnchor.Row != 0 || v.SelAnchor.Col != 0 {
		t.Errorf("selStart = (%d, %d), want (0, 0)", v.SelAnchor.Row, v.SelAnchor.Col)
	}
	if v.Cursor.Row != 1 || v.Cursor.Col != 0 {
		t.Errorf("cursor = (%d, %d), want (1, 0)", v.Cursor.Row, v.Cursor.Col)
	}

	// 2nd Ctrl+L: extends selection to line 2 col 0
	ed.selectLine()
	if v.Cursor.Row != 2 || v.Cursor.Col != 0 {
		t.Errorf("cursor = (%d, %d), want (2, 0)", v.Cursor.Row, v.Cursor.Col)
	}

	// 3rd Ctrl+L: extends selection to line 2 end
	ed.selectLine()
	if v.Cursor.Row != 2 || v.Cursor.Col != len("third line") {
		t.Errorf("cursor = (%d, %d), want (2, %d)", v.Cursor.Row, v.Cursor.Col, len("third line"))
	}
}

func TestWordUnderCursor(t *testing.T) {
	buf := NewBuffer("", []byte("func (e *Editor) finishCommand() error {"))
	v := &View{Buf: buf}
	v.Cursor = Position{Row: 0, Col: 20} // on 'f' in finishCommand

	start, end := v.Buf.WordBounds(v.Cursor)
	if start == end {
		t.Fatalf("wordAt(%+v, %+v) returned empty range", v.Cursor.Row, v.Cursor.Col)
	}
	word := v.Buf.TextRange(start, end)
	if word != "finishCommand" {
		t.Fatalf("wordAt(%+v, %+v) = %q, want %q", v.Cursor.Row, v.Cursor.Col, word, "finishCommand")
	}
}

func TestReplaceCurrentAndSkip(t *testing.T) {
	buf := NewBuffer("", []byte("one two one three one"))
	v := &View{Buf: buf}
	v.Cursor = Position{Row: 0, Col: 0}
	ed := &Editor{
		views: []*View{v},
	}

	ed.startFind()
	ed.findInput.SetText("one")
	ed.updateFind(kero.KeyEvent{Key: kero.KeyEnter})
	ed.updateFind(kero.KeyEvent{Key: kero.KeyRune, Rune: 'r', Mod: kero.ModCtrl})
	ed.replaceInput.SetText("1")
	ed.updateFind(kero.KeyEvent{Key: kero.KeyEnter})

	if got := string(v.Buf.Lines[0]); got != "1 two one three one" {
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
		views: []*View{
			{Buf: NewBuffer("", []byte("Cat\ncatapult\nDOG"))},
		},
	}
	ed.startFind()
	ed.findInput.SetText("cat")
	ed.updateFind(kero.KeyEvent{Key: kero.KeyRune, Rune: 'r', Mod: kero.ModCtrl})
	ed.replaceInput.SetText("fox")
	ctrlEnter := kero.KeyEvent{Key: kero.KeyEnter, Mod: kero.ModCtrl}
	ed.updateFind(ctrlEnter)

	if got := string(bytes.Join(ed.Buf().Lines, []byte{'\n'})); got != "fox\nfoxapult\nDOG" {
		t.Fatalf("after replace all = %q, want %q", got, "fox\nfoxapult\nDOG")
	}
	if ed.message != "replaced 2 matches" {
		t.Fatalf("message = %q, want replacement count", ed.message)
	}
}
