package lsp

type InitializeParams struct {
	ProcessID             int                   `json:"processId"`
	RootURI               string                `json:"rootUri"`
	Capabilities          ClientCapabilities    `json:"capabilities"`
	InitializationOptions InitializationOptions `json:"initializationOptions,omitempty"`
}

type InitializationOptions struct {
	SemanticTokens bool `json:"ui.semanticTokens,omitempty"`
}

type ClientCapabilities struct {
	TextDocument TextDocumentClientCapabilities `json:"textDocument,omitempty"`
}

type TextDocumentClientCapabilities struct {
	SemanticTokens SemanticTokensClientCapabilities `json:"semanticTokens,omitempty"`
}

type SemanticTokensClientCapabilities struct {
	Requests                SemanticTokensRequestsClientCapabilities `json:"requests,omitempty"`
	TokenTypes              []string                                 `json:"tokenTypes,omitempty"`
	TokenModifiers          []string                                 `json:"tokenModifiers,omitempty"`
	Formats                 []string                                 `json:"formats,omitempty"`
	OverlappingTokenSupport bool                                     `json:"overlappingTokenSupport,omitempty"`
	MultilineTokenSupport   bool                                     `json:"multilineTokenSupport,omitempty"`
}

type SemanticTokensRequestsClientCapabilities struct {
	Full bool `json:"full,omitempty"`
}

type DidOpenTextDocumentParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type DidChangeTextDocumentParams struct {
	TextDocument   VersionedTextDocumentIdentifier  `json:"textDocument"`
	ContentChanges []TextDocumentContentChangeEvent `json:"contentChanges"`
}

type VersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

type TextDocumentContentChangeEvent struct {
	Text string `json:"text"`
}

type DidSaveTextDocumentParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

const (
	DiagnosticSeverityError   = 1
	DiagnosticSeverityWarning = 2
	DiagnosticSeverityInfo    = 3
	DiagnosticSeverityHint    = 4
)

// Diagnostic represents an item reported by textDocument/publishDiagnostics.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"` // 1: Error, 2: Warning, 3: Information, 4: Hint
	Code     string `json:"code,omitempty"`
	Message  string `json:"message"`
}

type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Position struct {
	Line      int `json:"line"`      // 0-based
	Character int `json:"character"` // 0-based UTF-16 code units
}

// TextDocumentPositionParams contains parameters for location-based requests
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"` // 0-based Line, Character
}

// Location represents a location inside a resource, such as a line inside a text file.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// ReferenceContext controls whether the symbol's declaration is included.
type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// ReferenceParams contains parameters for textDocument/references requests.
type ReferenceParams struct {
	TextDocumentPositionParams
	Context ReferenceContext `json:"context"`
}

// TextEdit represents a textual edit applicable to a text document.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// WorkspaceEdit represents changes to many resources managed in the workspace.
type WorkspaceEdit struct {
	// Changes maps file URIs to a slice of TextEdits
	Changes         map[string][]TextEdit `json:"changes,omitempty"`
	DocumentChanges []TextDocumentEdit    `json:"documentChanges,omitempty"`
}

type TextDocumentEdit struct {
	TextDocument VersionedTextDocumentIdentifier `json:"textDocument"`
	Edits        []TextEdit                      `json:"edits"`
}

// RenameParams parameters for textDocument/rename request.
type RenameParams struct {
	TextDocumentPositionParams
	NewName string `json:"newName"`
}

// DocumentSymbolParams contains parameters for textDocument/documentSymbol requests.
type DocumentSymbolParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// CompletionParams contains parameters for textDocument/completion requests.
type CompletionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	Context      *CompletionContext     `json:"context,omitempty"`
}

// CompletionContext contains trigger information for completion requests.
type CompletionContext struct {
	TriggerKind      int    `json:"triggerKind"`
	TriggerCharacter string `json:"triggerCharacter,omitempty"`
}

// CompletionItem is a single completion entry returned by the server.
type CompletionItem struct {
	Label            string    `json:"label"`
	Kind             int       `json:"kind,omitempty"`
	Detail           string    `json:"detail,omitempty"`
	Documentation    any       `json:"documentation,omitempty"`
	SortText         string    `json:"sortText,omitempty"`
	FilterText       string    `json:"filterText,omitempty"`
	InsertText       string    `json:"insertText,omitempty"`
	InsertTextFormat int       `json:"insertTextFormat,omitempty"`
	TextEdit         *TextEdit `json:"textEdit,omitempty"`
}

// CompletionList wraps completion entries and can be partial.
type CompletionList struct {
	IsIncomplete bool             `json:"isIncomplete"`
	Items        []CompletionItem `json:"items"`
}

