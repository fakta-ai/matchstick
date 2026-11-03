package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/fakta-ai/matchstick/internal/op"
)

// JSON-RPC 2.0 error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcServerError    = -32000
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int       `json:"code"`
	Message string    `json:"message"`
	Data    *op.Error `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ServeRPC answers JSON-RPC 2.0 requests, one per line, until r ends: the method is an operation's name, the params its
// input (without _meta), the result its output. Requests run concurrently; notifications/cancelled stops one.
func ServeRPC(ctx context.Context, r io.Reader, w io.Writer, tools []op.Tool, run Exec) error {
	known := map[string]bool{}
	for _, t := range tools {
		known[t.Name] = true
	}
	var (
		mu       sync.Mutex
		inFlight = map[string]context.CancelFunc{}
		wg       sync.WaitGroup
	)
	respond := func(resp rpcResponse) {
		resp.JSONRPC = "2.0"
		if resp.ID == nil {
			resp.ID = json.RawMessage("null")
		}
		b, _ := json.Marshal(resp)
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write(append(b, '\n'))
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			respond(rpcResponse{Error: &rpcError{Code: rpcParseError, Message: "parse error: " + err.Error()}})
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			respond(rpcResponse{ID: req.ID, Error: &rpcError{Code: rpcInvalidRequest, Message: "invalid request"}})
			continue
		}
		if req.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &p) == nil {
				mu.Lock()
				if cancel, ok := inFlight[string(p.RequestID)]; ok {
					cancel()
				}
				mu.Unlock()
			}
			continue
		}
		if !known[req.Method] {
			if req.ID != nil {
				respond(rpcResponse{ID: req.ID, Error: &rpcError{Code: rpcMethodNotFound,
					Message: "no operation " + req.Method}})
			}
			continue
		}
		params, err := withoutMeta(req.Params)
		if err != nil {
			respond(rpcResponse{ID: req.ID, Error: &rpcError{Code: rpcInvalidParams, Message: "params must be an object",
				Data: op.Errorf(op.CodeInvalidInput, "params must be an object")}})
			continue
		}
		callCtx, cancel := context.WithCancel(ctx)
		key := string(req.ID)
		if req.ID != nil {
			mu.Lock()
			inFlight[key] = cancel
			mu.Unlock()
		}
		wg.Go(func() {
			defer func() {
				cancel()
				mu.Lock()
				delete(inFlight, key)
				mu.Unlock()
			}()
			out, e := Call(callCtx, run, req.Method, params)
			if req.ID == nil {
				return
			}
			if e != nil {
				code := rpcServerError
				if e.Code == op.CodeInvalidInput {
					code = rpcInvalidParams
				}
				respond(rpcResponse{ID: req.ID, Error: &rpcError{Code: code, Message: e.Message, Data: e}})
				return
			}
			respond(rpcResponse{ID: req.ID, Result: out})
		})
	}
	wg.Wait()
	return scanner.Err()
}
