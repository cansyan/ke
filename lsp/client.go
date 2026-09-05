package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type Client struct {
	cmd           *exec.Cmd
	stdin         io.Writer
	reqID         int64
	OnDiagnostics func(uri string, diags []Diagnostic)
	mu            sync.Mutex

	nextID    atomic.Int64
	pendingMu sync.Mutex
	pending   map[int64]chan []byte
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// RawResponse maps incoming JSON-RPC response fields.
type RawResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// ExtractID converts raw JSON ID field into int64. Returns 0 if missing or invalid.
func (r *RawResponse) ExtractID() int64 {
	if len(r.ID) == 0 || bytes.Equal(r.ID, []byte("null")) {
		return 0
	}
	// Handles int numeric IDs: 1, 2, 3...
	if id, err := strconv.ParseInt(string(r.ID), 10, 64); err == nil {
		return id
	}
	return 0
}

func (c *Client) SendRequest(method string, params any) int64 {
	id := c.nextID.Add(1)

	req := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return 0
	}

	c.writeRaw(data)
	return id
}

func (c *Client) SendRequestWithChan(method string, params any, respChan chan []byte) int64 {
	id := c.nextID.Add(1)

	c.pendingMu.Lock()
	if c.pending == nil {
		c.pending = make(map[int64]chan []byte)
	}
	c.pending[id] = respChan
	c.pendingMu.Unlock()

	req := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return 0
	}

	c.writeRaw(data)
	return id
}

func (c *Client) writeRaw(data []byte) {
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))

	c.mu.Lock()
	defer c.mu.Unlock()
	c.stdin.Write([]byte(header))
	c.stdin.Write(data)
}

func StartClient(goplsPath string, onDiagnostics func(string, []Diagnostic)) (*Client, error) {
	cmd := exec.Command(goplsPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	c := &Client{
		cmd:           cmd,
		stdin:         stdin,
		OnDiagnostics: onDiagnostics,
	}

	go c.readLoop(bufio.NewReader(stdout))
	return c, nil
}

func (c *Client) SendNotification(method string, params any) {
	msg := notification{JSONRPC: "2.0", Method: method, Params: params}
	c.write(msg)
}

func (c *Client) write(v any) {
	data, _ := json.Marshal(v)
	body := string(data)
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))

	c.mu.Lock()
	defer c.mu.Unlock()
	c.stdin.Write([]byte(header + body))
}

// readLoop continuously reads LSP JSON-RPC frames from server stdout
func (c *Client) readLoop(r io.Reader) {
	bufReader := bufio.NewReader(r)

	for {
		// 1. Read LSP Headers (e.g., Content-Length: 123\r\n\r\n)
		contentLength := 0
		for {
			line, err := bufReader.ReadString('\n')
			if err != nil {
				return // Server connection closed or crashed
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break // End of headers
			}
			if after, ok := strings.CutPrefix(line, "Content-Length:"); ok {
				lengthStr := strings.TrimSpace(after)
				contentLength, _ = strconv.Atoi(lengthStr)
			}
		}

		if contentLength == 0 {
			continue
		}

		// 2. Read full JSON payload body
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(bufReader, body); err != nil {
			return
		}

		// 3. Dispatch payload
		c.dispatchIncomingMessage(body)
	}
}

// dispatchIncomingMessage checks if message is a response to a request or a notification
func (c *Client) dispatchIncomingMessage(body []byte) {
	var raw RawResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return
	}

	reqID := raw.ExtractID()

	// Case A: The message is a response to an outgoing request (reqID > 0)
	if reqID > 0 {
		c.pendingMu.Lock()
		ch, exists := c.pending[reqID]
		c.pendingMu.Unlock()

		if exists {
			if raw.Error != nil {
				log.Printf("LSP request %d error: code=%d message=%s", reqID, raw.Error.Code, raw.Error.Message)
				ch <- nil
			} else {
				// Send raw JSON result payload back to the awaiting request caller
				ch <- raw.Result
			}
		}
		return
	}

	// Case B: The message is an async notification from server (e.g. textDocument/publishDiagnostics)
	var notif struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &notif); err == nil && notif.Method != "" {
		c.handleServerNotification(notif.Method, notif.Params)
	}
}

func (c *Client) Close() {
	c.cmd.Process.Kill()
}

// Response structure for RPC calls
type Response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) GotoDefinition(uri string, line, char int) ([]Location, error) {
	params := TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     Position{Line: line, Character: char},
	}

	// Register channel for response
	respChan := make(chan []byte, 1)
	id := c.SendRequest("textDocument/definition", params)

	c.pendingMu.Lock()
	if c.pending == nil {
		c.pending = make(map[int64]chan []byte)
	}
	c.pending[id] = respChan
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	// Await response with timeout (e.g. 2s)
	select {
	case data := <-respChan:
		if len(data) == 0 || string(data) == "null" {
			return nil, nil // No definition found
		}

		// Response can be a single Location or []Location
		var single Location
		if err := json.Unmarshal(data, &single); err == nil && single.URI != "" {
			return []Location{single}, nil
		}

		var multi []Location
		if err := json.Unmarshal(data, &multi); err == nil {
			return multi, nil
		}

		return nil, fmt.Errorf("failed to parse definition response")

	case <-time.After(2 * time.Second):
		return nil, fmt.Errorf("goto definition request timed out")
	}
}

