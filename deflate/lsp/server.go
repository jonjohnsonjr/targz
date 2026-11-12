package lsp

import (
	"bufio"
	"bytes"
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
	"unicode"
	"unicode/utf8"

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
		// Generated documents are registered before the editor follows their
		// definition link. Preserve the shared trace when the ensuing didOpen
		// echoes the same file back to us instead of parsing the archive again.
		if method == "textDocument/didOpen" {
			if existing, ok := s.docs[params.TextDocument.URI]; ok && existing.trace != nil {
				return nil, nil
			}
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
		if link, found, isRFC, err := s.rfcDefinitionAt(raw); err != nil {
			return nil, err
		} else if isRFC {
			if !found {
				return []locationLink{}, nil
			}
			return []locationLink{link}, nil
		}
		if doc, ids, origin, ok, err := s.provenanceAt(raw); err != nil {
			return nil, err
		} else if ok {
			traceDoc, traceErr := s.tarTraceDocument(doc)
			if traceErr != nil {
				return nil, traceErr
			}
			links := make([]locationLink, 0, len(ids))
			for _, id := range ids {
				links = append(links, locationLink{
					OriginSelectionRange: origin,
					TargetURI:            traceDoc.uri, TargetRange: traceDoc.opRange(id), TargetSelectionRange: traceDoc.tokens[id].Output,
				})
			}
			return links, nil
		}
		if doc, entry, ok, err := s.tarEntryAt(raw); err != nil {
			return nil, err
		} else if ok {
			at, positionErr := requestPosition(raw)
			if positionErr != nil {
				return nil, positionErr
			}
			link, found, linkErr := s.tarDefinition(doc, entry, at)
			if linkErr != nil {
				return nil, linkErr
			}
			if !found {
				return []locationLink{}, nil
			}
			return []locationLink{link}, nil
		}
		if doc, node, ok, err := s.encodingAt(raw); err != nil {
			return nil, err
		} else if ok {
			at, positionErr := requestPosition(raw)
			if positionErr != nil {
				return nil, positionErr
			}
			link, found, linkErr := s.encodingDefinition(doc, node, at)
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
			if containsPosition(tokens.LengthExtra, at) {
				if link, ok := doc.rfcLink(tokens.LengthExtra, lengthExtraRFCAnchor(int(op.LengthSymbol))); ok {
					return []locationLink{link}, nil
				}
				return []locationLink{}, nil
			}
			if containsPosition(tokens.DistanceExtra, at) {
				if link, ok := doc.rfcLink(tokens.DistanceExtra, distanceExtraRFCAnchor(int(op.DistanceSymbol))); ok {
					return []locationLink{link}, nil
				}
				return []locationLink{}, nil
			}
			if containsPosition(tokens.Length, at) {
				if link, ok, linkErr := s.encodingLink(doc, tokens.LengthSymbol, doc.lengthDefs[op.ID]); linkErr != nil {
					return nil, linkErr
				} else if ok {
					return []locationLink{link}, nil
				}
				return []locationLink{}, nil
			}
			if containsPosition(tokens.Distance, at) {
				if link, ok, linkErr := s.encodingLink(doc, tokens.DistanceSymbol, doc.distanceDefs[op.ID]); linkErr != nil {
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
		var params struct {
			Context struct {
				IncludeDeclaration bool `json:"includeDeclaration"`
			} `json:"context"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		if doc, entry, ok, err := s.tarEntryAt(raw); err != nil {
			return nil, err
		} else if ok {
			at, positionErr := requestPosition(raw)
			if positionErr != nil {
				return nil, positionErr
			}
			if containsPosition(entry.LinkToken, at) && entry.LinkName != "" {
				if target, found := doc.tarEntryByName(resolvedTarLink(entry)); found {
					return doc.tarReferences(target, params.Context.IncludeDeclaration), nil
				}
				return []location{}, nil
			}
			if !containsPosition(entry.NameToken, at) {
				return []location{}, nil
			}
			return doc.tarReferences(entry, params.Context.IncludeDeclaration), nil
		}
		if doc, node, ok, err := s.encodingAt(raw); err != nil {
			return nil, err
		} else if ok {
			return s.encodingReferences(doc, node, params.Context.IncludeDeclaration)
		}
		doc, op, err := s.operationAt(raw)
		if err != nil || op == nil {
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
		if doc, entry, ok, err := s.tarEntryAt(raw); err != nil {
			return nil, err
		} else if ok {
			at, positionErr := requestPosition(raw)
			if positionErr != nil {
				return nil, positionErr
			}
			if !containsPosition(entry.NameToken, at) && !containsPosition(entry.LinkToken, at) {
				return []locationLink{}, nil
			}
			if containsPosition(entry.LinkToken, at) && entry.LinkName != "" {
				if target, found := doc.tarEntryByName(resolvedTarLink(entry)); found {
					entry = target
				} else {
					return []locationLink{}, nil
				}
			}
			return s.tarImplementations(doc, entry)
		}
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
		if _, origin, anchorName, ok, err := s.rfcTermRequest(raw); err != nil {
			return nil, err
		} else if ok {
			return map[string]any{
				"contents": map[string]any{"kind": "markdown", "value": rfcHover(anchorName)},
				"range":    origin,
			}, nil
		}
		if doc, node, ok, err := s.encodingAt(raw); err != nil {
			return nil, err
		} else if ok {
			return map[string]any{
				"contents": map[string]any{"kind": "markdown", "value": doc.encodingHover(node)},
				"range":    doc.encodingSelection(node),
			}, nil
		}
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
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          lspRange         `json:"range"`
	SelectionRange lspRange         `json:"selectionRange"`
	Children       []documentSymbol `json:"children,omitempty"`
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

func (s *Server) rfcDefinitionAt(raw json.RawMessage) (locationLink, bool, bool, error) {
	var params struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position position `json:"position"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return locationLink{}, false, false, err
	}
	doc, ok := s.docs[params.TextDocument.URI]
	if !ok || doc.rfcLines == nil {
		return locationLink{}, false, false, nil
	}
	origin, anchorName, ok := doc.rfcTermAt(params.Position)
	if !ok {
		return locationLink{}, false, true, nil
	}
	anchor, ok := rfcAnchors[anchorName]
	if !ok || origin == anchor.Selection {
		return locationLink{}, false, true, nil
	}
	return locationLink{
		OriginSelectionRange: origin,
		TargetURI:            doc.uri,
		TargetRange:          anchor.TargetRange,
		TargetSelectionRange: anchor.Selection,
	}, true, true, nil
}

func (s *Server) rfcTermRequest(raw json.RawMessage) (*document, lspRange, string, bool, error) {
	var params struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position position `json:"position"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, lspRange{}, "", false, err
	}
	doc, ok := s.docs[params.TextDocument.URI]
	if !ok || doc.rfcLines == nil {
		return nil, lspRange{}, "", false, nil
	}
	origin, anchor, ok := doc.rfcTermAt(params.Position)
	return doc, origin, anchor, ok, nil
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

func (s *Server) provenanceAt(raw json.RawMessage) (*document, []int, lspRange, bool, error) {
	var params struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position position `json:"position"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, nil, lspRange{}, false, err
	}
	doc, ok := s.docs[params.TextDocument.URI]
	if !ok || doc.meta.View != "provenance" {
		return nil, nil, lspRange{}, false, nil
	}
	if doc.loadErr != nil {
		return nil, nil, lspRange{}, false, doc.loadErr
	}
	ids, ok := doc.provenance[params.Position.Line]
	if !ok {
		return doc, nil, lspRange{}, false, nil
	}
	origin := doc.lineRange(params.Position.Line)
	if !containsPosition(origin, params.Position) {
		return doc, nil, lspRange{}, false, nil
	}
	return doc, ids, origin, true, nil
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
	type symbolNode struct {
		symbol   documentSymbol
		children []*symbolNode
	}
	var roots []*symbolNode
	var stack []*symbolNode
	for _, line := range lines {
		entry := d.tar[line]
		r := d.lineRange(line)
		detail := fmt.Sprintf("%s · %d bytes · out[%d:%d]", entry.Kind, entry.Size, entry.Data.Start, entry.Data.End)
		if entry.LinkName != "" {
			detail += " → " + entry.LinkName
		}
		node := &symbolNode{symbol: documentSymbol{
			Name: entry.DisplayName, Detail: detail, Kind: entry.SymbolKind, Range: r, SelectionRange: entry.NameToken,
		}}
		if entry.Depth == 0 || len(stack) < entry.Depth {
			roots = append(roots, node)
		} else {
			parent := stack[entry.Depth-1]
			parent.children = append(parent.children, node)
		}
		if len(stack) > entry.Depth {
			stack = stack[:entry.Depth]
		}
		stack = append(stack, node)
	}
	var convert func(*symbolNode) documentSymbol
	convert = func(node *symbolNode) documentSymbol {
		result := node.symbol
		for _, child := range node.children {
			result.Children = append(result.Children, convert(child))
		}
		return result
	}
	symbols := make([]documentSymbol, 0, len(roots))
	for _, root := range roots {
		symbols = append(symbols, convert(root))
	}
	return symbols
}

func (d *document) tarHover(entry tarEntry) string {
	var b strings.Builder
	if entry.Synthetic {
		return fmt.Sprintf("**implicit tar directory** `%s/`", strings.TrimSuffix(entry.Name, "/"))
	}
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
	if d.trace != nil {
		producers := d.trace.Producers(query)
		if len(producers) != 0 {
			fmt.Fprintf(&b, "  \nDEFLATE provenance: `%d` producing operations (first is %%%d)", len(producers), producers[0].Op.ID)
		}
	}
	return b.String()
}

func (d *document) tarEntryByName(name string) (tarEntry, bool) {
	want := normalizedTarPath(name)
	lines := make([]int, 0, len(d.tar))
	for line := range d.tar {
		lines = append(lines, line)
	}
	sort.Ints(lines)
	for _, line := range lines {
		entry := d.tar[line]
		if !entry.Synthetic && normalizedTarPath(entry.Name) == want {
			return entry, true
		}
	}
	return tarEntry{}, false
}

func (d *document) tarReferences(target tarEntry, includeDeclaration bool) []location {
	locations := make([]location, 0)
	if includeDeclaration {
		locations = append(locations, location{URI: d.uri, Range: target.NameToken})
	}
	for _, entry := range d.tar {
		if entry.LinkName != "" && resolvedTarLink(entry) == normalizedTarPath(target.Name) {
			locations = append(locations, location{URI: d.uri, Range: entry.LinkToken})
		}
	}
	return locations
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

func (s *Server) encodingDefinition(d *document, node encodingNode, at position) (locationLink, bool, error) {
	tokens := d.encodingTok[node.Label]
	if node.Alphabet == "length-program" && containsPosition(tokens.Extra, at) {
		if node.BlockID >= 0 && node.BlockID < len(d.trace.Blocks) {
			block := d.trace.Blocks[node.BlockID]
			if block.Dynamic != nil && node.Symbol >= 0 && node.Symbol < len(block.Dynamic.LengthRuns) {
				anchor := codeLengthExtraRFCAnchor(block.Dynamic.LengthRuns[node.Symbol].Symbol)
				link, ok := d.rfcLink(tokens.Extra, anchor)
				return link, ok, nil
			}
		}
	}
	origin := d.encodingSelection(node)
	if node.Parent != "" {
		return s.encodingLink(d, origin, node.Parent)
	}
	link, ok := d.rfcLink(origin, node.RFCAnchor)
	return link, ok, nil
}

func (d *document) rfcLink(origin lspRange, anchorName string) (locationLink, bool) {
	anchor, ok := rfcAnchors[anchorName]
	if d.meta.RFC == "" || !ok {
		return locationLink{}, false
	}
	return locationLink{
		OriginSelectionRange: origin,
		TargetURI:            fileURI(d.meta.RFC),
		TargetRange:          anchor.TargetRange,
		TargetSelectionRange: anchor.Selection,
	}, true
}

func (s *Server) encodingReferences(d *document, node encodingNode, includeDeclaration bool) ([]location, error) {
	traceDoc := d
	if d.meta.TraceURI != "" {
		var ok bool
		traceDoc, ok = s.docs[d.meta.TraceURI]
		if !ok {
			return nil, fmt.Errorf("trace document is not open: %s", d.meta.TraceURI)
		}
	}
	locations := make([]location, 0)
	if includeDeclaration {
		locations = append(locations, location{URI: d.uri, Range: d.encodingSelection(node)})
	}
	for _, child := range buildEncodingGraph(traceDoc.trace).Nodes {
		if child.Parent != node.Label {
			continue
		}
		link, ok, err := s.encodingLink(traceDoc, lspRange{}, child.Label)
		if err != nil {
			return nil, err
		}
		if ok {
			locations = append(locations, location{URI: link.TargetURI, Range: link.TargetSelectionRange})
		}
	}
	return locations, nil
}

func (s *Server) tarDefinition(d *document, entry tarEntry, at position) (locationLink, bool, error) {
	if entry.Synthetic || (!containsPosition(entry.NameToken, at) && !containsPosition(entry.LinkToken, at)) {
		return locationLink{}, false, nil
	}
	if entry.LinkName != "" {
		target, ok := d.tarEntryByName(resolvedTarLink(entry))
		if !ok {
			return locationLink{}, false, nil
		}
		origin := entry.LinkToken
		if containsPosition(entry.NameToken, at) {
			origin = entry.NameToken
		}
		return locationLink{
			OriginSelectionRange: origin,
			TargetURI:            d.uri,
			TargetRange:          d.lineRange(target.Line),
			TargetSelectionRange: target.NameToken,
		}, true, nil
	}
	if entry.Kind != "file" || !containsPosition(entry.NameToken, at) {
		return locationLink{}, false, nil
	}
	uri, err := s.tarContentDocument(d, entry)
	if err != nil {
		return locationLink{}, false, err
	}
	return locationLink{
		OriginSelectionRange: entry.NameToken,
		TargetURI:            uri,
		TargetRange:          lspRange{},
		TargetSelectionRange: lspRange{},
	}, true, nil
}

func (s *Server) tarContentDocument(d *document, entry tarEntry) (string, error) {
	dir, err := s.archiveGeneratedRoot(d)
	if err != nil {
		return "", err
	}
	rel := generatedEntryPath(entry.Name)
	for _, candidate := range d.tar {
		if !candidate.Synthetic && candidate.Index < entry.Index && normalizedTarPath(candidate.Name) == normalizedTarPath(entry.Name) {
			rel += fmt.Sprintf("~%d", entry.Index)
			break
		}
	}
	path := filepath.Join(dir, rel)
	for _, candidate := range []string{path, path + ".hex"} {
		if _, err := os.Stat(candidate); err == nil {
			return fileURI(candidate), nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect extracted tar entry: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create extracted tar entry directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gzpls-entry-*")
	if err != nil {
		return "", fmt.Errorf("create extracted tar entry: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := copyTarEntry(d.meta.Source, d.meta.Raw, entry.Index, tmp); err != nil {
		tmp.Close()
		return "", fmt.Errorf("extract tar entry %s: %w", entry.Name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close extracted tar entry: %w", err)
	}
	text, err := isTextFile(tmpPath)
	if err != nil {
		return "", fmt.Errorf("classify extracted tar entry: %w", err)
	}
	if text {
		if err := os.Rename(tmpPath, path); err != nil {
			return "", fmt.Errorf("publish extracted tar entry: %w", err)
		}
	} else {
		path += ".hex"
		if err := renderHexFile(tmpPath, path); err != nil {
			return "", fmt.Errorf("render extracted tar entry: %w", err)
		}
	}
	if err := os.Chmod(path, 0o400); err != nil {
		return "", fmt.Errorf("make extracted tar entry read-only: %w", err)
	}
	return fileURI(path), nil
}

func isTextFile(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		value, size, err := r.ReadRune()
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if value == utf8.RuneError && size == 1 {
			return false, nil
		}
		if value != '\n' && value != '\r' && value != '\t' && !unicode.IsPrint(value) {
			return false, nil
		}
	}
}

func renderHexFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(target)
		}
	}()
	if err := renderHex(out, in); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func generatedEntryPath(name string) string {
	clean := normalizedTarPath(name)
	if clean == "" || clean == "." {
		clean = "entry"
	}
	return filepath.FromSlash(clean)
}

func isTextPayload(data []byte) bool {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	for _, r := range string(data) {
		if r != '\n' && r != '\r' && r != '\t' && !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func renderHexPayload(data []byte) []byte {
	var out bytes.Buffer
	_ = renderHex(&out, bytes.NewReader(data))
	return out.Bytes()
}

func renderHex(out io.Writer, in io.Reader) error {
	buf := make([]byte, 16)
	for offset := 0; ; offset += 16 {
		n, err := io.ReadFull(in, buf)
		if err == io.EOF {
			return nil
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			return err
		}
		if _, writeErr := fmt.Fprintf(out, "%08x: ", offset); writeErr != nil {
			return writeErr
		}
		for i := 0; i < 16; i++ {
			if i < n {
				if _, writeErr := fmt.Fprintf(out, "%02x ", buf[i]); writeErr != nil {
					return writeErr
				}
			} else {
				if _, writeErr := io.WriteString(out, "   "); writeErr != nil {
					return writeErr
				}
			}
		}
		if _, writeErr := io.WriteString(out, " |"); writeErr != nil {
			return writeErr
		}
		for _, value := range buf[:n] {
			if value >= 0x20 && value <= 0x7e {
				if _, writeErr := out.Write([]byte{value}); writeErr != nil {
					return writeErr
				}
			} else {
				if _, writeErr := io.WriteString(out, "."); writeErr != nil {
					return writeErr
				}
			}
		}
		if _, writeErr := io.WriteString(out, "|\n"); writeErr != nil {
			return writeErr
		}
		if err == io.ErrUnexpectedEOF {
			return nil
		}
	}
}

func (s *Server) tarImplementations(d *document, entry tarEntry) ([]locationLink, error) {
	if entry.Synthetic {
		return []locationLink{}, nil
	}
	if err := ensureTrace(d); err != nil {
		return nil, fmt.Errorf("load DEFLATE trace: %w", err)
	}
	query := entry.Data
	if query.Start == query.End {
		query = entry.Header
	}
	provenanceDoc, err := s.tarProvenanceDocument(d, entry, query)
	if err != nil {
		return nil, err
	}
	lines := make([]int, 0, len(provenanceDoc.provenance))
	for line := range provenanceDoc.provenance {
		lines = append(lines, line)
	}
	sort.Ints(lines)
	target := lspRange{}
	if len(lines) != 0 {
		target = provenanceDoc.lineRange(lines[0])
	}
	return []locationLink{{
		OriginSelectionRange: entry.NameToken,
		TargetURI:            provenanceDoc.uri, TargetRange: target, TargetSelectionRange: target,
	}}, nil
}

func (s *Server) tarProvenanceDocument(d *document, entry tarEntry, query deflate.OutRange) (*document, error) {
	root, err := s.archiveGeneratedRoot(d)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, ".gzpls", "provenance", generatedEntryPath(entry.Name)+".deflate")
	uri := fileURI(path)
	if existing, ok := s.docs[uri]; ok {
		return existing, nil
	}
	meta := metadata{
		Source: d.meta.Source, Raw: d.meta.Raw, View: "provenance", RFC: d.meta.RFC,
		Entry: entry.Name, OutputStart: uint64(query.Start), OutputEnd: uint64(query.End),
	}
	listing, err := renderProvenanceDocument(meta, d.trace)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create provenance directory: %w", err)
	}
	if err := os.WriteFile(path, listing, 0o600); err != nil {
		return nil, fmt.Errorf("write provenance document: %w", err)
	}
	doc := openDerivedDocument(uri, string(listing), d)
	if doc.loadErr != nil {
		return nil, fmt.Errorf("open provenance document: %w", doc.loadErr)
	}
	s.docs[uri] = doc
	return doc, nil
}

func (s *Server) tarTraceDocument(d *document) (*document, error) {
	dir, err := s.archiveGeneratedRoot(d)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".gzpls", "trace.deflate")
	uri := fileURI(path)
	if existing, ok := s.docs[uri]; ok {
		return existing, nil
	}
	listing, err := disassembleView(d.meta.Source, d.meta.Raw, d.trace, "trace")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create trace directory: %w", err)
	}
	if err := os.WriteFile(path, listing, 0o600); err != nil {
		return nil, fmt.Errorf("write generated trace: %w", err)
	}
	doc := openDerivedDocument(uri, string(listing), d)
	if doc.loadErr != nil {
		return nil, doc.loadErr
	}
	s.docs[uri] = doc
	return doc, nil
}

func (s *Server) archiveGeneratedRoot(d *document) (string, error) {
	dir, err := s.ensureGeneratedDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(dir, fmt.Sprintf("archive-%x", d.revision[:6]))
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create archive document root: %w", err)
	}
	return root, nil
}

func (s *Server) ensureGeneratedDir() (string, error) {
	if s.generatedDir == "" {
		dir, err := os.MkdirTemp("", "gzpls-generated-")
		if err != nil {
			return "", fmt.Errorf("create generated document directory: %w", err)
		}
		s.generatedDir = dir
	}
	return s.generatedDir, nil
}

func (s *Server) symbolDocument(traceDoc *document, node encodingNode) (*document, error) {
	dir, err := s.archiveGeneratedRoot(traceDoc)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".gzpls", fmt.Sprintf("block-%d-%s.gzsym", node.BlockID, node.Alphabet))
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create symbol table directory: %w", err)
	}
	if err := os.WriteFile(path, listing, 0o600); err != nil {
		return nil, fmt.Errorf("write symbol table: %w", err)
	}
	doc := openDerivedDocument(uri, string(listing), traceDoc)
	if doc.loadErr != nil {
		return nil, fmt.Errorf("open symbol table: %w", doc.loadErr)
	}
	s.docs[uri] = doc
	return doc, nil
}

