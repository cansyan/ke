package main

import (
	"io"
	"testing"
)

func TestFindNextWrapsWithinStartingLine(t *testing.T) {
	b := NewBuffer("", []byte("target middle\nother line"))

	start, end, ok := b.FindNext("target", Position{Row: 0, Col: 7})
	if !ok {
		t.Fatal("FindNext() did not find the wrapped match")
	}

	if start != (Position{Row: 0, Col: 0}) || end != (Position{Row: 0, Col: 6}) {
		t.Fatalf("FindNext() = (%+v, %+v), want ({Row: 0, Col: 0}, {Row: 0, Col: 6})", start, end)
	}
}

func TestFindPrevWrapsWithinStartingLine(t *testing.T) {
	b := NewBuffer("", []byte("target middle\nother line"))

	start, end, ok := b.FindPrev("target", Position{Row: 0, Col: 6})
	if !ok {
		t.Fatal("FindPrev() did not find the wrapped match")
	}

	if start != (Position{Row: 0, Col: 0}) || end != (Position{Row: 0, Col: 6}) {
		t.Fatalf("FindPrev() = (%+v, %+v), want ({Row: 0, Col: 0}, {Row: 0, Col: 6})", start, end)
	}
}

func TestFindIgnoresCase(t *testing.T) {
	b := NewBuffer("", []byte("Target middle TARGET"))

	start, end, ok := b.FindNextIgnoreCase("target", Position{Row: 0, Col: 0})
	if !ok || start != (Position{Row: 0, Col: 0}) || end != (Position{Row: 0, Col: 6}) {
		t.Fatalf("FindNext() = (%+v, %+v, %v), want ({Row: 0, Col: 0}, {Row: 0, Col: 6}, true)", start, end, ok)
	}

	start, end, ok = b.FindPrevIgnoreCase("target", Position{Row: 0, Col: len([]rune("Target middle TARGET"))})
	if !ok || start != (Position{Row: 0, Col: 14}) || end != (Position{Row: 0, Col: 20}) {
		t.Fatalf("FindPrev() = (%+v, %+v, %v), want ({Row: 0, Col: 14}, {Row: 0, Col: 20}, true)", start, end, ok)
	}
}

func TestFindAndReplaceAllAreCaseSensitive(t *testing.T) {
	b := NewBuffer("", []byte("Target target TARGET"))

	if _, _, ok := b.FindNext("target", Position{Row: 0, Col: 0}); !ok {
		t.Fatal("FindNext() did not find the exact-case match")
	}
	if start, _, ok := b.FindNext("TARGET", Position{Row: 0, Col: 0}); !ok || start.Col != 14 {
		t.Fatalf("FindNext(TARGET) = (%+v, %v), want exact match at column 14", start, ok)
	}
	if _, _, ok := b.FindNext("tArGeT", Position{Row: 0, Col: 0}); ok {
		t.Fatal("FindNext() matched a case-mismatched query")
	}

	if count := b.ReplaceAll("target", "x"); count != 1 {
		t.Fatalf("ReplaceAll() count = %d, want 1", count)
	}
	if got := string(b.Lines[0]); got != "Target x TARGET" {
		t.Fatalf("ReplaceAll() result = %q, want %q", got, "Target x TARGET")
	}
}

func TestBufferReader_TrailingNewline(t *testing.T) {
	tests := []struct {
		name     string
		lines    [][]byte
		expected string
	}{
		{
			name:     "Single line file",
			lines:    [][]byte{[]byte("package main")},
			expected: "package main",
		},
		{
			name:     "Multi-line file",
			lines:    [][]byte{[]byte("package main"), []byte(""), []byte("func main() {}")},
			expected: "package main\n\nfunc main() {}",
		},
		{
			name:     "File ending with an empty line",
			lines:    [][]byte{[]byte("foo"), []byte("bar"), []byte("")},
			expected: "foo\nbar\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &Buffer{Lines: tt.lines}
			reader := buf.NewReader()

			// Read entire content via io.ReadAll (uses varying buffer chunk sizes)
			gotBytes, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("unexpected error reading buffer: %v", err)
			}

			got := string(gotBytes)
			if got != tt.expected {
				t.Errorf("content mismatch:\ngot:      %q\nexpected: %q", got, tt.expected)
			}
		})
	}
}

func TestBufferUndo(t *testing.T) {
	b := NewBuffer("", nil)
	var p Position
	src := "hi"
	p = b.Insert(p, src)
	p = b.Insert(p, src)

	// the second edit should be merged to the first one
	if len(b.Lines) == 0 || len(b.records) != 1 {
		t.Fatalf("unexpected buffer length: %d", len(b.Lines))
	}

	// delete the line
	b.Delete(Position{Row: 0, Col: 0}, p)

	b.Undo()
	b.Undo()
	if got := string(b.Lines[0]); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
	// t.Logf("%+v, %d", b.records, b.recordIdx)

	// redo insert
	b.Redo()
	want := "hihi"
	if got := string(b.Lines[0]); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}

	// redo delete
	b.Redo()
	want = ""
	if got := string(b.Lines[0]); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}
