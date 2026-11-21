package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestMediaHTTPClientLimits(t *testing.T) {
	const max = 1 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size := map[string]int{"/ok": max, "/big": max + mediaEncryptionAllowance + 1, "/chunked": 3 * max}[r.URL.Path]
		if r.URL.Path == "/chunked" {
			// No Content-Length: the limit must hold while reading.
			for i := 0; i < size/4096; i++ {
				w.Write(make([]byte, 4096))
				w.(http.Flusher).Flush()
			}
			return
		}
		if r.Method == http.MethodPost {
			io.Copy(io.Discard, r.Body)
		}
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.Write(make([]byte, size))
	}))
	defer srv.Close()
	c := newMediaHTTPClient(max)

	resp, err := c.Get(srv.URL + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || len(data) != max {
		t.Fatalf("within limit: %d %v", len(data), err)
	}

	if _, err := c.Get(srv.URL + "/big"); err == nil || !errors.Is(err, errMediaTooLarge) {
		t.Fatalf("announced oversize: %v", err)
	}

	resp, err = c.Get(srv.URL + "/chunked")
	if err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !errors.Is(err, errMediaTooLarge) || int64(len(data)) > max+mediaEncryptionAllowance {
		t.Fatalf("streamed oversize: read %d, err %v", len(data), err)
	}

	// Uploads (POST) are not limited by this transport.
	resp, err = c.Post(srv.URL+"/big", "application/octet-stream", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("POST limited: %v", err)
	}
	resp.Body.Close()
}
