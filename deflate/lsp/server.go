package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jonjohnsonjr/targz/deflate"
)

// Server is a minimal LSP 3.17 server. It intentionally implements only the
// protocol needed to make DEFLATE call hierarchy work in ordinary editors.
type Server struct {
	in           *bufio.Reader
	out          io.Writer
	mu           sync.Mutex
	docs         map[string]*document
	generatedDir string
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
	defer func() {
		if s.generatedDir != "" {
			_ = os.RemoveAll(s.generatedDir)
		}
	}()
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
				"documentSymbolProvider": true,
				"hoverProvider":          true,
				"implementationProvider": true,
				"referencesProvider":     true,
				"renameProvider":         map[string]any{"prepareProvider": true},
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
	case "textDocument/documentSymbol":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		doc, ok := s.docs[params.TextDocument.URI]
		if !ok {
			return nil, fmt.Errorf("document is not open: %s", params.TextDocument.URI)
		}
		if doc.loadErr != nil {
			return nil, doc.loadErr
		}
		return doc.documentSymbols(), nil
	case "textDocument/prepareCallHierarchy":
		doc, op, err := s.operationAt(raw)
		if err != nil || op == nil {
			return nil, err
		}
		return []callHierarchyItem{doc.item(*op)}, nil
	case "textDocument/prepareRename":
		doc, op, err := s.literalAt(raw)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"range":       doc.tokens[op.ID].Literal,
			"placeholder": quoteByte(op.Literal),
		}, nil
	case "textDocument/rename", "gzpls/renameImpact":
		doc, op, err := s.literalAt(raw)
		if err != nil {
			return nil, err
		}
		var params struct {
			NewName string `json:"newName"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		replacement, err := parseRenameByte(params.NewName)
		if err != nil {
			return nil, err
		}
		if replacement == op.Literal {
			return nil, fmt.Errorf("literal is already %s", quoteByte(op.Literal))
		}
		impact, err := doc.trace.RenameImpact(op.ID, replacement)
		if err != nil {
			return nil, err
		}
		view := viewRenameImpact(impact)
		if method == "gzpls/renameImpact" {
			return view, nil
		}
		rewritten, impact, err := doc.trace.RewriteLiteral(op.ID, replacement)
		if err != nil {
			return nil, err
		}
		var nextTrace *deflate.Trace
		if doc.meta.Raw {
			nextTrace, err = deflate.ParseRaw(rewritten)
		} else {
			nextTrace, err = deflate.ParseGzip(rewritten)
		}
		if err != nil {
			return nil, fmt.Errorf("verify rewritten stream: %w", err)
		}
		renderView := doc.meta.View
		if renderView == "" {
			renderView = "trace"
		}
		listing, err := disassembleView(doc.meta.Source, doc.meta.Raw, nextTrace, renderView)
		if err != nil {
			return nil, err
		}
		oldSize := 0
		if info, statErr := os.Stat(doc.meta.Source); statErr == nil {
			oldSize = int(info.Size())
		}
		if err := replaceFile(doc.meta.Source, rewritten); err != nil {
			return nil, err
		}
		next := openDocument(doc.uri, string(listing))
		if next.loadErr != nil {
			return nil, fmt.Errorf("open rewritten document: %w", next.loadErr)
		}
		s.docs[doc.uri] = next
		message := renameAppliedMessage(impact, oldSize, len(rewritten))
		if err := s.writeMessage(map[string]any{
			"jsonrpc": "2.0",
			"method":  "window/showMessage",
			"params":  map[string]any{"type": 2, "message": message},
		}); err != nil {
			return nil, err
		}
		return map[string]any{"changes": map[string]any{
			doc.uri: []textEdit{{
				Range:   lspRange{Start: position{}, End: doc.end},
				NewText: string(listing),
			}},
		}}, nil
	case "textDocument/definition":
		if doc, node, ok, err := s.encodingAt(raw); err != nil {
			return nil, err
		} else if ok {
			link, found, linkErr := s.encodingDefinition(doc, node)
			if linkErr != nil {
				return nil, linkErr
			}
			if !found {
				return []locationLink{}, nil
			}
			return []locationLink{link}, nil
		}
		doc, op, err := s.operationAt(raw)
		if err != nil || op == nil {
			return nil, err
		}
		at, err := requestPosition(raw)
		if err != nil {
			return nil, err
		}
		if op.Kind == deflate.LiteralOp {
			if label := doc.literalDefs[op.ID]; label != "" {
				origin := doc.tokens[op.ID].Bits
				if containsPosition(doc.tokens[op.ID].Literal, at) {
					origin = doc.tokens[op.ID].Literal
				}
				if link, ok, linkErr := s.encodingLink(doc, origin, label); linkErr != nil {
					return nil, linkErr
				} else if ok {
					return []locationLink{link}, nil
				}
			}
			return []locationLink{}, nil
		}
		if op.Kind == deflate.MatchOp {
			tokens := doc.tokens[op.ID]
			if containsPosition(tokens.Length, at) {
				if link, ok, linkErr := s.encodingLink(doc, tokens.Length, doc.lengthDefs[op.ID]); linkErr != nil {
					return nil, linkErr
				} else if ok {
					return []locationLink{link}, nil
				}
				return []locationLink{}, nil
			}
			if containsPosition(tokens.Distance, at) {
				if link, ok, linkErr := s.encodingLink(doc, tokens.Distance, doc.distanceDefs[op.ID]); linkErr != nil {
					return nil, linkErr
				} else if ok {
					return []locationLink{link}, nil
				}
				return []locationLink{}, nil
			}
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
		if doc, entry, ok, err := s.tarEntryAt(raw); err != nil {
			return nil, err
		} else if ok {
			return map[string]any{
				"contents": map[string]any{"kind": "markdown", "value": doc.tarHover(entry)},
				"range":    doc.lineRange(entry.Line),
			}, nil
		}
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

type textEdit struct {
	Range   lspRange `json:"range"`
	NewText string   `json:"newText"`
}

type documentSymbol struct {
	Name           string   `json:"name"`
	Detail         string   `json:"detail,omitempty"`
	Kind           int      `json:"kind"`
	Range          lspRange `json:"range"`
	SelectionRange lspRange `json:"selectionRange"`
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

func requestPosition(raw json.RawMessage) (position, error) {
	var params struct {
		Position position `json:"position"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return position{}, err
	}
	return params.Position, nil
}

