package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jonjohnsonjr/targz/deflate"
)

// Server is a minimal LSP 3.17 server. It intentionally implements only the
// protocol needed to make DEFLATE call hierarchy work in ordinary editors.
type Server struct {
	in   *bufio.Reader
	out  io.Writer
	mu   sync.Mutex
	docs map[string]*document
}

// NewServer constructs a server which reads JSON-RPC messages from in and
// writes replies to out.
func NewServer(in io.Reader, out io.Writer) *Server {
	return &Server{in: bufio.NewReader(in), out: out, docs: make(map[string]*document)}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve processes requests until EOF or an exit notification.
func (s *Server) Serve() error {
	for {
		body, err := s.readMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			return fmt.Errorf("decode JSON-RPC request: %w", err)
		}
		if req.Method == "exit" {
			return nil
		}
		result, callErr := s.handle(req.Method, req.Params)
		if len(req.ID) == 0 {
			continue
		}
		var resp any
		if callErr != nil {
			resp = map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   &responseError{Code: -32602, Message: callErr.Error()},
			}
		} else {
			resp = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		}
		if err := s.writeMessage(resp); err != nil {
			return err
		}
	}
}

func (s *Server) handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{
			"capabilities": map[string]any{
				"textDocumentSync":       1,
				"callHierarchyProvider":  true,
				"definitionProvider":     true,
				"hoverProvider":          true,
				"implementationProvider": true,
				"referencesProvider":     true,
			},
			"serverInfo": map[string]any{"name": "gzpls", "version": "0.1.0"},
		}, nil
	case "initialized", "shutdown":
		return nil, nil
	case "textDocument/didOpen", "textDocument/didChange":
		var params struct {
			TextDocument struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		text := params.TextDocument.Text
		if len(params.ContentChanges) != 0 {
			text = params.ContentChanges[len(params.ContentChanges)-1].Text
		}
		s.docs[params.TextDocument.URI] = openDocument(params.TextDocument.URI, text)
		return nil, nil
	case "textDocument/didClose":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		delete(s.docs, params.TextDocument.URI)
		return nil, nil
	case "textDocument/prepareCallHierarchy":
		doc, op, err := s.operationAt(raw)
		if err != nil || op == nil {
			return nil, err
		}
		return []callHierarchyItem{doc.item(*op)}, nil
	case "textDocument/definition":
		doc, op, err := s.operationAt(raw)
		if err != nil || op == nil {
			return nil, err
		}
		seed, ok := op.Seed()
		if !ok {
			return []locationLink{}, nil
		}
		productions := doc.trace.Producers(seed)
		links := make([]locationLink, 0, len(productions))
		for _, production := range productions {
			links = append(links, doc.link(op.ID, production.Op.ID))
		}
		return links, nil
	case "textDocument/references":
		doc, op, err := s.operationAt(raw)
		if err != nil || op == nil {
			return nil, err
		}
		var params struct {
			Context struct {
				IncludeDeclaration bool `json:"includeDeclaration"`
			} `json:"context"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		uses := doc.trace.Uses(op.Output)
		locations := make([]location, 0, len(uses)+1)
		if params.Context.IncludeDeclaration {
			locations = append(locations, doc.location(op.ID))
		}
		for _, use := range uses {
			locations = append(locations, doc.location(use.Op.ID))
		}
		return locations, nil
	case "textDocument/implementation":
		doc, op, err := s.operationAt(raw)
		if err != nil || op == nil {
			return nil, err
		}
		ids, err := doc.implementations(*op)
		if err != nil {
			return nil, err
		}
		links := make([]locationLink, 0, len(ids))
		for _, id := range ids {
			links = append(links, doc.link(op.ID, id))
		}
		return links, nil
	case "callHierarchy/incomingCalls":
		doc, op, err := s.operationFromItem(raw)
		if err != nil {
			return nil, err
		}
		uses := doc.trace.Uses(op.Output)
		calls := make([]incomingCall, 0, len(uses))
		for _, use := range uses {
			calls = append(calls, incomingCall{
				From:       doc.item(use.Op),
				FromRanges: []lspRange{doc.opRange(use.Op.ID)},
			})
		}
		return calls, nil
	case "callHierarchy/outgoingCalls":
		doc, op, err := s.operationFromItem(raw)
		if err != nil {
			return nil, err
		}
		seed, ok := op.Seed()
		if !ok {
			return []outgoingCall{}, nil
		}
		productions := doc.trace.Producers(seed)
		calls := make([]outgoingCall, 0, len(productions))
		for _, production := range productions {
			calls = append(calls, outgoingCall{
				To:         doc.item(production.Op),
				FromRanges: []lspRange{doc.opRange(op.ID)},
			})
		}
		return calls, nil
	case "textDocument/hover":
		doc, op, err := s.operationAt(raw)
		if err != nil || op == nil {
			return nil, err
		}
		return map[string]any{
			"contents": map[string]any{"kind": "markdown", "value": doc.hover(*op)},
			"range":    doc.opRange(op.ID),
		}, nil
	default:
		return nil, fmt.Errorf("method not found: %s", method)
	}
}

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type location struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

type locationLink struct {
	OriginSelectionRange lspRange `json:"originSelectionRange,omitempty"`
	TargetURI            string   `json:"targetUri"`
	TargetRange          lspRange `json:"targetRange"`
	TargetSelectionRange lspRange `json:"targetSelectionRange"`
}

type itemData struct {
	URI string `json:"uri"`
	Op  int    `json:"op"`
}

type callHierarchyItem struct {
	Name           string   `json:"name"`
	Kind           int      `json:"kind"`
	Detail         string   `json:"detail,omitempty"`
	URI            string   `json:"uri"`
	Range          lspRange `json:"range"`
	SelectionRange lspRange `json:"selectionRange"`
	Data           itemData `json:"data"`
}

type incomingCall struct {
	From       callHierarchyItem `json:"from"`
	FromRanges []lspRange        `json:"fromRanges"`
}

type outgoingCall struct {
	To         callHierarchyItem `json:"to"`
	FromRanges []lspRange        `json:"fromRanges"`
}

func (s *Server) operationAt(raw json.RawMessage) (*document, *deflate.Op, error) {
	var params struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position position `json:"position"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, nil, err
	}
	doc, ok := s.docs[params.TextDocument.URI]
	if !ok {
		return nil, nil, fmt.Errorf("document is not open: %s", params.TextDocument.URI)
	}
	if doc.loadErr != nil {
		return nil, nil, doc.loadErr
	}
	id, ok := doc.lineOp[params.Position.Line]
	if !ok || id < 0 || id >= len(doc.trace.Ops) {
		return doc, nil, nil
	}
	op := doc.trace.Ops[id]
	return doc, &op, nil
}

