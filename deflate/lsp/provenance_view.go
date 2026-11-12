package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jonjohnsonjr/targz/deflate"
)

type provenanceGroup struct {
	BlockID int
	Kind    deflate.OpKind
	Ops     []int
	Bytes   uint64
}

func provenanceGroups(trace *deflate.Trace, query deflate.OutRange) []provenanceGroup {
	var groups []provenanceGroup
	index := make(map[[2]int]int)
	for _, production := range trace.Producers(query) {
		key := [2]int{production.Op.BlockID, int(production.Op.Kind)}
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, provenanceGroup{BlockID: production.Op.BlockID, Kind: production.Op.Kind})
		}
		groups[i].Ops = append(groups[i].Ops, production.Op.ID)
		groups[i].Bytes += production.Intersection.Len()
	}
	return groups
}

func renderProvenanceDocument(meta metadata, trace *deflate.Trace) ([]byte, error) {
	metaValue, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	query := deflate.OutRange{Start: deflate.OutPos(meta.OutputStart), End: deflate.OutPos(meta.OutputEnd)}
	groups := provenanceGroups(trace, query)
	var out bytes.Buffer
	fmt.Fprintf(&out, "# provenance · %s · out[%d:%d]\n", meta.Entry, query.Start, query.End)
	lastBlock := -1
	for _, group := range groups {
		if group.BlockID != lastBlock {
			if lastBlock >= 0 {
				out.WriteByte('\n')
			}
			fmt.Fprintf(&out, "block %d\n", group.BlockID)
			lastBlock = group.BlockID
		}
		fmt.Fprintf(&out, "%s ×%d  # %d B\n", provenanceKind(group.Kind), len(group.Ops), group.Bytes)
	}
	fmt.Fprintf(&out, "\n%s%s\n", headerPrefix, metaValue)
	return out.Bytes(), nil
}

func provenanceKind(kind deflate.OpKind) string {
	switch kind {
	case deflate.LiteralOp:
		return "literals"
	case deflate.MatchOp:
		return "copies"
	case deflate.StoredOp:
		return "stored"
	default:
		return strings.ToLower(kind.String())
	}
}
