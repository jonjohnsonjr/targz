package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/jonjohnsonjr/targz/gsip"
	"github.com/jonjohnsonjr/targz/ranger"
	"github.com/jonjohnsonjr/targz/tarfs"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if strings.HasPrefix(args[0], "http") {
		resp, err := http.Head(args[0])
		if err != nil {
			return err
		}
		rra := ranger.New(context.TODO(), args[0], http.DefaultTransport)

		zr, err := gsip.NewReader(rra, resp.ContentLength)
		if err != nil {
			return err
		}

		fsys, err := tarfs.New(zr, resp.ContentLength)
		if err != nil {
			return err
		}

		return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			fmt.Println(p)
			return nil
		})
	}

	f, err := os.Open(args[0])
	if err != nil {
		return err
	}

	info, err := f.Stat()
	if err != nil {
		return err
	}

	zr, err := gsip.NewReader(f, info.Size())
	if err != nil {
		return err
	}

	// We don't know the size until we decompress it.
	fsys, err := tarfs.New(zr, 1<<63-1)
	if err != nil {
		return err
	}

	if len(args) == 1 {
		return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if p == "." {
				return nil
			}

			e, err := fsys.Entry(p)
			if err != nil {
				return fmt.Errorf("Entry(%q): %w", p, err)
			}
			fmt.Printf("%d+%d: %s\n", e.Offset, e.Size(), p)
			return nil
		})
	}

	return http.ListenAndServe(args[1], http.FileServer(http.FS(fsys)))
}
