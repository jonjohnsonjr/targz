// gzpls provides IDE-grade language intelligence for gzip and raw DEFLATE
// streams, a sentence which should not be examined too closely.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	deflatelsp "github.com/jonjohnsonjr/targz/deflate/lsp"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage:")
		fmt.Fprintln(os.Stderr, "  gzpls disassemble [-raw] [-view auto|trace|tar] FILE")
		fmt.Fprintln(os.Stderr, "  gzpls serve")
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	switch flag.Arg(0) {
	case "serve":
		if err := deflatelsp.NewServer(os.Stdin, os.Stdout).Serve(); err != nil {
			fatal(err)
		}
	case "disassemble":
		fs := flag.NewFlagSet("disassemble", flag.ExitOnError)
		raw := fs.Bool("raw", false, "parse raw DEFLATE instead of detecting gzip")
		view := fs.String("view", "auto", "render auto, trace, or tar view")
		progress := fs.Bool("progress", false, "emit JSON progress events on stderr")
		if err := fs.Parse(flag.Args()[1:]); err != nil {
			fatal(err)
		}
		if fs.NArg() != 1 {
			flag.Usage()
			os.Exit(2)
		}
		var report func(deflatelsp.ViewProgress)
		if *progress {
			encoder := json.NewEncoder(os.Stderr)
			report = func(event deflatelsp.ViewProgress) {
				_ = encoder.Encode(event)
			}
		}
		listing, err := deflatelsp.DisassembleFileViewProgress(fs.Arg(0), *raw, *view, report)
		if err != nil {
			fatal(err)
		}
		if _, err := os.Stdout.Write(listing); err != nil {
			fatal(err)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
