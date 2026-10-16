// SPDX-License-Identifier: MIT

package resultssvc

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite-api/gen/ermite"
	"connectrpc.com/connect"
)

// Blob operations: the first segment after BlobPath, and the first part of
// the signature.
const (
	opUpload   = "upload"
	opDownload = "download"
)

// Limits of the blob requests.
const (
	// chunkSize is the largest data of one message to ermite
	// (UploadJobArtifactBlockRequest.data, DownloadJobArtifactResponse.chunk),
	// and the one buffer a block upload holds.
	chunkSize = 1 << 20
	// maxBlockIDLen is the largest block id, decoded (Azure's limit).
	maxBlockIDLen = 64
	// maxBlockListBody bounds a Put Block List body: 50,000 ids of 64 bytes in
	// base64 with their tags is about 6 MiB.
	maxBlockListBody = 8 << 20
	// maxBlocks is the most ids a block list may name (Azure's limit, and
	// CommitJobArtifactRequest's).
	maxBlocks = 50000
	// putBlobBlockID is the id of the one block of a single-shot Put Blob.
	putBlobBlockID = "ermite-put-blob"
	// downloadTries is how many times a download stream that breaks is reopened
	// in a row without receiving a byte.
	downloadTries = 3
)

// blobSig signs a blob URL: base64url(BlobMAC(op, id, job, se)). The SDK sends
// no bearer token with blob requests (the URL is all it has), so the URL is the
// credential. It is bound to the operation, the artifact, the job of the
// channel and an expiry: a URL that leaks out of its sandbox is useless on the
// channel of any other job.
func (s *Service) blobSig(job Job, op string, id, se int64) string {
	return base64.RawURLEncoding.EncodeToString(s.auth.BlobMAC([]byte(op), []byte(strconv.FormatInt(id, 10)), []byte(job.ID), []byte(strconv.FormatInt(se, 10))))
}

