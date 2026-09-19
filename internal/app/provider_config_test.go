package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

func TestTripoConfigFromEnv(t *testing.T) {
	t.Setenv("TRIPO_BASE_URL", "")
	c, err := ConfigFromEnv()
	if err != nil || c.TripoBaseURL != tripo.DefaultBaseURL {
		t.Fatal(c.TripoBaseURL, err)
	}
	t.Setenv("TRIPO_BASE_URL", "https://openapi.tripo3d.ai/v3/")
	c, err = ConfigFromEnv()
	if err != nil || c.TripoBaseURL != "https://openapi.tripo3d.ai/v3" {
		t.Fatal(c.TripoBaseURL, err)
	}
	c.DataDir = t.TempDir()
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.provider.(*tripo.Client).BaseURL != c.TripoBaseURL {
		t.Fatal("base not passed to provider")
	}
	t.Setenv("TRIPO_BASE_URL", "https://secret@other.example/v3")
	if _, err = ConfigFromEnv(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid config accepted or leaked", err)
	}
	c.DataDir = filepath.Join(t.TempDir(), "must-not-exist")
	c.TripoBaseURL = "https://other.example/v3"
	if _, err = New(c); err == nil {
		t.Fatal("New bypassed config validation")
	}
	if _, err = os.Stat(c.DataDir); !os.IsNotExist(err) {
		t.Fatal("invalid config touched runtime data")
	}
}
