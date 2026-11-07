package workspace

import (
	"context"
	"errors"

	"kagari/internal/textpage"
)

// ReadFilePage reads the current workspace file and returns a versioned page.
func (w *Workspace) ReadFilePage(ctx context.Context, path string, req textpage.Request) (textpage.Page, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return textpage.Page{}, err
	}
	if w.closed {
		return textpage.Page{}, errors.New("workspace is closed")
	}
	return w.readFilePage(ctx, path, req)
}

func (w *Workspace) readFilePage(ctx context.Context, path string, req textpage.Request) (textpage.Page, error) {
	rel, err := cleanPath(path, false)
	if err != nil {
		return textpage.Page{}, err
	}
	content, err := w.readFile(rel)
	if err != nil {
		return textpage.Page{}, err
	}
	if err := ctx.Err(); err != nil {
		return textpage.Page{}, err
	}
	return textpage.Read(content, req, true)
}