// blobOK checks the signature and the expiry of a blob URL, in constant time.
func (s *Service) blobOK(job Job, op string, id int64, q url.Values) bool {
	if s.auth == nil {
		return false
	}
	se, err := strconv.ParseInt(q.Get("se"), 10, 64)
	if err != nil || s.now().Unix() >= se {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(q.Get("sig"))
	if err != nil {
		return false
	}
	want, _ := base64.RawURLEncoding.DecodeString(s.blobSig(job, op, id, se))
	return hmac.Equal(got, want)
}

// blob serves the Azure block blob subset that @azure/storage-blob uses on the
// signed URLs: Put Block, Put Block List and Put Blob on upload URLs, Get Blob
// on download URLs.
func (s *Service) blob(job Job, w http.ResponseWriter, r *http.Request, rest string) {
	op, idText, _ := strings.Cut(rest, "/")
	id, err := strconv.ParseInt(idText, 10, 64)
	if (op != opUpload && op != opDownload) || err != nil || id <= 0 {
		writeBlobError(w, http.StatusNotFound, "ResourceNotFound")
		return
	}
	q := r.URL.Query()
	if !s.blobOK(job, op, id, q) {
		writeBlobError(w, http.StatusForbidden, "AuthenticationFailed")
		return
	}
	comp := q.Get("comp")
	s.log.Debug().Str("method", r.Method).Str("op", op).Str("comp", comp).Int64("artifact", id).Str("job", job.ID).Msg("results blob")
	switch {
	case op == opUpload && r.Method == http.MethodPut && comp == "block":
		bid, err := base64.StdEncoding.DecodeString(q.Get("blockid"))
		if err != nil || len(bid) == 0 || len(bid) > maxBlockIDLen {
			writeBlobError(w, http.StatusBadRequest, "InvalidQueryParameterValue")
			return
		}
		if err := s.putBlock(r.Context(), job, id, bid, r.Body); err != nil {
			s.blobFailed(w, err, "InvalidBlobOrBlock", id)
			return
		}
		writeBlobCreated(w)
	case op == opUpload && r.Method == http.MethodPut && strings.EqualFold(comp, "blocklist"):
		ids, status, code := readBlockList(w, r)
		if code != "" {
			writeBlobError(w, status, code)
			return
		}
		if err := s.commit(r.Context(), job, id, ids); err != nil {
			s.blobFailed(w, err, "InvalidBlockList", id)
			return
		}
		writeBlobCreated(w)
	case op == opUpload && r.Method == http.MethodPut && comp == "":
		// Put Blob: the whole content in one request (the SDK's uploadData for
		// small buffers). It becomes one block, then a commit of that block.
		bid := []byte(putBlobBlockID)
		if err := s.putBlock(r.Context(), job, id, bid, r.Body); err != nil {
			s.blobFailed(w, err, "InvalidBlobOrBlock", id)
			return
		}
		if err := s.commit(r.Context(), job, id, [][]byte{bid}); err != nil {
			s.blobFailed(w, err, "InvalidBlockList", id)
			return
		}
		writeBlobCreated(w)
	case op == opDownload && r.Method == http.MethodGet && comp == "":
		s.download(job, w, r, id)
	default:
		writeBlobError(w, http.StatusMethodNotAllowed, "UnsupportedHttpVerb")
	}
}

// errBody is a failure to read the request body: the client's side, not
// ermite's.
type errBody struct{ err error }

func (e errBody) Error() string { return "reading the request body: " + e.err.Error() }
func (e errBody) Unwrap() error { return e.err }

// putBlock streams a request body to ermite as one block, in messages of at
// most chunkSize. Nothing is spooled: memory is the one buffer. A body that
// fails to read cancels the stream, so that ermite stores nothing of the block
// (a stream that breaks stores nothing; one that ends cleanly would store a
// short block).
func (s *Service) putBlock(ctx context.Context, job Job, id int64, blockID []byte, body io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	buf := make([]byte, chunkSize)
	var st *connect.ClientStreamForClient[ermite.UploadJobArtifactBlockRequest, ermite.UploadJobArtifactBlockResponse]
	for {
		n, rerr := io.ReadFull(body, buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			if st != nil {
				// Cancel first, then wait for the call to end without closing the
				// request side: closing it would end the stream cleanly.
				cancel()
				if conn, err := st.Conn(); err == nil {
					_ = conn.CloseResponse()
				}
			}
			return errBody{rerr}
		}
		if n > 0 || st == nil {
			msg := &ermite.UploadJobArtifactBlockRequest{Data: buf[:n]}
			if st == nil {
				// The stream opens on the first data read, so that a body that
				// fails at once never reaches ermite.
				st = job.Client.UploadJobArtifactBlock(ctx)
				msg.JobId, msg.ArtifactId, msg.BlockId = job.ID, id, blockID
			}
			if err := st.Send(msg); err != nil {
				// io.EOF: ermite ended the stream; its error comes with the
				// response.
				_, cerr := st.CloseAndReceive()
				if cerr == nil {
					cerr = err
				}
				return cerr
			}
		}
		if rerr != nil {
			break
		}
	}
	_, err := st.CloseAndReceive()
	return err
}

// commit sets the content of the artifact to the blocks named.
func (s *Service) commit(ctx context.Context, job Job, id int64, ids [][]byte) error {
	_, err := job.Client.CommitJobArtifact(ctx, connect.NewRequest(&ermite.CommitJobArtifactRequest{JobId: job.ID, ArtifactId: id, BlockIds: ids}))
	return err
}

// readBlockList reads a Put Block List body: <BlockList> with <Latest>,
// <Committed> or <Uncommitted> children holding base64 block ids. All three
// name an uploaded block here: there is no committed blob to take a block
// from. It returns a status and an Azure error code when the body is refused.
// It reads tokens rather than decoding into a struct, so that a list over
// maxBlocks is refused as soon as its id 50,001 opens.
func readBlockList(w http.ResponseWriter, r *http.Request) ([][]byte, int, string) {
	const (
		badXML   = "InvalidXmlDocument"
		badBlock = "InvalidBlockList"
	)
	dec := xml.NewDecoder(http.MaxBytesReader(w, r.Body, maxBlockListBody))
	var (
		ids   [][]byte
		text  []byte
		depth int
		root  bool
	)
	for {
		tok, err := dec.RawToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var mb *http.MaxBytesError
			if errors.As(err, &mb) {
				return nil, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge"
			}
			return nil, http.StatusBadRequest, badXML
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 1 && !root && t.Name.Local == "BlockList":
				root = true
			case depth == 2 && (t.Name.Local == "Latest" || t.Name.Local == "Committed" || t.Name.Local == "Uncommitted"):
				if len(ids) == maxBlocks {
					return nil, http.StatusBadRequest, badBlock
				}
				text = text[:0]
			default:
				return nil, http.StatusBadRequest, badXML
			}
		case xml.CharData:
			if depth == 2 {
				text = append(text, t...)
			}
		case xml.EndElement:
			if depth == 2 {
				id, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(text)))
				if err != nil || len(id) == 0 || len(id) > maxBlockIDLen {
					return nil, http.StatusBadRequest, badBlock
				}
				ids = append(ids, id)
			}
			depth--
		}
	}
	if !root || depth != 0 {
		return nil, http.StatusBadRequest, badXML
	}
	return ids, 0, ""
}

