package main

import (
	"github.com/cansyan/kero"
	"testing"
)

func TestTextInputUpdate(t *testing.T) {
	input := TextInput{}
	input.Update(kero.KeyEvent{Key: kero.KeyRune, Rune: 'a'})
	input.Update(kero.KeyEvent{Key: kero.KeyRune, Rune: 'b'})
	input.Update(kero.KeyEvent{Key: kero.KeyLeft})
	input.Update(kero.KeyEvent{Key: kero.KeyRune, Rune: 'X'})

	if input.Value != "aXb" {
		t.Fatalf("Value = %q, want aXb", input.Value)
	}
	if input.Cursor != 2 {
		t.Fatalf("Cursor = %d, want 2", input.Cursor)
	}
}

func TestTextInputDrawCursorAtEnd(t *testing.T) {
	input := TextInput{Value: "hi", Cursor: 2}
	f := kero.NewFrame(4, 1)

	input.Draw(&f, kero.Rect{X: 0, Y: 0, W: 4, H: 1}, kero.NewStyle())

	cell := f.Cell(2, 0)
	if cell.Ch != ' ' {
		t.Fatalf("cursor cell = %q, want space", cell.Ch)
	}
	if cell.Style.Attr&kero.AttrReverse == 0 {
		t.Fatalf("cursor cell is not reversed")
	}
}

func TestTextInputInsertRuneDeletesSelection(t *testing.T) {
	input := TextInput{Value: "abcd", Cursor: 3, SelAnchor: 1}

	input.Update(kero.KeyEvent{Key: kero.KeyRune, Rune: 'X'})

	if input.Value != "aXd" {
		t.Fatalf("Value = %q, want aXd", input.Value)
	}
	if input.Cursor != 2 {
		t.Fatalf("Cursor = %d, want 2", input.Cursor)
	}
	if input.SelAnchor != 2 || input.Cursor != 2 {
		t.Fatalf("selection = (%d, %d), want nil", input.SelAnchor, input.Cursor)
	}
}
