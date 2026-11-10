// deflate-debug prints a bit-exact DEFLATE instruction trace.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"unicode"

	"github.com/jonjohnsonjr/targz/deflate"
)

func main() {
	raw := flag.Bool("raw", false, "parse raw DEFLATE instead of detecting gzip")
	limit := flag.Int("limit", 200, "maximum operations to print; negative prints all")
	origin := flag.Int64("origin", -1, "trace one output byte back to its origin")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: deflate-debug [-raw] [-limit n] [-origin offset] FILE")
		os.Exit(2)
	}

	data, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fatal(err)
	}

	var trace *deflate.Trace
	if !*raw && len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		trace, err = deflate.ParseGzip(data)
	} else {
		trace, err = deflate.ParseRaw(data)
	}
	if err != nil {
		fatal(err)
	}

	for _, member := range trace.Members {
		fmt.Printf("member %d  c:%s-%s  u:%d-%d", member.ID, member.Input.Start, member.Input.End, member.Output.Start, member.Output.End)
		if member.Name != "" {
			fmt.Printf("  name=%q", member.Name)
		}
		fmt.Println()
	}
	for _, block := range trace.Blocks {
		fmt.Printf("block %d  %-7s final=%t  c:%s-%s  u:%d-%d  ops=%d\n",
			block.ID, block.Kind, block.Final,
			block.Input.Start, block.Input.End,
			block.Output.Start, block.Output.End, block.NumOps)
	}

	printed := 0
	for _, op := range trace.Ops {
		if *limit >= 0 && printed >= *limit {
			fmt.Printf("... %d operations omitted\n", len(trace.Ops)-printed)
			break
		}
		switch op.Kind {
		case deflate.LiteralOp:
			fmt.Printf("%%%d  u:%d  c:%s-%s  literal  0x%02x %s\n",
				op.ID, op.Output.Start, op.Input.Start, op.Input.End,
				op.Literal, quoteByte(op.Literal))
		case deflate.StoredOp:
			fmt.Printf("%%%d  u:%d-%d  c:%s-%s  stored  len=%d\n",
				op.ID, op.Output.Start, op.Output.End,
				op.Input.Start, op.Input.End, op.Length)
		case deflate.MatchOp:
			seed, _ := op.Seed()
			fmt.Printf("%%%d  u:%d-%d  c:%s-%s  match   len=%d dist=%d seed=out[%d:%d]\n",
				op.ID, op.Output.Start, op.Output.End,
				op.Input.Start, op.Input.End,
				op.Length, op.Distance, seed.Start, seed.End)
		}
		printed++
	}

	if *origin >= 0 {
		steps, err := trace.Origin(deflate.OutPos(*origin))
		if err != nil {
			fatal(err)
		}
		fmt.Printf("\norigin of output[%d]=0x%02x %s\n", *origin, trace.Output[*origin], quoteByte(trace.Output[*origin]))
		for _, step := range steps {
			switch step.Op.Kind {
			case deflate.MatchOp:
				fmt.Printf("  out[%d] <- %%%d match len=%d dist=%d; direct out[%d], seed out[%d]\n",
					step.Output, step.Op.ID, step.Op.Length, step.Op.Distance,
					step.DirectSource, step.SeedSource)
			case deflate.LiteralOp:
				fmt.Printf("  out[%d] <- %%%d literal %s from c:%s-%s\n",
					step.Output, step.Op.ID, quoteByte(step.Op.Literal),
					step.Op.Input.Start, step.Op.Input.End)
			case deflate.StoredOp:
				fmt.Printf("  out[%d] <- %%%d stored byte from c:%s-%s\n",
					step.Output, step.Op.ID,
					step.StoredInput.Start, step.StoredInput.End)
			}
		}
	}
}

func quoteByte(b byte) string {
	if unicode.IsPrint(rune(b)) {
		return strconv.QuoteRune(rune(b))
	}
	return strconv.QuoteRuneToASCII(rune(b))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
