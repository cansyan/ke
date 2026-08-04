package main

import (
	"testing"

	"kero"
)

func TestDisplayColumnAndRuneIndex(t *testing.T) {
	line := "a\tbc"

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

func TestEnsureCursorVisible_WithTabs(t *testing.T) {
	ed := &Editor{
		lines: []string{"\thello world"},
		row:   0,
		col:   0,
	}

	// Mock context with width = 10 (line number width = 1, space = 1, textW = 8)
	ctx := &kero.Context{Width: 10, Height: 10}

	ed.ensureCursorVisible(ctx)
	if ed.colOffset != 0 {
		t.Fatalf("expected colOffset = 0, got %d", ed.colOffset)
	}

	// Move cursor to 'w' in "world" (rune index 7: '\t', h, e, l, l, o, ' ') -> display column 4 + 6 = 10
	ed.col = 7
	ed.ensureCursorVisible(ctx)
	// textW = 10 - 1 - 1 = 8. cursorDisplay = 10.
	// 10 >= colOffset + 8 => colOffset = 10 - 8 + 1 = 3.
	if ed.colOffset != 3 {
		t.Fatalf("expected colOffset = 3, got %d", ed.colOffset)
	}

	// Move cursor back to index 0 ('\t', display column 0)
	ed.col = 0
	ed.ensureCursorVisible(ctx)
	if ed.colOffset != 0 {
		t.Fatalf("expected colOffset = 0 when returning to start, got %d", ed.colOffset)
	}
}

func TestMoveUpMoveDown_WithTabs(t *testing.T) {
	ed := &Editor{
		lines: []string{
			"\thello", // tab is 4 spaces (cols 0-3), 'h' at col 4
			"abcdefg", // 'e' is at col 4 (rune index 4)
		},
		row: 0,
		col: 1, // on 'h' (display col 4)
	}

	ed.moveDown()
	if ed.row != 1 {
		t.Fatalf("expected row 1, got %d", ed.row)
	}
	// display col 4 on "abcdefg" corresponds to rune index 4 ('e')
	if ed.col != 4 {
		t.Fatalf("expected col 4, got %d", ed.col)
	}

	ed.moveUp()
	if ed.row != 0 {
		t.Fatalf("expected row 0, got %d", ed.row)
	}
	// display col 4 on "\thello" corresponds to rune index 1 ('h')
	if ed.col != 1 {
		t.Fatalf("expected col 1, got %d", ed.col)
	}
}

func TestTab_MultiLineSelection(t *testing.T) {
	ed := &Editor{
		lines: []string{
			"first line",
			"second line",
			"third line",
		},
		row:         1,
		col:         6,
		selecting:   true,
		selStartRow: 0,
		selStartCol: 2,
		selEndRow:   1,
		selEndCol:   6,
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

	for i, line := range ed.lines {
		if line != expectedLines[i] {
			t.Errorf("line %d = %q, want %q", i, line, expectedLines[i])
		}
	}

	if ed.selStartCol != 3 {
		t.Errorf("selStartCol = %d, want 3", ed.selStartCol)
	}
	if ed.selEndCol != 7 {
		t.Errorf("selEndCol = %d, want 7", ed.selEndCol)
	}
	if ed.col != 7 {
		t.Errorf("col = %d, want 7", ed.col)
	}
}

func TestTab_SingleLineSelection(t *testing.T) {
	ed := &Editor{
		lines: []string{
			"hello world",
		},
		row:         0,
		col:         5,
		selecting:   true,
		selStartRow: 0,
		selStartCol: 0,
		selEndRow:   0,
		selEndCol:   5,
	}

	ctx := &kero.Context{Width: 80, Height: 24}
	ev := kero.KeyEvent{Key: kero.KeyTab}

	err := ed.Update(ctx, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Single line selection should be deleted and replaced with a tab character
	if ed.lines[0] != "\t world" {
		t.Errorf("lines[0] = %q, want %q", ed.lines[0], "\t world")
	}
	if ed.selecting {
		t.Errorf("expected selecting to be false")
	}
}

func TestShiftTab_UnindentSelection(t *testing.T) {
	ed := &Editor{
		lines: []string{
			"\tfirst line",
			"    second line",
			"third line",
		},
		row:         1,
		col:         7,
		selecting:   true,
		selStartRow: 0,
		selStartCol: 3,
		selEndRow:   1,
		selEndCol:   7,
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

	for i, line := range ed.lines {
		if line != expectedLines[i] {
			t.Errorf("line %d = %q, want %q", i, line, expectedLines[i])
		}
	}

	if ed.selStartCol != 2 {
		t.Errorf("selStartCol = %d, want 2", ed.selStartCol)
	}
	if ed.selEndCol != 3 {
		t.Errorf("selEndCol = %d, want 3", ed.selEndCol)
	}
	if ed.col != 3 {
		t.Errorf("col = %d, want 3", ed.col)
	}
}

func TestShiftTab_UnindentLineWithoutSelection(t *testing.T) {
	ed := &Editor{
		lines: []string{
			"\thello world",
		},
		row: 0,
		col: 6,
	}

	ctx := &kero.Context{Width: 80, Height: 24}
	ev := kero.KeyEvent{Key: kero.KeyTab, Mod: kero.ModShift}

	err := ed.Update(ctx, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ed.lines[0] != "hello world" {
		t.Errorf("lines[0] = %q, want %q", ed.lines[0], "hello world")
	}
	if ed.col != 5 {
		t.Errorf("col = %d, want 5", ed.col)
	}
}

func TestStartSelectLine(t *testing.T) {
	ed := &Editor{
		lines: []string{
			"first line",
			"second line",
			"third line",
		},
		row: 0,
		col: 3,
	}

	// 1st Ctrl+L: selects line 0 down to line 1 col 0
	ed.selectLine()
	if !ed.selecting {
		t.Errorf("expected selecting to be true")
	}
	if ed.selStartRow != 0 || ed.selStartCol != 0 {
		t.Errorf("selStart = (%d, %d), want (0, 0)", ed.selStartRow, ed.selStartCol)
	}
	if ed.selEndRow != 1 || ed.selEndCol != 0 {
		t.Errorf("selEnd = (%d, %d), want (1, 0)", ed.selEndRow, ed.selEndCol)
	}
	if ed.row != 1 || ed.col != 0 {
		t.Errorf("cursor = (%d, %d), want (1, 0)", ed.row, ed.col)
	}

	// 2nd Ctrl+L: extends selection to line 2 col 0
	ed.selectLine()
	if ed.selEndRow != 2 || ed.selEndCol != 0 {
		t.Errorf("selEnd = (%d, %d), want (2, 0)", ed.selEndRow, ed.selEndCol)
	}

	// 3rd Ctrl+L: extends selection to line 2 end
	ed.selectLine()
	if ed.selEndRow != 2 || ed.selEndCol != len("third line") {
		t.Errorf("selEnd = (%d, %d), want (2, %d)", ed.selEndRow, ed.selEndCol, len("third line"))
	}
}

func TestWordUnderCursor(t *testing.T) {
	ed := &Editor{
		lines: []string{
			"func (e *Editor) finishCommand() error {",
		},
		row: 0,
		col: 20, // on 'f' in finishCommand
	}
	if got := ed.wordUnderCursor(); got != "finishCommand" {
		t.Fatalf("wordUnderCursor() = %q, want %q", got, "finishCommand")
	}
}

func TestSmartGoto(t *testing.T) {
	ed := &Editor{
		lines: []string{
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
		},
		row: 11, // line with e.finishCommand()
		col: 7,  // on finishCommand
	}

	ctx := &kero.Context{Width: 80, Height: 24}

	// Press Ctrl+]
	ev := kero.KeyEvent{Key: kero.KeyRune, Rune: ']', Mod: kero.ModCtrl}
	if err := ed.Update(ctx, ev); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ed.row != 5 {
		t.Errorf("ed.row = %d, want 5 (line of func (e *Editor) finishCommand)", ed.row)
	}
}