// download streams the content of an artifact. If ermite's stream breaks
// after the headers, it is reopened at the offset reached and the same
// response goes on; when that fails downloadTries times in a row the response
// is aborted, so the client sees a truncated body and not a clean end.
func (s *Service) download(job Job, w http.ResponseWriter, r *http.Request, id int64) {
	ctx := r.Context()
	open := func(off int64) (*connect.ServerStreamForClient[ermite.DownloadJobArtifactResponse], *ermite.DownloadJobArtifactResponse, error) {
		st, err := job.Client.DownloadJobArtifact(ctx, connect.NewRequest(&ermite.DownloadJobArtifactRequest{JobId: job.ID, ArtifactId: id, Offset: off}))
		if err != nil {
			return nil, nil, err
		}
		if !st.Receive() {
			err := st.Err()
			_ = st.Close()
			if err == nil {
				err = connect.NewError(connect.CodeUnavailable, errors.New("the download stream ended before its first message"))
			}
			return nil, nil, err
		}
		first := st.Msg()
		if first.GetArtifact() == nil || first.GetArtifact().GetId() != id {
			_ = st.Close()
			return nil, nil, connect.NewError(connect.CodeInternal, errors.New("the download stream does not describe the artifact"))
		}
		return st, first, nil
	}
	st, first, err := open(0)
	if err != nil {
		s.blobFailed(w, err, "InvalidQueryParameterValue", id)
		return
	}
	a := first.GetArtifact()
	size := a.GetSize()
	h := w.Header()
	mt := a.GetMediaType()
	if mt == "" {
		mt = "application/zip"
	}
	h.Set("Content-Type", mt)
	if mt != "application/zip" {
		// download-artifact v5+ names a file uploaded as is from it
		// (streamExtractExternal, download-artifact v8.0.2 dist/index.js:126608-126618).
		h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(a.GetName()))
	}
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	h.Set("x-ms-blob-type", "BlockBlob")
	if t := a.GetCreatedAt(); t != nil {
		h.Set("Last-Modified", t.AsTime().UTC().Format(http.TimeFormat))
	}
	w.WriteHeader(http.StatusOK)

	var off int64
	msg := first
	fails := 0
	for {
		for {
			c := msg.GetChunk()
			if off+int64(len(c)) > size {
				s.log.Warn().Int64("artifact", id).Str("job", job.ID).Msg("results: the download stream is longer than the artifact")
				_ = st.Close()
				panic(http.ErrAbortHandler)
			}
			if len(c) > 0 {
				if _, err := w.Write(c); err != nil {
					_ = st.Close() // the client is gone
					return
				}
				off += int64(len(c))
				fails = 0
			}
			if !st.Receive() {
				break
			}
			msg = st.Msg()
		}
		serr := st.Err()
		_ = st.Close()
		if off == size && serr == nil {
			return
		}
		if serr == nil {
			serr = fmt.Errorf("the stream ended at %d of %d bytes", off, size)
		}
		for {
			fails++
			if fails > downloadTries || ctx.Err() != nil {
				s.log.Warn().Int64("artifact", id).Str("job", job.ID).Int64("offset", off).Err(serr).Msg("results: download aborted")
				panic(http.ErrAbortHandler)
			}
			s.log.Debug().Int64("artifact", id).Int64("offset", off).Err(serr).Msg("results: download stream broke, reopening")
			st, msg, serr = open(off)
			if serr == nil {
				break
			}
		}
	}
}

// blobFailed answers an error of a blob request, from ermite or from the
// body. invalid is the code of a 400 (Azure's code for a bad block or block
// list).
func (s *Service) blobFailed(w http.ResponseWriter, err error, invalid string, id int64) {
	var be errBody
	if errors.As(err, &be) {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeBlobError(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge")
			return
		}
		s.log.Debug().Int64("artifact", id).Err(err).Msg("results: blob body")
		writeBlobError(w, http.StatusBadRequest, "IncompleteBody")
		return
	}
	code := connect.CodeOf(err)
	s.log.Warn().Int64("artifact", id).Str("code", code.String()).Err(err).Msg("results: ermite refused")
	switch code {
	case connect.CodeResourceExhausted:
		writeBlobError(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge")
	case connect.CodeNotFound:
		writeBlobError(w, http.StatusNotFound, "BlobNotFound")
	case connect.CodeFailedPrecondition, connect.CodeInvalidArgument:
		writeBlobError(w, http.StatusBadRequest, invalid)
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		writeBlobError(w, http.StatusForbidden, "AuthenticationFailed")
	default:
		writeBlobError(w, http.StatusServiceUnavailable, "ServerBusy")
	}
}

// writeBlobCreated is Azure's 201 to a Put Block, Put Block List or Put Blob,
// with the headers the SDK reads into its response object.
func writeBlobCreated(w http.ResponseWriter) {
	h := w.Header()
	now := time.Now().UTC()
	h.Set("ETag", fmt.Sprintf(`"0x%X"`, now.UnixNano()))
	h.Set("Last-Modified", now.Format(http.TimeFormat))
	h.Set("x-ms-request-id", strconv.FormatInt(now.UnixNano(), 16))
	h.Set("x-ms-version", "2025-11-05")
	h.Set("x-ms-request-server-encrypted", "false")
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// writeBlobError is an Azure storage error: the code in the XML body and in
// x-ms-error-code, which the SDK reads.
func writeBlobError(w http.ResponseWriter, status int, code string) {
	body := `<?xml version="1.0" encoding="utf-8"?><Error><Code>` + code + `</Code><Message>` + code + `</Message></Error>`
	h := w.Header()
	h.Set("Content-Type", "application/xml")
	h.Set("x-ms-error-code", code)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
