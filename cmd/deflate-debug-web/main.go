// deflate-debug-web serves the interactive DEFLATE provenance debugger.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/jonjohnsonjr/targz/deflate"
	"github.com/jonjohnsonjr/targz/deflate/debugui"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "address on which to serve the debugger")
	raw := flag.Bool("raw", false, "parse raw DEFLATE instead of detecting gzip")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: deflate-debug-web [-raw] [-addr host:port] FILE")
		os.Exit(2)
	}

	data, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	var trace *deflate.Trace
	if !*raw && len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		trace, err = deflate.ParseGzip(data)
	} else {
		trace, err = deflate.ParseRaw(data)
	}
	if err != nil {
		log.Fatal(err)
	}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	url := "http://" + listener.Addr().String()
	log.Printf("parsed %d output bytes into %d operations", len(trace.Output), len(trace.Ops))
	log.Printf("DEFLATE debugger: %s", url)
	log.Fatal(http.Serve(listener, debugui.Handler(trace)))
}