// WorkspaceSymbolParams contains parameters for workspace/symbol requests.
type WorkspaceSymbolParams struct {
	Query string `json:"query"`
}

type SymbolKind uint32

func (k SymbolKind) String() string {
	switch k {
	case File:
		return "File"
	case Module:
		return "Module"
	case Namespace:
		return "Namespace"
	case Package:
		return "Package"
	case Class:
		return "Class"
	case Method:
		return "Method"
	case Property:
		return "Property"
	case Field:
		return "Field"
	case Constructor:
		return "Constructor"
	case Enum:
		return "Enum"
	case Interface:
		return "Interface"
	case Function:
		return "Function"
	case Variable:
		return "Variable"
	case Constant:
		return "Constant"
	case String:
		return "String"
	case Number:
		return "Number"
	case Boolean:
		return "Boolean"
	case Array:
		return "Array"
	case Object:
		return "Object"
	case Key:
		return "Key"
	case Null:
		return "Null"
	case EnumMember:
		return "EnumMember"
	case Struct:
		return "Struct"
	case Event:
		return "Event"
	case Operator:
		return "Operator"
	case TypeParameter:
		return "TypeParameter"
	default:
		return "Unknown"
	}
}

// from gopls internal implementation, more details see:
// https://github.com/golang/tools/blob/master/gopls/internal/protocol/tsprotocol.go#L6949
const (
	File          SymbolKind = 1
	Module        SymbolKind = 2
	Namespace     SymbolKind = 3
	Package       SymbolKind = 4
	Class         SymbolKind = 5
	Method        SymbolKind = 6
	Property      SymbolKind = 7
	Field         SymbolKind = 8
	Constructor   SymbolKind = 9
	Enum          SymbolKind = 10
	Interface     SymbolKind = 11
	Function      SymbolKind = 12
	Variable      SymbolKind = 13
	Constant      SymbolKind = 14
	String        SymbolKind = 15
	Number        SymbolKind = 16
	Boolean       SymbolKind = 17
	Array         SymbolKind = 18
	Object        SymbolKind = 19
	Key           SymbolKind = 20
	Null          SymbolKind = 21
	EnumMember    SymbolKind = 22
	Struct        SymbolKind = 23
	Event         SymbolKind = 24
	Operator      SymbolKind = 25
	TypeParameter SymbolKind = 26
)

// DocumentSymbol represents a symbol definition inside a document.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Kind           SymbolKind       `json:"kind"`
	Deprecated     bool             `json:"deprecated,omitempty"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// SymbolInformation is the older flat form returned by some LSP servers.
type SymbolInformation struct {
	Name          string     `json:"name"`
	Kind          SymbolKind `json:"kind"`
	Deprecated    bool       `json:"deprecated,omitempty"`
	Location      Location   `json:"location"`
	ContainerName string     `json:"containerName,omitempty"`
}

type SemanticTokensParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// SemanticTokens Legend describes the token types and modifiers supported/returned by the server.
type SemanticTokensLegend struct {
	TokenTypes     []string `json:"tokenTypes"`
	TokenModifiers []string `json:"tokenModifiers"`
}

const (
	// These are the tokens defined by LSP 3.18, but a client is
	// free to send its own set; any tokens that the server emits
	// that are not in this set are simply not encoded in the bitfield.
	TokComment   = "comment"       // for a comment
	TokFunction  = "function"      // for a function
	TokKeyword   = "keyword"       // for a keyword
	TokLabel     = "label"         // for a control label (LSP 3.18)
	TokMacro     = "macro"         // for text/template tokens
	TokMethod    = "method"        // for a method
	TokNamespace = "namespace"     // for an imported package name
	TokNumber    = "number"        // for a numeric literal
	TokOperator  = "operator"      // for an operator
	TokParameter = "parameter"     // for a parameter variable
	TokProperty  = "property"      // for a struct field
	TokString    = "string"        // for a string literal
	TokType      = "type"          // for a type name (plus other uses)
	TokTypeParam = "typeParameter" // for a type parameter
	TokVariable  = "variable"      // for a var or const
)

// DefaultGoplsLegend is a slice of types gopls will return as its server capabilities.
var DefaultGoplsLegend = []string{
	TokNamespace,
	TokType,
	TokTypeParam,
	TokParameter,
	TokProperty,
	TokVariable,
	TokFunction,
	TokMethod,
	TokMacro,
	TokKeyword,
	TokComment,
	TokString,
	TokNumber,
	TokOperator,
	TokLabel,
}

type SemanticTokensOptions struct {
	Legend SemanticTokensLegend `json:"legend"`
	Full   bool                 `json:"full,omitempty"`
}

type SemanticTokens struct {
	ResultID string   `json:"resultId,omitempty"`
	Data     []uint32 `json:"data"` // Delta-encoded integers
}
