package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/peer"
)

// uploadAttachmentToPeer streams a file from this host's upload directory to
// the ordinary upload endpoint on another kojo host. The returned path belongs
// to the destination host and is therefore safe to pass to its local backend.
func uploadAttachmentToPeer(ctx context.Context, baseURL string, src agent.MessageAttachment) (agent.MessageAttachment, error) {
	f, size, kind := openUploadPath(src.Path)
	if kind != "" {
		return agent.MessageAttachment{}, fmt.Errorf("open attachment %q: %s", filepath.Base(src.Name), kind)
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	writeDone := make(chan error, 1)
	go func() {
		defer f.Close()
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
			"name": "file", "filename": src.Name,
		}))
		contentType := src.Mime
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		h.Set("Content-Type", contentType)
		part, err := mw.CreatePart(h)
		if err == nil {
			var n int64
			n, err = io.Copy(part, io.LimitReader(f, size+1))
			if err == nil && n != size {
				err = io.ErrUnexpectedEOF
			}
		}
		if closeErr := mw.Close(); err == nil {
			err = closeErr
		}
		_ = pw.CloseWithError(err)
		writeDone <- err
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/upload", pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		<-writeDone
		return agent.MessageAttachment{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := peer.NoKeepAliveHTTPClient(0).Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		<-writeDone
		return agent.MessageAttachment{}, fmt.Errorf("upload attachment to holder: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A peer can reject before consuming the multipart body (auth, size,
		// switching). Close the reader before waiting for the writer so the
		// pipe goroutine cannot remain blocked forever.
		_ = pr.CloseWithError(fmt.Errorf("holder upload returned %s", resp.Status))
		<-writeDone
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return agent.MessageAttachment{}, fmt.Errorf("holder upload returned %s", resp.Status)
	}
	writeErr := <-writeDone
	if writeErr != nil {
		return agent.MessageAttachment{}, fmt.Errorf("stream attachment to holder: %w", writeErr)
	}
	var out agent.MessageAttachment
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return agent.MessageAttachment{}, fmt.Errorf("decode holder upload: %w", err)
	}
	if out.Path == "" {
		return agent.MessageAttachment{}, fmt.Errorf("holder upload returned an empty path")
	}
	return out, nil
}
