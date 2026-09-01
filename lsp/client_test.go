package lsp

import (
	"encoding/json"
	"testing"
)

func TestDecodeDocumentSymbolsFromHierarchicalResponse(t *testing.T) {
	payload := []byte(`[
		{"name":"Main","kind":12,"range":{"start":{"line":0,"character":0},"end":{"line":10,"character":0}},"selectionRange":{"start":{"line":0,"character":0},"end":{"line":10,"character":0}},"children":[
			{"name":"run","kind":6,"range":{"start":{"line":1,"character":0},"end":{"line":3,"character":0}},"selectionRange":{"start":{"line":1,"character":0},"end":{"line":3,"character":0}}}
		]}
	]`)

	symbols, err := decodeDocumentSymbols(payload)
	if err != nil {
		t.Fatalf("decodeDocumentSymbols returned error: %v", err)
	}
	if len(symbols) != 1 {
		t.Fatalf("expected 1 top-level symbol, got %d", len(symbols))
	}
	if symbols[0].Name != "Main" {
		t.Fatalf("expected top-level symbol name Main, got %q", symbols[0].Name)
	}
	if len(symbols[0].Children) != 1 || symbols[0].Children[0].Name != "run" {
		t.Fatalf("expected nested symbol run, got %#v", symbols[0].Children)
	}
}

func TestDecodeDocumentSymbolsFromFlatInfoResponse(t *testing.T) {
	payload := []byte(`[
		{"name":"Item","kind":5,"location":{"uri":"file:///tmp/main.go","range":{"start":{"line":2,"character":4},"end":{"line":2,"character":8}}}}
	]`)

	symbols, err := decodeDocumentSymbols(payload)
	if err != nil {
		t.Fatalf("decodeDocumentSymbols returned error: %v", err)
	}
	if len(symbols) != 1 {
		t.Fatalf("expected 1 symbol, got %d", len(symbols))
	}
	if symbols[0].Name != "Item" {
		t.Fatalf("expected symbol name Item, got %q", symbols[0].Name)
	}
	if symbols[0].SelectionRange.Start.Line != 2 || symbols[0].SelectionRange.Start.Character != 4 {
		t.Fatalf("expected selection range to be preserved, got %#v", symbols[0].SelectionRange)
	}
	_ = json.RawMessage(nil)
}

func TestDecodeWorkspaceSymbolsResponse(t *testing.T) {
	payload := []byte(`[
		{"name":"View","kind":6,"location":{"uri":"file:///tmp/main.go","range":{"start":{"line":10,"character":0},"end":{"line":20,"character":0}}},"containerName":"main"}
	]`)

	symbols, err := decodeWorkspaceSymbols(payload)
	if err != nil {
		t.Fatalf("decodeWorkspaceSymbols returned error: %v", err)
	}
	if len(symbols) != 1 {
		t.Fatalf("expected 1 workspace symbol, got %d", len(symbols))
	}
	if symbols[0].Name != "View" {
		t.Fatalf("expected workspace symbol name View, got %q", symbols[0].Name)
	}
	if symbols[0].Location.URI != "file:///tmp/main.go" {
		t.Fatalf("expected workspace symbol location uri file:///tmp/main.go, got %q", symbols[0].Location.URI)
	}
}
