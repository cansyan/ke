package main

import "kero"

// TextInput is a small editable single-line text widget.
type TextInput struct {
	Value    string
	Cursor   int
	SelStart int // selection start
	SelEnd   int
}

func (t *TextInput) adjustSelect() (int, int) {
	runes := []rune(t.Value)
	start, end := t.SelStart, t.SelEnd
	if start > end {
		start, end = end, start
	}
	if start < 0 {
		start = 0
	}
	if end < 0 {
		end = 0
	}
	if start > len(runes) {
		start = len(runes)
	}
	if end > len(runes) {
		end = len(runes)
	}
	return start, end
}

func (t *TextInput) clearSelect() {
	t.SelStart = t.Cursor
	t.SelEnd = t.Cursor
}

// Update applies keyboard input to the text input.
func (t *TextInput) Update(ev kero.Event) {
	e, ok := ev.(kero.KeyEvent)
	if !ok {
		return
	}

	runes := []rune(t.Value)
	if t.Cursor < 0 {
		t.Cursor = 0
	}
	if t.Cursor > len(runes) {
		t.Cursor = len(runes)
	}

	switch e.Key {
	case kero.KeyRune:
		if e.Mod&kero.ModCtrl != 0 {
			return
		}
		start, end := t.adjustSelect()
		if start != end {
			runes = append(runes[:start], runes[end:]...)
			t.Cursor = start
			t.SelStart = start
			t.SelEnd = start
		}

		runes = append(runes, 0)
		copy(runes[t.Cursor+1:], runes[t.Cursor:])
		runes[t.Cursor] = e.Rune
		t.Cursor++
		t.clearSelect()
	case kero.KeyBackspace:
		start, end := t.adjustSelect()
		if start != end {
			runes = append(runes[:start], runes[end:]...)
			t.Cursor = start
			t.clearSelect()
			break
		}
		if t.Cursor > 0 {
			runes = append(runes[:t.Cursor-1], runes[t.Cursor:]...)
			t.Cursor--
		}
	case kero.KeyDelete:
		start, end := t.adjustSelect()
		if start != end {
			runes = append(runes[:start], runes[end:]...)
			t.Cursor = start
			t.clearSelect()
			break
		}
		if t.Cursor < len(runes) {
			runes = append(runes[:t.Cursor], runes[t.Cursor+1:]...)
		}
	case kero.KeyLeft:
		if t.Cursor > 0 {
			t.Cursor--
		}
	case kero.KeyRight:
		if t.Cursor < len(runes) {
			t.Cursor++
		}
	case kero.KeyHome:
		t.Cursor = 0
	case kero.KeyEnd:
		t.Cursor = len(runes)
	}

	t.Value = string(runes)
}

// Draw renders the text input and its cursor.
func (t TextInput) Draw(f *kero.Frame, r kero.Rect, s kero.Style) {
	selectStyle := s.Foreground(kero.ColorBlack).Background(kero.ColorYellow)
	cursorStyle := s.Reverse()
	runes := []rune(t.Value)

	if t.Cursor < 0 {
		t.Cursor = 0
	}
	if t.Cursor > len(runes) {
		t.Cursor = len(runes)
	}

	start, end := t.adjustSelect()

	for i, ch := range runes {
		x := r.X + i
		y := r.Y

		if x >= r.Right() {
			break
		}

		if i >= start && i < end {
			f.Set(x, y, ch, selectStyle)
			continue
		}

		if i == t.Cursor {
			f.Set(x, y, ch, cursorStyle)
			continue
		}

		f.Set(x, y, ch, s)
	}

	if t.Cursor == len(runes) {
		x := r.X + t.Cursor
		if x < r.Right() {
			f.Set(x, r.Y, ' ', cursorStyle)
		}
	}
}