// handleServerNotification dispatches incoming async notifications from the LSP server.
func (c *Client) handleServerNotification(method string, params json.RawMessage) {
	switch method {
	case "textDocument/publishDiagnostics":
		var diagParams PublishDiagnosticsParams
		if err := json.Unmarshal(params, &diagParams); err != nil {
			log.Printf("failed to unmarshal diagnostics: %v", err)
			return
		}

		// Optional: Forward diagnostics to custom handler if registered
		if c.OnDiagnostics != nil {
			c.OnDiagnostics(diagParams.URI, diagParams.Diagnostics)
		}

	default:
		// Ignore unhandled notifications (e.g., window/logMessage, $/progress)
	}
}

func (c *Client) FindReferences(uri string, line, char int, includeDecl bool) ([]Location, error) {
	params := ReferenceParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: uri},
			Position:     Position{Line: line, Character: char},
		},
		Context: ReferenceContext{
			IncludeDeclaration: includeDecl,
		},
	}

	respChan := make(chan []byte, 1)
	id := c.SendRequest("textDocument/references", params)

	c.pendingMu.Lock()
	if c.pending == nil {
		c.pending = make(map[int64]chan []byte)
	}
	c.pending[id] = respChan
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	select {
	case data := <-respChan:
		if len(data) == 0 || string(data) == "null" {
			return nil, nil // No references found
		}

		var locs []Location
		if err := json.Unmarshal(data, &locs); err != nil {
			return nil, fmt.Errorf("failed to parse references response: %w", err)
		}
		return locs, nil

	case <-time.After(3 * time.Second):
		return nil, fmt.Errorf("find references request timed out")
	}
}

func (c *Client) Rename(uri string, line, char int, newName string) (*WorkspaceEdit, error) {
	params := RenameParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: uri},
			Position:     Position{Line: line, Character: char},
		},
		NewName: newName,
	}

	respChan := make(chan []byte, 1)
	id := c.SendRequest("textDocument/rename", params)

	c.pendingMu.Lock()
	if c.pending == nil {
		c.pending = make(map[int64]chan []byte)
	}
	c.pending[id] = respChan
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	select {
	case data := <-respChan:
		if len(data) == 0 || string(data) == "null" {
			return nil, nil // No edits returned or invalid symbol
		}

		var edit WorkspaceEdit
		if err := json.Unmarshal(data, &edit); err != nil {
			return nil, fmt.Errorf("failed to parse rename workspace edit: %w", err)
		}
		return &edit, nil

	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("rename request timed out")
	}
}

func (c *Client) DocumentSymbols(uri string) ([]DocumentSymbol, error) {
	params := DocumentSymbolParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
	}

	respChan := make(chan []byte, 1)
	id := c.SendRequest("textDocument/documentSymbol", params)

	c.pendingMu.Lock()
	if c.pending == nil {
		c.pending = make(map[int64]chan []byte)
	}
	c.pending[id] = respChan
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	select {
	case data := <-respChan:
		if len(data) == 0 || string(data) == "null" {
			return nil, nil
		}
		return decodeDocumentSymbols(data)
	case <-time.After(3 * time.Second):
		return nil, fmt.Errorf("document symbols request timed out")
	}
}

func (c *Client) GetFileSymbols(uri string) ([]DocumentSymbol, error) {
	return c.DocumentSymbols(uri)
}

func (c *Client) FileSymbols(uri string) ([]DocumentSymbol, error) {
	return c.DocumentSymbols(uri)
}

func (c *Client) Completion(uri string, line, char int) ([]CompletionItem, error) {
	params := CompletionParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     Position{Line: line, Character: char},
	}

	respChan := make(chan []byte, 1)
	id := c.SendRequest("textDocument/completion", params)

	c.pendingMu.Lock()
	if c.pending == nil {
		c.pending = make(map[int64]chan []byte)
	}
	c.pending[id] = respChan
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	select {
	case data := <-respChan:
		if len(data) == 0 || string(data) == "null" {
			return nil, nil
		}
		return decodeCompletionItems(data)
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("completion request timed out")
	}
}

func (c *Client) GetCompletions(uri string, line, char int) ([]CompletionItem, error) {
	return c.Completion(uri, line, char)
}

