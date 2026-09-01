package main

import (
	"bytes"
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
			expected: "package main\n",
		},
		{
			name:     "Multi-line file",
			lines:    [][]byte{[]byte("package main"), []byte(""), []byte("func main() {}")},
			expected: "package main\n\nfunc main() {}\n",
		},
		{
			name:     "File ending with an empty line",
			lines:    [][]byte{[]byte("foo"), []byte("bar"), []byte("")},
			expected: "foo\nbar\n\n",
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

// TestBufferReader_SmallBuffer simulates Read() calls with tiny slice chunks (e.g. 4 bytes)
// to catch edge cases where line text fills p right before '\n'.
func TestBufferReader_SmallBuffer(t *testing.T) {
	buf := &Buffer{
		Lines: [][]byte{
			[]byte("hello"),
			[]byte("world"),
		},
	}
	expected := "hello\nworld\n"

	reader := buf.NewReader()
	var out bytes.Buffer
	p := make([]byte, 4) // small 4-byte buffer to force multiple Read iterations

	for {
		n, err := reader.Read(p)
		if n > 0 {
			out.Write(p[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}
	}

	if out.String() != expected {
		t.Errorf("small buffer chunk read failed:\ngot:      %q\nexpected: %q", out.String(), expected)
	}
}
