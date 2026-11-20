package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfinementAndLimit(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	outside := filepath.Join(t.TempDir(), "secret")
	if err = os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(s.Dir, "in", "escape")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, filepath.Join(s.Dir, "in", "escape"), "relative", s.Dir} {
		if _, err = s.Inspect(p); err == nil {
			t.Fatalf("accepted %s", p)
		}
	}
	if _, err = s.Save(context.Background(), "x.txt", "", strings.NewReader("12345"), 4); err == nil {
		t.Fatal("accepted oversized file")
	}
	p, err := s.Save(context.Background(), "../../file.pdf", "application/pdf", strings.NewReader("ok"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if p.Size != 2 || filepath.Dir(p.Path) != filepath.Join(s.Dir, "in") {
		t.Fatal(p)
	}
	if err = os.Chtimes(p.Path, time.Now().Add(-25*time.Hour), time.Now().Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = s.Cleanup(time.Now(), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatal("old file retained")
	}
}