func decodeCompletionItems(data []byte) ([]CompletionItem, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}

	var list CompletionList
	if err := json.Unmarshal(data, &list); err == nil && len(list.Items) > 0 {
		return list.Items, nil
	}

	var items []CompletionItem
	if err := json.Unmarshal(data, &items); err == nil {
		return items, nil
	}

	var wrapped struct {
		Items []CompletionItem `json:"items"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && len(wrapped.Items) > 0 {
		return wrapped.Items, nil
	}

	return nil, fmt.Errorf("failed to parse completion response")
}

func (c *Client) WorkspaceSymbols(query string) ([]SymbolInformation, error) {
	params := WorkspaceSymbolParams{Query: query}

	respChan := make(chan []byte, 1)
	id := c.SendRequest("workspace/symbol", params)

	c.pendingMu.Lock()
	if c.pending == nil {
		c.pending = make(map[int64]chan []byte)
	}
	c.pending[id] = respChan
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	select {
	case data := <-respChan:
		if len(data) == 0 || string(data) == "null" {
			return nil, nil
		}

		var symbols []SymbolInformation
		if err := json.Unmarshal(data, &symbols); err == nil {
			return symbols, nil
		}

		var single SymbolInformation
		if err := json.Unmarshal(data, &single); err == nil && single.Name != "" {
			return []SymbolInformation{single}, nil
		}

		return nil, fmt.Errorf("failed to parse workspace symbols response")
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("workspace symbols request timed out")
	}
}

func (c *Client) GetWorkspaceSymbols(query string) ([]SymbolInformation, error) {
	return c.WorkspaceSymbols(query)
}

func decodeDocumentSymbols(data []byte) ([]DocumentSymbol, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(data, &items); err == nil && len(items) > 0 {
		if _, hasChildren := items[0]["children"]; hasChildren {
			var hier []DocumentSymbol
			if err := json.Unmarshal(data, &hier); err == nil {
				return hier, nil
			}
		}
		if _, hasLocation := items[0]["location"]; hasLocation {
			var flat []SymbolInformation
			if err := json.Unmarshal(data, &flat); err == nil {
				out := make([]DocumentSymbol, 0, len(flat))
				for _, s := range flat {
					selection := s.Location.Range
					out = append(out, DocumentSymbol{
						Name:           s.Name,
						Kind:           s.Kind,
						Deprecated:     s.Deprecated,
						Range:          s.Location.Range,
						SelectionRange: selection,
					})
				}
				return out, nil
			}
		}
	}

	var hier []DocumentSymbol
	if err := json.Unmarshal(data, &hier); err == nil {
		return hier, nil
	}

	var flat []SymbolInformation
	if err := json.Unmarshal(data, &flat); err == nil {
		out := make([]DocumentSymbol, 0, len(flat))
		for _, s := range flat {
			selection := s.Location.Range
			out = append(out, DocumentSymbol{
				Name:           s.Name,
				Kind:           s.Kind,
				Deprecated:     s.Deprecated,
				Range:          s.Location.Range,
				SelectionRange: selection,
			})
		}
		return out, nil
	}

	return nil, fmt.Errorf("failed to parse document symbols response")
}

func decodeWorkspaceSymbols(data []byte) ([]SymbolInformation, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}

	var symbols []SymbolInformation
	if err := json.Unmarshal(data, &symbols); err == nil {
		return symbols, nil
	}

	var single SymbolInformation
	if err := json.Unmarshal(data, &single); err == nil && single.Name != "" {
		return []SymbolInformation{single}, nil
	}

	return nil, fmt.Errorf("failed to parse workspace symbols response")
}

// CharToByteOffset converts UTF-16 code unit offset
// to byte index within a UTF-8 encoded line buffer.
func CharToByteOffset(line []byte, utf16Char int) int {
	if utf16Char <= 0 {
		return 0
	}

	byteIdx := 0
	utf16Count := 0

	for byteIdx < len(line) {
		r, size := utf8.DecodeRune(line[byteIdx:])
		if r == utf8.RuneError && size == 1 {
			byteIdx++
			utf16Count++
			continue
		}

		// Runes >= U+10000 require surrogate pairs in UTF-16 (2 units)
		needed := 1
		if r >= 0x10000 {
			needed = 2
		}

		if utf16Count+needed > utf16Char {
			break
		}

		utf16Count += needed
		byteIdx += size
	}

	return byteIdx
}

// CharFromByteOffset converts byte offset on a line to UTF-16 code units for LSP.
func CharFromByteOffset(line []byte, byteOffset int) int {
	if byteOffset <= 0 {
		return 0
	}
	utf16Count := 0
	currByte := 0

	for currByte < byteOffset && currByte < len(line) {
		r, size := utf8.DecodeRune(line[currByte:])
		if r == utf8.RuneError && size == 1 {
			currByte++
			utf16Count++
			continue
		}

		// Runes >= U+10000 require surrogate pairs (2 units) in UTF-16
		if r >= 0x10000 {
			utf16Count += 2
		} else {
			utf16Count++
		}
		currByte += size
	}
	return utf16Count
}

func (c *Client) GetSemanticTokens(uri string) (*SemanticTokens, error) {
	params := SemanticTokensParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
	}

	respChan := make(chan []byte, 1)
	id := c.SendRequestWithChan("textDocument/semanticTokens/full", params, respChan)

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	select {
	case data := <-respChan:
		if len(data) == 0 || string(data) == "null" {
			return nil, nil
		}

		var tokens SemanticTokens
		if err := json.Unmarshal(data, &tokens); err != nil {
			return nil, fmt.Errorf("failed to parse semantic tokens: %w", err)
		}
		return &tokens, nil

	case <-time.After(3 * time.Second):
		return nil, fmt.Errorf("semantic tokens request timed out")
	}
}
