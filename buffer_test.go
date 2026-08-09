package main

import "testing"

func TestFindNextWrapsWithinStartingLine(t *testing.T) {
	buffer := NewBuffer("target middle\nother line")

	start, end, ok := buffer.FindNext("target", Position{Row: 0, Col: 7})
	if !ok {
		t.Fatal("FindNext() did not find the wrapped match")
	}

	if start != (Position{Row: 0, Col: 0}) || end != (Position{Row: 0, Col: 6}) {
		t.Fatalf("FindNext() = (%+v, %+v), want ({Row: 0, Col: 0}, {Row: 0, Col: 6})", start, end)
	}
}

func TestFindPrevWrapsWithinStartingLine(t *testing.T) {
	buffer := NewBuffer("target middle\nother line")

	start, end, ok := buffer.FindPrev("target", Position{Row: 0, Col: 6})
	if !ok {
		t.Fatal("FindPrev() did not find the wrapped match")
	}

	if start != (Position{Row: 0, Col: 0}) || end != (Position{Row: 0, Col: 6}) {
		t.Fatalf("FindPrev() = (%+v, %+v), want ({Row: 0, Col: 0}, {Row: 0, Col: 6})", start, end)
	}
}

func TestFindIgnoresCase(t *testing.T) {
	buffer := NewBuffer("Target middle TARGET")

	start, end, ok := buffer.FindNextIgnoreCase("target", Position{Row: 0, Col: 0})
	if !ok || start != (Position{Row: 0, Col: 0}) || end != (Position{Row: 0, Col: 6}) {
		t.Fatalf("FindNext() = (%+v, %+v, %v), want ({Row: 0, Col: 0}, {Row: 0, Col: 6}, true)", start, end, ok)
	}

	start, end, ok = buffer.FindPrevIgnoreCase("target", Position{Row: 0, Col: len([]rune("Target middle TARGET"))})
	if !ok || start != (Position{Row: 0, Col: 14}) || end != (Position{Row: 0, Col: 20}) {
		t.Fatalf("FindPrev() = (%+v, %+v, %v), want ({Row: 0, Col: 14}, {Row: 0, Col: 20}, true)", start, end, ok)
	}
}

func TestFindAndReplaceAllAreCaseSensitive(t *testing.T) {
	buffer := NewBuffer("Target target TARGET")

	if _, _, ok := buffer.FindNext("target", Position{Row: 0, Col: 0}); !ok {
		t.Fatal("FindNext() did not find the exact-case match")
	}
	if start, _, ok := buffer.FindNext("TARGET", Position{Row: 0, Col: 0}); !ok || start.Col != 14 {
		t.Fatalf("FindNext(TARGET) = (%+v, %v), want exact match at column 14", start, ok)
	}
	if _, _, ok := buffer.FindNext("tArGeT", Position{Row: 0, Col: 0}); ok {
		t.Fatal("FindNext() matched a case-mismatched query")
	}

	if count := buffer.ReplaceAll("target", "x"); count != 1 {
		t.Fatalf("ReplaceAll() count = %d, want 1", count)
	}
	if got := buffer.String(); got != "Target x TARGET" {
		t.Fatalf("ReplaceAll() result = %q, want %q", got, "Target x TARGET")
	}
}
