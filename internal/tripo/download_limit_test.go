package tripo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

type limitTransport func(*http.Request) (*http.Response, error)

func (f limitTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestDownloadResourceLimits(t *testing.T) {
	for _, length := range []int64{DownloadLimit + 1, -1} {
		c := New("unused")
		c.DownloadHTTP = &http.Client{Transport: limitTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, ContentLength: length, Body: io.NopCloser(io.LimitReader(zeroReader{}, DownloadLimit+1))}, nil
		})}
		data, e := c.Download(context.Background(), "https://public.example/model.glb")
		if !errors.Is(e, ErrDownloadLimit) || data != nil {
			t.Fatal("protection failed", e)
		}
	}
}