func (s *Server) operationFromItem(raw json.RawMessage) (*document, deflate.Op, error) {
	var params struct {
		Item struct {
			Data itemData `json:"data"`
		} `json:"item"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, deflate.Op{}, err
	}
	doc, ok := s.docs[params.Item.Data.URI]
	if !ok {
		return nil, deflate.Op{}, fmt.Errorf("document is not open: %s", params.Item.Data.URI)
	}
	if doc.loadErr != nil {
		return nil, deflate.Op{}, doc.loadErr
	}
	if params.Item.Data.Op < 0 || params.Item.Data.Op >= len(doc.trace.Ops) {
		return nil, deflate.Op{}, fmt.Errorf("operation %d is outside trace", params.Item.Data.Op)
	}
	return doc, doc.trace.Ops[params.Item.Data.Op], nil
}

func (d *document) item(op deflate.Op) callHierarchyItem {
	name := fmt.Sprintf("%%%d %s", op.ID, op.Kind)
	kind := 14 // SymbolKind.Constant
	detail := fmt.Sprintf("out[%d:%d] · bits[%s:%s]", op.Output.Start, op.Output.End, op.Input.Start, op.Input.End)
	if op.Kind == deflate.MatchOp {
		kind = 12 // SymbolKind.Function, obviously.
		name = fmt.Sprintf("%%%d match len=%d dist=%d", op.ID, op.Length, op.Distance)
		if op.Length > op.Distance {
			detail += fmt.Sprintf(" · overlapping repetition ×%d", (op.Length+op.Distance-1)/op.Distance)
		}
	} else if op.Kind == deflate.LiteralOp {
		name = fmt.Sprintf("%%%d literal %s", op.ID, quoteByte(op.Literal))
	}
	r := d.opRange(op.ID)
	return callHierarchyItem{Name: name, Kind: kind, Detail: detail, URI: d.uri, Range: r, SelectionRange: r, Data: itemData{URI: d.uri, Op: op.ID}}
}

func (d *document) opRange(id int) lspRange {
	line := d.opLine[id]
	return lspRange{Start: position{Line: line}, End: position{Line: line, Character: d.lineLen[line]}}
}

func (d *document) location(id int) location {
	return location{URI: d.uri, Range: d.opRange(id)}
}

func (d *document) link(originID, targetID int) locationLink {
	origin := d.tokens[originID].Seed
	if origin == (lspRange{}) {
		origin = d.opRange(originID)
	}
	target := d.tokens[targetID].Output
	if target == (lspRange{}) {
		target = d.opRange(targetID)
	}
	return locationLink{
		OriginSelectionRange: origin,
		TargetURI:            d.uri,
		TargetRange:          d.opRange(targetID),
		TargetSelectionRange: target,
	}
}

// implementations returns the concrete literal and stored operations from
// which an operation's value originates. A DEFLATE distance is at most 32 KiB,
// so walking each byte in the normalized seed keeps this simple, exact, and
// bounded without expanding a potentially enormous match destination.
func (d *document) implementations(op deflate.Op) ([]int, error) {
	if op.Kind != deflate.MatchOp {
		return []int{op.ID}, nil
	}
	seed, _ := op.Seed()
	seen := make(map[int]struct{})
	for pos := seed.Start; pos < seed.End; pos++ {
		steps, err := d.trace.Origin(pos)
		if err != nil {
			return nil, err
		}
		seen[steps[len(steps)-1].Op.ID] = struct{}{}
	}
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids, nil
}

func (d *document) hover(op deflate.Op) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s %%%d**  \nOutput: `[%d:%d)`  \nCompressed bits: `[%s:%s)`", op.Kind, op.ID, op.Output.Start, op.Output.End, op.Input.Start, op.Input.End)
	if seed, ok := op.Seed(); ok {
		fmt.Fprintf(&b, "  \nSource seed: `[%d:%d)`", seed.Start, seed.End)
		if op.Length > op.Distance {
			fmt.Fprintf(&b, "  \nOverlapping copy: the %d-byte seed repeats %d times. The newly written bytes become source bytes during the same operation.", op.Distance, (op.Length+op.Distance-1)/op.Distance)
		}
	}
	return b.String()
}

func (s *Server) readMessage() ([]byte, error) {
	length := -1
	for {
		line, err := s.in.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length: %w", err)
			}
		}
	}
	if length < 0 {
		return nil, errors.New("missing Content-Length")
	}
	body := make([]byte, length)
	_, err := io.ReadFull(s.in, body)
	return body, err
}

func (s *Server) writeMessage(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.out, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = s.out.Write(body)
	return err
}
