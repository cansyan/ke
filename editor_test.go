package main

import (
	"bytes"
	"testing"

	"github.com/cansyan/ke/lsp"
	"github.com/cansyan/kero"
)

func TestDisplayColumn(t *testing.T) {
	line := []byte("a\tbc")
	if got := ByteOffsetToVisualCol(line, 2, 4); got != 4 {
		t.Fatalf("ByteOffsetToVisualCol = %+v, want col 4", got)
	}
	if got := ByteOffsetToVisualCol(line, 3, 4); got != 5 {
		t.Fatalf("ByteOffsetToVisualCol = %+v, want col 5", got)
	}
	if got := VisualColToByteOffset(line, 4, 4); got != 2 {
		t.Fatalf("VisualColToByteOffset = %+v, want col 2", got)
	}
	if got := VisualColToByteOffset(line, 5, 4); got != 3 {
		t.Fatalf("VisualColToByteOffset = %+v, want col 3", got)
	}
}

func TestEnsureCursorVisible_WithTabs(t *testing.T) {
	buf := NewBuffer("", []byte("\thello world"))
	// Mock total width = 10, marker + line number + space leaves view.Width = 7
	v := &View{Buf: buf, Width: 7, Height: 10, Cursor: Position{Row: 0, Col: 0}}

	v.showCursorCenter()
	if v.ScrollCol != 0 {
		t.Fatalf("expected colOffset = 0, got %d", v.ScrollCol)
	}

	// Move cursor to 'w' in "world" (byte offset 7: '\t', h, e, l, l, o, ' ')
	v.Cursor.Col = 7
	// display column 4 + 6 = 10
	vCol := v.Buf.VisualCol(v.Cursor.Row, v.Cursor.Col, 4)
	v.showCursorCenter()
	if v.ScrollCol != vCol-v.Width+1 {
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

func TestEnter_AutoIndentAfterBlockOpen(t *testing.T) {
	buf := NewBuffer("", []byte("    {"))
	v := &View{Buf: buf}
	v.Cursor = Position{Row: 0, Col: len("    {")}
	ed := &Editor{views: []*View{v}}

	ctx := &kero.Context{Width: 80, Height: 24}
	err := ed.Update(ctx, kero.KeyEvent{Key: kero.KeyEnter})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := string(bytes.Join(v.Buf.Lines, []byte{'\n'}))
	want := "    {\n    \t"
	if got != want {
		t.Fatalf("after enter = %q, want %q", got, want)
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
	ed.find.Input.SetText("one")
	ed.updateFind(kero.KeyEvent{Key: kero.KeyEnter})
	ed.updateFind(kero.KeyEvent{Key: kero.KeyRune, Rune: 'r', Mod: kero.ModCtrl})
	ed.find.ReplaceInput.SetText("1")
	ed.updateFind(kero.KeyEvent{Key: kero.KeyEnter})

	if got := string(v.Buf.Lines[0]); got != "1 two one three one" {
		t.Fatalf("after replace current = %q, want %q", got, "1 two one three one")
	}
	if !ed.find.Match || ed.find.MatchStart.Col != 6 {
		t.Fatalf("next match = (%+v, %+v, %v), want start at column 6", ed.find.MatchStart, ed.find.MatchEnd, ed.find.Match)
	}

	ed.updateFind(kero.KeyEvent{Key: kero.KeyTab})
	if !ed.find.Match {
		t.Fatal("expected skip to wrap to the first remaining match")
	}
	if ed.find.MatchStart.Col != 16 {
		t.Fatalf("skipped match start = %d, want 16", ed.find.MatchStart.Col)
	}
}

func TestReplaceAll(t *testing.T) {
	ed := &Editor{
		views: []*View{
			{Buf: NewBuffer("", []byte("Cat\ncatapult\nDOG"))},
		},
	}
	ed.startFind()
	ed.find.Input.SetText("cat")
	ed.updateFind(kero.KeyEvent{Key: kero.KeyRune, Rune: 'r', Mod: kero.ModCtrl})
	ed.find.ReplaceInput.SetText("fox")
	ctrlEnter := kero.KeyEvent{Key: kero.KeyEnter, Mod: kero.ModCtrl}
	ed.updateFind(ctrlEnter)

	if got := string(bytes.Join(ed.Buf().Lines, []byte{'\n'})); got != "fox\nfoxapult\nDOG" {
		t.Fatalf("after replace all = %q, want %q", got, "fox\nfoxapult\nDOG")
	}
	if ed.message != "replaced 2 matches" {
		t.Fatalf("message = %q, want replacement count", ed.message)
	}
}

func TestDrawCompletion_SmartPosition(t *testing.T) {
	lines := make([]byte, 0)
	for i := range 30 {
		lines = append(lines, []byte("line "+string(rune('0'+i))+"\n")...)
	}
	buf := NewBuffer("", lines)
	v := &View{Buf: buf, Cursor: Position{Row: 0, Col: 0}}
	ed := &Editor{
		views: []*View{v},
		completion: Completion{
			Active: true,
			Items: []lsp.CompletionItem{
				{Label: "item1"},
				{Label: "item2"},
				{Label: "item3"},
			},
		},
	}

	// 1. Cursor at top (Row 0): should draw below cursor (rows 1, 2, 3)
	fTop := kero.NewFrame(80, 24)
	ed.drawCompletion(&fTop, kero.Style{})

	// Verify that cell at row 1, gutterWidth has completion content (not empty)
	// incicator is " > ", "gutterWidth(len(buf.Lines))-2" should be the x of arrow
	cellRow1 := fTop.Cell(gutterWidth(len(buf.Lines))-2, 1)
	if cellRow1.Ch != ' ' && cellRow1.Ch != '>' {
		t.Fatalf("expected completion item rendered at row 1, got rune %q", cellRow1.Ch)
	}

	// 2. Cursor lower down (Row 10): space above = 10 >= visibleRows 3, should draw above cursor (rows 7, 8, 9)
	v.Cursor.Row = 10
	fAbove := kero.NewFrame(80, 24)
	ed.drawCompletion(&fAbove, kero.Style{})

	cellRow7 := fAbove.Cell(gutterWidth(len(buf.Lines))-2, 7)
	if cellRow7.Ch != ' ' && cellRow7.Ch != '>' {
		t.Fatalf("expected completion item rendered at row 7, got rune %q", cellRow7.Ch)
	}
}

func TestContextMenu(t *testing.T) {
	buf := NewBuffer("test.go", []byte("hello world"))
	v := &View{Buf: buf, Width: 80, Height: 24, Cursor: Position{Row: 0, Col: 0}}
	v.Selecting = true
	v.SelAnchor = Position{Row: 0, Col: 0}
	v.Cursor = Position{Row: 0, Col: 5} // selected "hello"
	ed := &Editor{views: []*View{v}}

	ctx := &kero.Context{Width: 80, Height: 24}

	// 1. Right click to open menu
	mRight := kero.MouseEvent{Button: kero.MouseRight, Action: kero.MousePress, X: 10, Y: 5}
	if err := ed.Update(ctx, mRight); err != nil {
		t.Fatalf("unexpected error on right click: %v", err)
	}

	if !ed.menu.Active {
		t.Fatalf("expected menu to be active after right click")
	}

	expectedItems := []string{"Copy", "Paste", "Definition", "References", "Rename"}
	if len(ed.menu.Items) != len(expectedItems) {
		t.Fatalf("expected %d items, got %d", len(expectedItems), len(ed.menu.Items))
	}
	for i, item := range ed.menu.Items {
		if item != expectedItems[i] {
			t.Errorf("item %d = %q, want %q", i, item, expectedItems[i])
		}
	}

	// 2. Click inside menu on option "copy" (index 0 at Y=5)
	mCopy := kero.MouseEvent{Button: kero.MouseLeft, Action: kero.MousePress, X: 11, Y: 5}
	if err := ed.Update(ctx, mCopy); err != nil {
		t.Fatalf("unexpected error on copy click: %v", err)
	}
	if ed.menu.Active {
		t.Fatalf("expected menu to be hidden after selecting an item")
	}
	if ed.clipboard != "hello" {
		t.Fatalf("expected clipboard to be %q, got %q", "hello", ed.clipboard)
	}

	// 3. Right click to open menu again
	if err := ed.Update(ctx, mRight); err != nil {
		t.Fatalf("unexpected error on right click: %v", err)
	}
	if !ed.menu.Active {
		t.Fatalf("expected menu active")
	}

	// 4. Click outside menu (e.g. X=0, Y=0) should hide menu
	mOutside := kero.MouseEvent{Button: kero.MouseLeft, Action: kero.MousePress, X: 0, Y: 0}
	if err := ed.Update(ctx, mOutside); err != nil {
		t.Fatalf("unexpected error on outside click: %v", err)
	}
	if ed.menu.Active {
		t.Fatalf("expected menu to be hidden after clicking outside")
	}

	// 5. Test KeyEsc dismissal
	if err := ed.Update(ctx, mRight); err != nil {
		t.Fatalf("unexpected error on right click: %v", err)
	}
	if !ed.menu.Active {
		t.Fatalf("expected menu to be active")
	}
	if err := ed.Update(ctx, kero.KeyEvent{Key: kero.KeyEsc}); err != nil {
		t.Fatalf("unexpected error on KeyEsc: %v", err)
	}
	if ed.menu.Active {
		t.Fatalf("expected menu to be hidden after KeyEsc")
	}
}
