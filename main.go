package main

import (
	"io/fs"
	"log"
	"net/http"
	"os"

	"github.com/jonjohnsonjr/targz/gsip"
	"github.com/jonjohnsonjr/targz/tarfs"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
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
			log.Println(p)
			return nil
		})
	}

	return http.ListenAndServe(args[1], http.FileServer(http.FS(fsys)))
}