func containsPosition(r lspRange, p position) bool {
	return p.Line == r.Start.Line && p.Character >= r.Start.Character && p.Character < r.End.Character
}

func (s *Server) tarEntryAt(raw json.RawMessage) (*document, tarEntry, bool, error) {
	var params struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position position `json:"position"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, tarEntry{}, false, err
	}
	doc, ok := s.docs[params.TextDocument.URI]
	if !ok {
		return nil, tarEntry{}, false, fmt.Errorf("document is not open: %s", params.TextDocument.URI)
	}
	if doc.loadErr != nil {
		return nil, tarEntry{}, false, doc.loadErr
	}
	entry, ok := doc.tar[params.Position.Line]
	return doc, entry, ok, nil
}

func (s *Server) encodingAt(raw json.RawMessage) (*document, encodingNode, bool, error) {
	var params struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position position `json:"position"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, encodingNode{}, false, err
	}
	doc, ok := s.docs[params.TextDocument.URI]
	if !ok {
		return nil, encodingNode{}, false, fmt.Errorf("document is not open: %s", params.TextDocument.URI)
	}
	if doc.loadErr != nil {
		return nil, encodingNode{}, false, doc.loadErr
	}
	label, ok := doc.encodingLine[params.Position.Line]
	if !ok {
		return doc, encodingNode{}, false, nil
	}
	node, ok := doc.encoding[label]
	return doc, node, ok, nil
}

