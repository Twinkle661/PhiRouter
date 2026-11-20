package media

import (
	"PhiRouter/internal/core"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Store struct {
	Dir  string
	root *os.Root
}

func New(dir string) (*Store, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"in", "out"} {
		if err = os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Store{dir, root}, nil
}
func (s *Store) Close() error { return s.root.Close() }
func (s *Store) relative(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("attachment path must be absolute")
	}
	rel, err := filepath.Rel(s.Dir, filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("attachment must be inside media directory")
	}
	return rel, nil
}
func (s *Store) Open(path string) (io.ReadCloser, error) {
	rel, err := s.relative(path)
	if err != nil {
		return nil, err
	}
	return s.root.Open(rel)
}
func (s *Store) Inspect(path string) (core.Part, error) {
	rel, err := s.relative(path)
	if err != nil {
		return core.Part{}, err
	}
	f, err := s.root.Open(rel)
	if err != nil {
		return core.Part{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return core.Part{}, err
	}
	if !st.Mode().IsRegular() {
		return core.Part{}, fmt.Errorf("attachment must be a regular file")
	}
	mt := mime.TypeByExtension(filepath.Ext(path))
	if mt == "" {
		mt = "application/octet-stream"
	}
	mt = strings.Split(mt, ";")[0]
	kind := core.PartFile
	if strings.HasPrefix(mt, "image/") {
		kind = core.PartImage
	}
	if strings.HasPrefix(mt, "audio/") {
		kind = core.PartAudio
	}
	return core.Part{Type: kind, Path: filepath.Join(s.Dir, rel), MIME: mt, Size: st.Size()}, nil
}
func (s *Store) Save(ctx context.Context, name, mt string, r io.Reader, limit int64) (core.Part, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return core.Part{}, err
	}
	id[6] = (id[6] & 15) | 64
	id[8] = (id[8] & 63) | 128
	ext := filepath.Ext(filepath.Base(name))
	if len(ext) > 16 || strings.ContainsAny(ext, "/\\") {
		ext = ""
	}
	rel := fmt.Sprintf("in/%x-%x-%x-%x-%x%s", id[:4], id[4:6], id[6:8], id[8:10], id[10:], ext)
	f, err := s.root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return core.Part{}, err
	}
	n, err := io.Copy(f, io.LimitReader(&contextReader{ctx, r}, limit+1))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if n > limit {
		err = fmt.Errorf("file exceeds download limit")
	}
	if err != nil {
		_ = s.root.Remove(rel)
		return core.Part{}, err
	}
	p, err := s.Inspect(filepath.Join(s.Dir, rel))
	if mt != "" {
		p.MIME = mt
	}
	return p, err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func (s *Store) Cleanup(now time.Time, ttl time.Duration) error {
	for _, dir := range []string{"in", "out"} {
		err := fs.WalkDir(s.root.FS(), dir, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if e.IsDir() {
				return nil
			}
			st, err := e.Info()
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if now.Sub(st.ModTime()) >= ttl {
				if err = s.root.Remove(path); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) Validate(e core.Envelope) (core.Envelope, error) {
	if len(e.Parts) == 0 {
		return e, fmt.Errorf("parts must not be empty")
	}
	for i, p := range e.Parts {
		switch p.Type {
		case core.PartText:
			if p.Path != "" {
				return e, fmt.Errorf("text part cannot contain a path")
			}
		case core.PartImage, core.PartAudio, core.PartFile:
			q, err := s.Inspect(p.Path)
			if err != nil {
				return e, err
			}
			q.Type = p.Type
			q.Caption = p.Caption
			e.Parts[i] = q
		default:
			return e, fmt.Errorf("unknown part type %q", p.Type)
		}
	}
	return e, nil
}
