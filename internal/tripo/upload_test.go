package tripo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
)

func TestUploadModelAndDecimateToken(t *testing.T) {
	data := testfixture.Cube(1200)
	uploads, submits := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("missing authentication")
		}
		if r.URL.Path == "/files" {
			uploads++
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				http.Error(w, "bad", 400)
				return
			}
			defer file.Close()
			got, _ := io.ReadAll(file)
			if string(got) != string(data) || header.Filename != "model.glb" {
				t.Error("uploaded bytes changed")
			}
			io.WriteString(w, `{"code":0,"data":{"file_token":"file_test"}}`)
			return
		}
		if r.URL.Path != "/mesh/decimate" {
			t.Error("unexpected route")
		}
		submits++
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		if payload["input"] != "file_test" {
			t.Error("wrong input token")
		}
		io.WriteString(w, `{"code":0,"data":{"task_id":"task_test"}}`)
	}))
	defer server.Close()
	c := New("fixture")
	c.BaseURL = server.URL
	token, err := c.UploadModel(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Submit(context.Background(), "decimate", Params{Input: token, FaceLimit: 500})
	if err != nil || id != "task_test" || uploads != 1 || submits != 1 {
		t.Fatalf("protocol failed: %v", err)
	}
	if _, err = c.UploadModel(context.Background(), []byte("not a model")); err == nil || uploads != 1 {
		t.Fatal("invalid file was sent")
	}
}

func TestUploadLimitAndTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	c := New("fixture")
	c.BaseURL = server.URL
	large := make([]byte, UploadLimit+1)
	copy(large, "glTF")
	if _, err := c.UploadModel(context.Background(), large); err == nil {
		t.Fatal("oversized model accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := c.UploadModel(ctx, testfixture.Cube(12)); err == nil {
		t.Fatal("upload timeout not returned")
	}
}

func TestUploadFailureDoesNotSubmitProduction(t *testing.T) {
	for _, body := range []string{`{"code":0,"data":{}}`, `{"code":42,"data":{"file_token":"bad"}}`, `not-json`} {
		t.Run(body, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; io.WriteString(w, body) }))
			defer s.Close()
			c := New("fixture")
			c.BaseURL = s.URL
			if _, err := c.UploadModel(context.Background(), testfixture.Cube(12)); err == nil {
				t.Fatal("invalid response accepted")
			}
			if calls != 1 {
				t.Fatal("upload retried implicitly")
			}
		})
	}
}
