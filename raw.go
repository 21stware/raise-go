package raise

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RawResponse is the answer of a call whose body is raw bytes (App.Blobs.Get). Body streams the bytes; the caller reads it and closes
// it. The other fields are taken from the answer's headers.
type RawResponse struct {
	Body io.ReadCloser
	// ContentType is the Content-Type the platform sent ("" when none).
	ContentType string
	// ContentLength is the number of bytes, -1 when the platform did not say.
	ContentLength int64
	// LastModified is the Last-Modified time, zero when there is none.
	LastModified time.Time
	// ETag is the ETag header, "" when there is none.
	ETag string
	// Header is every header of the answer (X-Request-Id, the rate-limit headers…).
	Header http.Header
}

func newRawResponse(resp *http.Response) RawResponse {
	out := RawResponse{
		Body:          resp.Body,
		ContentType:   resp.Header.Get("Content-Type"),
		ContentLength: resp.ContentLength,
		ETag:          resp.Header.Get("ETag"),
		Header:        resp.Header,
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64); err == nil && n >= 0 {
		out.ContentLength = n
	}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		out.LastModified = t
	}
	return out
}

// rawBody is a request body of raw bytes, as the generated methods of a binary operation (App.Blobs.Put, App.Models.Transcribe) hand
// it to do. It is sent once; a reader that can seek is rewound to where it started for a retry or the one token refresh.
type rawBody struct {
	r           io.Reader
	size        int64 // -1: unknown, sent without a Content-Length
	contentType string
	seeker      io.Seeker
	start       int64
	sent        bool
	err         error
}

var errRawNotRewindable = errors.New("raise: the request body was already sent and its reader cannot seek back")

// newRawBody wraps r. A negative size is taken from r: its Len() (bytes.Reader, strings.Reader, bytes.Buffer) or the distance from its
// position to its end (os.File, any io.Seeker). An empty contentType is fallback.
func newRawBody(r io.Reader, size int64, contentType, fallback string) *rawBody {
	if contentType == "" {
		contentType = fallback
	}
	b := &rawBody{r: r, size: size, contentType: contentType}
	if r == nil {
		b.r, b.size = http.NoBody, 0
		return b
	}
	if s, ok := r.(io.Seeker); ok {
		if pos, err := s.Seek(0, io.SeekCurrent); err == nil {
			b.seeker, b.start = s, pos
		}
	}
	if b.size < 0 {
		if l, ok := r.(interface{ Len() int }); ok {
			b.size = int64(l.Len())
		} else if b.seeker != nil {
			end, err := b.seeker.Seek(0, io.SeekEnd)
			if err != nil {
				b.err = err
				return b
			}
			if _, err := b.seeker.Seek(b.start, io.SeekStart); err != nil {
				b.err = err
				return b
			}
			b.size = end - b.start
		} else {
			b.size = -1
		}
	}
	return b
}

// ready makes the body ready to be sent: the first time it is; again only when the reader can seek back to its start.
func (b *rawBody) ready() error {
	if b.err != nil {
		return b.err
	}
	if !b.sent {
		b.sent = true
		return nil
	}
	if b.seeker == nil {
		return errRawNotRewindable
	}
	_, err := b.seeker.Seek(b.start, io.SeekStart)
	return err
}

// apply puts the body on req. The reader is wrapped so that the HTTP client never closes the caller's reader (an os.File it rewinds).
func (b *rawBody) apply(req *http.Request) {
	switch {
	case b.size == 0:
		req.Body, req.ContentLength = http.NoBody, 0
	default:
		req.Body, req.ContentLength = io.NopCloser(b.r), b.size
	}
	req.GetBody = nil
	if b.contentType != "" {
		req.Header.Set("Content-Type", b.contentType)
	}
}
