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

