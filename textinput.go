package main

import (
	"slices"

	"github.com/cansyan/kero"
)

// TextInput is a small editable single-line text widget.
type TextInput struct {
	runes       []rune
	Cursor      int // Rune index
	Placeholder string
	SelectAll   bool
}

// SetText populates the input and sets cursor to the end.
func (t *TextInput) SetText(s string) {
	t.runes = []rune(s)
	t.Cursor = len(t.runes)
}

func (t *TextInput) SetTextAndSelectAll(s string) {
	t.runes = []rune(s)
	t.Cursor = len(t.runes)
	t.SelectAll = true
}

func (t *TextInput) Reset() {
	t.runes = t.runes[:0] // Reuse underlying array memory
	t.Cursor = 0
	t.Placeholder = ""
	t.SelectAll = false
}

func (t *TextInput) String() string {
	return string(t.runes)
}

// Len returns the current rune count.
func (t *TextInput) Len() int {
	return len(t.runes)
}

func (t *TextInput) Update(ev kero.Event) {
	e, ok := ev.(kero.KeyEvent)
	if !ok {
		return
	}

	// Handle SelectAll replacement on first keystroke
	if t.SelectAll {
		if e.Key == kero.KeyRune || e.Key == kero.KeyBackspace {
			t.SelectAll = false
			t.runes = t.runes[:0]
			t.Cursor = 0
			if e.Key == kero.KeyBackspace {
				return
			}
		} else {
			// Arrow keys or Esc just clear selection state
			t.SelectAll = false
		}
	}

	switch e.Key {
	case kero.KeyRune:
		if e.Mod&kero.ModCtrl != 0 {
			return
		}
		t.runes = slices.Insert(t.runes, t.Cursor, e.Rune)
		t.Cursor++

	case kero.KeyBackspace:
		if t.Cursor > 0 {
			t.runes = slices.Delete(t.runes, t.Cursor-1, t.Cursor)
			t.Cursor--
		}

	case kero.KeyDelete:
		if t.Cursor < len(t.runes) {
			t.runes = slices.Delete(t.runes, t.Cursor, t.Cursor+1)
		}

	case kero.KeyLeft:
		if t.Cursor > 0 {
			t.Cursor--
		}

	case kero.KeyRight:
		if t.Cursor < len(t.runes) {
			t.Cursor++
		}

	case kero.KeyHome:
		t.Cursor = 0

	case kero.KeyEnd:
		t.Cursor = len(t.runes)
	}
}

// toggleReverse flips the reverse attribute bit.
func toggleReverse(s kero.Style) kero.Style {
	s.Attr = s.Attr ^ kero.AttrReverse
	return s
}

// Draw renders the text input and its cursor.
func (t TextInput) Draw(f *kero.Frame, r kero.Rect, s kero.Style) {
	cursorStyle := toggleReverse(s)

	if t.SelectAll && len(t.runes) > 0 {
		for i, ch := range t.runes {
			x := r.X + i
			if x < r.Right() {
				f.Set(x, r.Y, ch, s.Reverse())
			}
		}
		return
	}

	if t.Cursor < 0 {
		t.Cursor = 0
	}
	if t.Cursor > len(t.runes) {
		t.Cursor = len(t.runes)
	}

	// Render placeholder when value is empty
	if len(t.runes) == 0 && t.Placeholder != "" {
		for i, ch := range []rune(t.Placeholder) {
			if i == 0 {
				f.Set(r.X+i, r.Y, ch, cursorStyle)
			} else {
				f.Set(r.X+i, r.Y, ch, s.Dim())
			}
		}
		return
	}

	// Render characters
	for i, ch := range t.runes {
		x := r.X + i
		if x >= r.Right() {
			break
		}

		if i == t.Cursor {
			f.Set(x, r.Y, ch, cursorStyle)
		} else {
			f.Set(x, r.Y, ch, s)
		}
	}

	// Render trailing cursor when at end of line
	if t.Cursor == len(t.runes) {
		x := r.X + t.Cursor
		if x < r.Right() {
			f.Set(x, r.Y, ' ', cursorStyle)
		}
	}
}
