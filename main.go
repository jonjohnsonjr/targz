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

		fsys, err := tarfs.New(zr)
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

	fsys, err := tarfs.New(zr)
	if err != nil {
		return err
	}

	if len(args) == 1 {
		return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			fmt.Println(p)
			return nil
		})
	}

	return http.ListenAndServe(args[1], http.FileServer(http.FS(fsys)))
}
