package replay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// sidecarString matches a sidecar reference as a whole JSON string,
// the form RFC 0001 gives an item's medium in a sidecar session.
var sidecarString = regexp.MustCompile(`"sidecar:(sha256:[0-9a-f]{64})"`)

// dataString matches a base64 data URL as a whole JSON string.
var dataString = regexp.MustCompile(`"data:[^",]*;base64,([A-Za-z0-9+/]*={0,2})"`)

// ResolveMedia returns items with each sidecar: reference among them
// resolved to a base64 data URL of the blob it names, read through br
// as the session holds it (agentsession.ReadSessionBlob), of the media
// type the session holds for the blob, or application/octet-stream
// for one held with none. A sidecar session's items name their media
// so, and no provider can fetch such a reference (RFC 0001, File), so
// an item taken from one is resolved before it is sent: a task's
// prompts, or what a replay serves. Items with no reference are
// returned as they are. A reference and no reader is
// session.ErrNoBlobReader; a blob the reader does not hold is its
// error.
func ResolveMedia(ctx context.Context, br agentsession.BlobReader, sessionID string, items openresponses.Items) (openresponses.Items, error) {
	r := &resolver{br: br, session: sessionID}
	out := make(openresponses.Items, len(items))
	for i, item := range items {
		conv, err := r.item(ctx, item)
		if err != nil {
			return nil, err
		}
		out[i] = conv
	}
	return out, nil
}

// resolver resolves a sidecar session's references, each blob read
// once.
type resolver struct {
	br      agentsession.BlobReader
	session string

	mu   sync.Mutex
	urls map[string]string
}

// newResolver returns the resolver for s, nil for a session that is
// not a sidecar session.
func newResolver(s *agentsession.Session, br agentsession.BlobReader) *resolver {
	if s.Header().Media != agentsession.MediaSidecar {
		return nil
	}
	return &resolver{br: br, session: s.ID()}
}

// item returns item with its references resolved, or item itself when
// it holds none or r is nil.
func (r *resolver) item(ctx context.Context, item openresponses.Item) (openresponses.Item, error) {
	if r == nil || item == nil {
		return item, nil
	}
	data, err := json.Marshal(item)
	if err != nil || !sidecarString.Match(data) {
		return item, nil
	}
	var failed error
	data = sidecarString.ReplaceAllFunc(data, func(m []byte) []byte {
		if failed != nil {
			return m
		}
		hash := string(sidecarString.FindSubmatch(m)[1])
		u, err := r.dataURL(ctx, hash)
		if err != nil {
			failed = err
			return m
		}
		quoted, _ := json.Marshal(u)
		return quoted
	})
	if failed != nil {
		return nil, failed
	}
	conv, err := openresponses.UnmarshalItem(data)
	if err != nil {
		return nil, fmt.Errorf("replay: resolve media in a %s item: %w", item.ItemType(), err)
	}
	return conv, nil
}

// items resolves each of items in place.
func (r *resolver) items(ctx context.Context, items openresponses.Items) error {
	for i, item := range items {
		conv, err := r.item(ctx, item)
		if err != nil {
			return err
		}
		items[i] = conv
	}
	return nil
}

// dataURL returns the data URL of the blob hash names.
func (r *resolver) dataURL(ctx context.Context, hash string) (string, error) {
	r.mu.Lock()
	u, ok := r.urls[hash]
	r.mu.Unlock()
	if ok {
		return u, nil
	}
	if r.br == nil {
		return "", fmt.Errorf("replay: %w: sidecar:%s in session %s", session.ErrNoBlobReader, hash, r.session)
	}
	data, mt, err := agentsession.ReadSessionBlob(ctx, r.br, r.session, hash)
	if err != nil {
		return "", fmt.Errorf("replay: sidecar:%s in session %s: %w", hash, r.session, err)
	}
	if mt == "" {
		mt = agentsession.DefaultMediaType
	}
	u = "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)
	r.mu.Lock()
	if r.urls == nil {
		r.urls = map[string]string{}
	}
	r.urls[hash] = u
	r.mu.Unlock()
	return u, nil
}

// requestHash is the hash a recorder writing the session gives req:
// over its canonical form, and in a sidecar session with each medium
// a blob of the session holds named by its sidecar: reference, which
// is the form the session records and hashes (RFC 0001, Request hash),
// so a replay that was served the media as data URLs sends what the
// record describes. named holds the session's blobs; it is nil for an
// inline session.
func requestHash(req openresponses.Request, named map[string]bool) (string, error) {
	req = session.Canonical(req)
	if len(named) == 0 {
		return agentsession.RequestHash(req)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("replay: encode request: %w", err)
	}
	if bytes.Contains(data, []byte(`"data:`)) {
		data = dataString.ReplaceAllFunc(data, func(m []byte) []byte {
			payload := dataString.FindSubmatch(m)[1]
			raw, err := base64.StdEncoding.DecodeString(string(payload))
			if err != nil {
				return m
			}
			hash := agentsession.HashBytes(raw)
			if !named[hash] {
				return m
			}
			return []byte(`"sidecar:` + hash + `"`)
		})
	}
	return agentsession.HashRequestJSON(data)
}

// namedBlobs returns the blobs a sidecar session names, nil for an
// inline one.
func namedBlobs(s *agentsession.Session) (map[string]bool, error) {
	if s.Header().Media != agentsession.MediaSidecar {
		return nil, nil
	}
	hashes, err := agentsession.SessionBlobs(s)
	if err != nil {
		return nil, fmt.Errorf("replay: the session's blobs: %w", err)
	}
	named := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		named[strings.TrimSpace(h)] = true
	}
	return named, nil
}
