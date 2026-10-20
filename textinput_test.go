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

func TestTextInputDrawWideCharacters(t *testing.T) {
	var input TextInput
	input.SetText("世界")
	f := kero.NewFrame(6, 1)

	input.Draw(&f, kero.Rect{X: 0, Y: 0, W: 6, H: 1}, kero.NewStyle())

	if got := f.Cell(0, 0).Ch; got != '世' {
		t.Fatalf("cell(0,0) = %q, want '世'", got)
	}
	if got := f.Cell(1, 0).Ch; got != kero.Continuation {
		t.Fatalf("cell(1,0) = %v, want Continuation", got)
	}
	if got := f.Cell(2, 0).Ch; got != '界' {
		t.Fatalf("cell(2,0) = %q, want '界'", got)
	}
	if got := f.Cell(3, 0).Ch; got != kero.Continuation {
		t.Fatalf("cell(3,0) = %v, want Continuation", got)
	}
	// Cursor at end of "世界" should be at visual column 4
	cell := f.Cell(4, 0)
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
