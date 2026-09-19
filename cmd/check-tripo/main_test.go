package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

func TestCheckOutputAndExit(t *testing.T) {
	for _, ok := range []bool{true, false} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/v3/account/balance" {
				t.Error("unexpected request")
			}
			if ok {
				io.WriteString(w, `{"code":0,"data":{"balance":123456}}`)
			} else {
				w.WriteHeader(401)
				io.WriteString(w, `{"code":2,"message":"api-secret"}`)
			}
		}))
		c := tripo.New("api-secret")
		c.BaseURL = s.URL + "/v3"
		var out bytes.Buffer
		code := check(context.Background(), &out, c)
		s.Close()
		if (code == 0) != ok || strings.Contains(out.String(), "api-secret") || strings.Contains(out.String(), "123456") {
			t.Fatal(code, out.String())
		}
	}
}