func (s *Server) literalAt(raw json.RawMessage) (*document, deflate.Op, error) {
	doc, op, err := s.operationAt(raw)
	if err != nil {
		return nil, deflate.Op{}, err
	}
	if op == nil || op.Kind != deflate.LiteralOp {
		return nil, deflate.Op{}, errors.New("rename is available only on literal bytes")
	}
	var params struct {
		Position position `json:"position"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, deflate.Op{}, err
	}
	r := doc.tokens[op.ID].Literal
	if params.Position.Line != r.Start.Line || params.Position.Character < r.Start.Character || params.Position.Character >= r.End.Character {
		return nil, deflate.Op{}, errors.New("place the cursor on the literal token to rename it")
	}
	return doc, *op, nil
}

type renameImpactView struct {
	Operation   int    `json:"operation"`
	Original    string `json:"original"`
	Replacement string `json:"replacement"`
	OutputBytes int    `json:"outputBytes"`
	Matches     int    `json:"matches"`
	Blocks      int    `json:"blocks"`
	Members     int    `json:"members"`
	FirstOutput uint64 `json:"firstOutput"`
	LastOutput  uint64 `json:"lastOutput"`
	DryRun      bool   `json:"dryRun"`
}

func viewRenameImpact(impact deflate.RenameImpact) renameImpactView {
	return renameImpactView{
		Operation: impact.OpID, Original: quoteByte(impact.Original), Replacement: quoteByte(impact.Replacement),
		OutputBytes: impact.OutputBytes, Matches: impact.Matches, Blocks: impact.Blocks, Members: impact.Members,
		FirstOutput: uint64(impact.FirstOutput), LastOutput: uint64(impact.LastOutput), DryRun: true,
	}
}

func renameImpactMessage(impact deflate.RenameImpact) string {
	checksum := "raw DEFLATE has no wrapper checksum"
	if impact.Members == 1 {
		checksum = "1 gzip CRC32 would need to be rewritten"
	} else if impact.Members > 1 {
		checksum = fmt.Sprintf("%d gzip CRC32 values would need to be rewritten", impact.Members)
	}
	return fmt.Sprintf(
		"Rename preview: %s → %s would affect %d output bytes through %d matches across %d blocks and %d gzip members (output %d…%d); %s. No edits applied: gzpls is not yet an assembler.",
		quoteByte(impact.Original), quoteByte(impact.Replacement), impact.OutputBytes, impact.Matches,
		impact.Blocks, impact.Members, impact.FirstOutput, impact.LastOutput, checksum,
	)
}

func renameAppliedMessage(impact deflate.RenameImpact, oldSize, newSize int) string {
	wrapper := "raw DEFLATE stream"
	if impact.Members == 1 {
		wrapper = "1 gzip member with a new CRC32"
	} else if impact.Members > 1 {
		wrapper = fmt.Sprintf("%d gzip members with new CRC32 values", impact.Members)
	}
	return fmt.Sprintf(
		"Renamed %s → %s across %d output bytes through %d matches; re-encoded %s (%d → %d compressed bytes).",
		quoteByte(impact.Original), quoteByte(impact.Replacement), impact.OutputBytes, impact.Matches,
		wrapper, oldSize, newSize,
	)
}

func replaceFile(path string, data []byte) (err error) {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat compressed source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("compressed source is not a regular file: %s", path)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".gzpls-*")
	if err != nil {
		return fmt.Errorf("create rewritten source: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(info.Mode().Perm()); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write rewritten source: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace compressed source: %w", err)
	}
	return nil
}

func parseRenameByte(name string) (byte, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("replacement must be one byte")
	}
	if strings.HasPrefix(name, "0x") || strings.HasPrefix(name, "0X") {
		value, err := strconv.ParseUint(name[2:], 16, 8)
		if err != nil {
			return 0, fmt.Errorf("invalid byte %q", name)
		}
		return byte(value), nil
	}
	if (name[0] == '\'' || name[0] == '"') && len(name) >= 2 {
		unquoted, err := strconv.Unquote(name)
		if err != nil {
			return 0, fmt.Errorf("invalid quoted byte %q: %w", name, err)
		}
		name = unquoted
	}
	runes := []rune(name)
	if len(runes) != 1 || runes[0] > 255 {
		return 0, fmt.Errorf("replacement %q is not exactly one byte", name)
	}
	return byte(runes[0]), nil
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

func (d *document) lineRange(line int) lspRange {
	return lspRange{Start: position{Line: line}, End: position{Line: line, Character: d.lineLen[line]}}
}

func (d *document) documentSymbols() []documentSymbol {
	if len(d.tar) == 0 {
		return []documentSymbol{}
	}
	lines := make([]int, 0, len(d.tar))
	for line := range d.tar {
		lines = append(lines, line)
	}
	sort.Ints(lines)
	symbols := make([]documentSymbol, 0, len(lines))
	for _, line := range lines {
		entry := d.tar[line]
		r := d.lineRange(line)
		detail := fmt.Sprintf("%s · %d bytes · out[%d:%d]", entry.Kind, entry.Size, entry.Data.Start, entry.Data.End)
		if entry.LinkName != "" {
			detail += " → " + entry.LinkName
		}
		symbols = append(symbols, documentSymbol{
			Name: entry.Name, Detail: detail, Kind: entry.SymbolKind, Range: r, SelectionRange: r,
		})
	}
	return symbols
}

func (d *document) tarHover(entry tarEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**tar %s** `%s`  \n", entry.Kind, entry.Name)
	fmt.Fprintf(&b, "Mode: `%s` · Size: `%d` bytes · Modified: `%s`  \n", entry.Mode, entry.Size, entry.Modified)
	fmt.Fprintf(&b, "Header: `out[%d:%d)` · Data: `out[%d:%d)`", entry.Header.Start, entry.Header.End, entry.Data.Start, entry.Data.End)
	if entry.LinkName != "" {
		fmt.Fprintf(&b, "  \nLink target: `%s`", entry.LinkName)
	}
	query := entry.Data
	if query.Start == query.End {
		query = entry.Header
	}
	producers := d.trace.Producers(query)
	if len(producers) != 0 {
		fmt.Fprintf(&b, "  \nDEFLATE provenance: `%d` producing operations (first is %%%d)", len(producers), producers[0].Op.ID)
	}
	return b.String()
}

func (d *document) location(id int) location {
	return location{URI: d.uri, Range: d.opRange(id)}
}

func (d *document) link(originID, targetID int) locationLink {
	origin := d.tokens[originID].Backref
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

func (s *Server) encodingLink(originDoc *document, origin lspRange, label string) (locationLink, bool, error) {
	targetDoc := originDoc
	target, ok := targetDoc.encoding[label]
	if !ok && originDoc.meta.TraceURI != "" {
		targetDoc, ok = s.docs[originDoc.meta.TraceURI]
		if ok {
			target, ok = targetDoc.encoding[label]
		}
	}
	if !ok {
		return locationLink{}, false, nil
	}
	if target.isSymbol() && targetDoc.meta.View != "symbols" {
		var err error
		targetDoc, err = s.symbolDocument(targetDoc, target)
		if err != nil {
			return locationLink{}, false, err
		}
		target, ok = targetDoc.encoding[label]
		if !ok {
			return locationLink{}, false, fmt.Errorf("generated symbol table is missing %s", label)
		}
	}
	return locationLink{
		OriginSelectionRange: origin,
		TargetURI:            targetDoc.uri,
		TargetRange:          targetDoc.lineRange(target.Line),
		TargetSelectionRange: targetDoc.encodingSelection(target),
	}, true, nil
}

func (s *Server) encodingDefinition(d *document, node encodingNode) (locationLink, bool, error) {
	origin := d.encodingSelection(node)
	if node.Parent != "" {
		return s.encodingLink(d, origin, node.Parent)
	}
	if d.meta.RFC == "" || node.RFCEnd <= node.RFCStart {
		return locationLink{}, false, nil
	}
	return locationLink{
		OriginSelectionRange: origin,
		TargetURI:            fileURI(d.meta.RFC),
		TargetRange: lspRange{
			Start: position{Line: node.RFCStart},
			End:   position{Line: node.RFCEnd},
		},
		TargetSelectionRange: lspRange{
			Start: position{Line: node.RFCSelect},
			End:   position{Line: node.RFCSelect + 1},
		},
	}, true, nil
}

func (s *Server) symbolDocument(traceDoc *document, node encodingNode) (*document, error) {
	if s.generatedDir == "" {
		dir, err := os.MkdirTemp("", "gzpls-symbols-")
		if err != nil {
			return nil, fmt.Errorf("create symbol table directory: %w", err)
		}
		s.generatedDir = dir
	}
	path := filepath.Join(s.generatedDir, fmt.Sprintf("block-%d-%s.gzsym", node.BlockID, node.Alphabet))
	uri := fileURI(path)
	if doc, ok := s.docs[uri]; ok {
		return doc, nil
	}
	meta := metadata{
		Source: traceDoc.meta.Source, Raw: traceDoc.meta.Raw, View: "symbols", RFC: traceDoc.meta.RFC,
		TraceURI: traceDoc.uri, Block: node.BlockID, Alphabet: node.Alphabet,
	}
	listing, err := renderSymbolDocument(meta, traceDoc.trace, node.BlockID, node.Alphabet)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, listing, 0o600); err != nil {
		return nil, fmt.Errorf("write symbol table: %w", err)
	}
	doc := openDocument(uri, string(listing))
	if doc.loadErr != nil {
		return nil, fmt.Errorf("open symbol table: %w", doc.loadErr)
	}
	s.docs[uri] = doc
	return doc, nil
}

func (d *document) encodingSelection(node encodingNode) lspRange {
	end := strings.IndexAny(node.Text, " \t")
	if end < 0 {
		end = len(node.Text)
	}
	return lspRange{
		Start: position{Line: node.Line},
		End:   position{Line: node.Line, Character: end},
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
