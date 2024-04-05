package ranger

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// TODO: Consider an extension method that is like ReadAt but returns a reader of a given size.
// TODO: Consider probing with single byte size ranges for redirects (and a way to disable it).

type Reader struct {
	ctx context.Context
	rt  http.RoundTripper
	uri string
}

func New(ctx context.Context, uri string, rt http.RoundTripper) *Reader {
	return &Reader{
		ctx: ctx,
		rt:  rt,
		uri: uri,
	}
}

func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	req, err := http.NewRequestWithContext(r.ctx, "GET", r.uri, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1))

	res, err := r.rt.RoundTrip(req)
	if err != nil {
		return 0, err
	}

	// TODO: Consider just keeping this open if the response doesn't support range.
	// It can still be faster to discard the compressed parts and only decompress the portion we need.
	defer res.Body.Close()

	if res.StatusCode == http.StatusPartialContent {
		return io.ReadFull(res.Body, p)
	}

	redir := res.Header.Get("Location")
	if redir == "" || res.StatusCode/100 != 3 {
		return 0, fmt.Errorf("%q does not support range requests, saw status: %d", r.uri, res.StatusCode)
	}

	res.Body.Close()

	u, err := url.Parse(redir)
	if err != nil {
		return 0, err
	}

	r.uri = req.URL.ResolveReference(u).String()
	return r.ReadAt(p, off)
}
