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
)

type Client struct {
	cmd           *exec.Cmd
	stdin         io.Writer
	reqID         int64
	OnDiagnostics func(uri string, diags []Diagnostic)
	mu            sync.Mutex

	nextID    int64
	pendingMu sync.Mutex
	pending   map[int64]chan []byte
}

type notification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
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

func (c *Client) SendRequest(method string, params interface{}) int64 {
	id := atomic.AddInt64(&c.nextID, 1)

	req := struct {
		JSONRPC string      `json:"jsonrpc"`
		ID      int64       `json:"id"`
		Method  string      `json:"method"`
		Params  interface{} `json:"params"`
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

	// Frame header + body
	frame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(data), data)
	c.stdin.Write([]byte(frame))

	return id
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

// func (c *Client) SendRequest(method string, params interface{}) int64 {
// 	id := atomic.AddInt64(&c.reqID, 1)
// 	msg := request{JSONRPC: "2.0", ID: id, Method: method, Params: params}
// 	c.write(msg)
// 	return id
// }

func (c *Client) SendNotification(method string, params interface{}) {
	msg := notification{JSONRPC: "2.0", Method: method, Params: params}
	c.write(msg)
}

func (c *Client) write(v interface{}) {
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
			if strings.HasPrefix(line, "Content-Length:") {
				lengthStr := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
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
				// Send JSON error or empty payload if request failed
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
