package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type Client struct {
	cmd    *exec.Cmd
	stdin  io.Writer
	reqID  int64
	OnDiag func(uri string, diags []Diagnostic)
	mu     sync.Mutex
}

// Wire format structures
type request struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type notification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type serverMsg struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func StartClient(goplsPath string, onDiag func(string, []Diagnostic)) (*Client, error) {
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
		cmd:    cmd,
		stdin:  stdin,
		OnDiag: onDiag,
	}

	go c.readLoop(bufio.NewReader(stdout))
	return c, nil
}

func (c *Client) SendRequest(method string, params interface{}) int64 {
	id := atomic.AddInt64(&c.reqID, 1)
	msg := request{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	c.write(msg)
	return id
}

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

func (c *Client) readLoop(r *bufio.Reader) {
	for {
		// Read Content-Length header
		var contentLength int
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break // end of headers
			}
			if strings.HasPrefix(line, "Content-Length:") {
				parts := strings.Split(line, ":")
				if len(parts) == 2 {
					contentLength, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
				}
			}
		}

		if contentLength == 0 {
			continue
		}

		buf := make([]byte, contentLength)
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}

		var msg serverMsg
		if err := json.Unmarshal(buf, &msg); err == nil {
			if msg.Method == "textDocument/publishDiagnostics" {
				var params PublishDiagnosticsParams
				if err := json.Unmarshal(msg.Params, &params); err == nil && c.OnDiag != nil {
					c.OnDiag(params.URI, params.Diagnostics)
				}
			}
		}
	}
}

func (c *Client) Close() {
	c.cmd.Process.Kill()
}