func (d *document) encodingSelection(node encodingNode) lspRange {
	if tokens, ok := d.encodingTok[node.Label]; ok && tokens.Primary != (lspRange{}) {
		return tokens.Primary
	}
	end := strings.IndexAny(node.Text, " \t")
	if end < 0 {
		end = len(node.Text)
	}
	return lspRange{
		Start: position{Line: node.Line},
		End:   position{Line: node.Line, Character: end},
	}
}

func (d *document) encodingHover(node encodingNode) string {
	name := node.Name
	if name == "" {
		name = strings.TrimPrefix(node.Label, "@")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**  \nWire: `%s`", name, strings.TrimSpace(strings.SplitN(node.Text, "#", 2)[0]))
	if parts := strings.SplitN(node.Text, "#", 2); len(parts) == 2 {
		fmt.Fprintf(&b, "  \n%s", strings.TrimSpace(parts[1]))
	}
	if node.Parent != "" {
		fmt.Fprintf(&b, "  \nDefined by: `%s`", node.Parent)
	} else if node.RFCAnchor != "" {
		fmt.Fprintf(&b, "  \nTerminal definition: RFC 1951")
	}
	return b.String()
}

func rfcHover(anchorName string) string {
	name := strings.TrimPrefix(anchorName, "term-")
	name = strings.TrimPrefix(name, "section-")
	return fmt.Sprintf("**RFC 1951 · %s**  \nGo to Definition jumps to the canonical definition.", strings.ToUpper(name))
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
