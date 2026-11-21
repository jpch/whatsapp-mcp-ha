package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// mediaEncryptionAllowance covers AES-CBC padding and the HMAC that WhatsApp adds to encrypted media.
const mediaEncryptionAllowance = 64 << 10

var errMediaTooLarge = errors.New("media download exceeds the size limit")

// limitedTransport bounds media downloads. whatsmeow reads the whole response body into memory and the
// advertised file length comes from the sender, so the limit is enforced here: responses announcing more
// than max bytes are refused, and bodies are cut off with an error once they exceed max.
type limitedTransport struct {
	base http.RoundTripper
	max  int64
}

func newMediaHTTPClient(maxBytes int64) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Transport: &limitedTransport{base: base, max: maxBytes + mediaEncryptionAllowance},
		Timeout:   10 * time.Minute,
	}
}

func (t *limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || req.Method != http.MethodGet {
		return resp, err
	}
	if resp.ContentLength > t.max {
		resp.Body.Close()
		return nil, fmt.Errorf("%w (%d bytes announced)", errMediaTooLarge, resp.ContentLength)
	}
	resp.Body = &limitedBody{rc: resp.Body, remaining: t.max}
	return resp, nil
}

// limitedBody returns errMediaTooLarge as soon as more than the allowed bytes arrive.
type limitedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, errMediaTooLarge
	}
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return 0, errMediaTooLarge
	}
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
