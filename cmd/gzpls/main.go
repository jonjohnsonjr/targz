// gzpls provides IDE-grade language intelligence for gzip and raw DEFLATE
// streams, a sentence which should not be examined too closely.
package main

import (
	"flag"
	"fmt"
	"os"

	deflatelsp "github.com/jonjohnsonjr/targz/deflate/lsp"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage:")
		fmt.Fprintln(os.Stderr, "  gzpls disassemble [-raw] FILE")
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
		if err := fs.Parse(flag.Args()[1:]); err != nil {
			fatal(err)
		}
		if fs.NArg() != 1 {
			flag.Usage()
			os.Exit(2)
		}
		listing, err := deflatelsp.DisassembleFile(fs.Arg(0), *raw)
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
