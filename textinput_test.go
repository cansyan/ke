package main

import (
	"testing"

	"github.com/cansyan/kero"
)

func TestTextInputUpdate(t *testing.T) {
	input := TextInput{}
	input.Update(kero.KeyEvent{Key: kero.KeyRune, Rune: 'a'})
	input.Update(kero.KeyEvent{Key: kero.KeyRune, Rune: 'b'})
	input.Update(kero.KeyEvent{Key: kero.KeyLeft})
	input.Update(kero.KeyEvent{Key: kero.KeyRune, Rune: 'X'})

	if input.String() != "aXb" {
		t.Fatalf("Value = %q, want aXb", input.String())
	}
	if input.Cursor != 2 {
		t.Fatalf("Cursor = %d, want 2", input.Cursor)
	}
}

func TestTextInputDrawCursorAtEnd(t *testing.T) {
	var input TextInput
	input.SetText("hi")
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

func TestTextInputSelectAll(t *testing.T) {
	var input TextInput
	input.SetTextAndSelectAll("abcd")

	input.Update(kero.KeyEvent{Key: kero.KeyRune, Rune: 'X'})

	if input.String() != "X" {
		t.Fatalf("Value = %q, want X", input.String())
	}
	if input.Cursor != 1 {
		t.Fatalf("Cursor = %d, want 1", input.Cursor)
	}
	if input.SelectAll != false {
		t.Fatalf("SelectAll = %v, want false", input.SelectAll)
	}
}
