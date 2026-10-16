// SPDX-License-Identifier: MIT

package resultssvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeErmite is an in-memory JobArtifactService that follows the semantics of
// proto/ermite/artifact.proto: job tokens bound to a (run, job, attempt),
// pending, committed and finalized artifacts, blocks replaced on retry, commit
// order, finalize replacing an earlier attempt's artifact of the same name,
// lists by run, downloads from an offset in chunks of 1 MiB, and deletes. Its
// hooks inject failures.
type fakeErmite struct {
	ermiteconnect.UnimplementedJobArtifactServiceHandler

	mu     sync.Mutex
	jobs   map[string]fakeJob // by job token
	nextID int64
	arts   map[int64]*fakeArt
	now    time.Time

	// maxArtifact, when > 0, is the most bytes of blocks one artifact may hold:
	// beyond, UploadJobArtifactBlock fails with resource_exhausted.
	maxArtifact int64
	// failNext makes the next call of a procedure (by method name) fail.
	failNext map[string]error
	// cut, when set, is called before each chunk of a download stream with the
	// offset the stream was opened at and the bytes it has sent; an error ends
	// the stream.
	cut func(open, sent int64) error
	// blockStreams counts the UploadJobArtifactBlock calls that ended.
	blockStreams int
}

type fakeJob struct {
	run     int64
	job     string
	attempt int32
}

type artState int

const (
	pending artState = iota
	committed
	finalized
)

type fakeArt struct {
	a       *ermite.Artifact
	state   artState
	blocks  map[string][]byte
	list    [][]byte
	content []byte
}

func newFakeErmite() *fakeErmite {
	return &fakeErmite{jobs: map[string]fakeJob{}, arts: map[int64]*fakeArt{}, failNext: map[string]error{},
		now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
}

// serve starts the fake on an HTTP server and returns its URL.
func (f *fakeErmite) serve(t *testing.T) string {
	t.Helper()
	path, h := ermiteconnect.NewJobArtifactServiceHandler(f)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// addJob registers a job token.
func (f *fakeErmite) addJob(token string, run int64, job string, attempt int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[token] = fakeJob{run, job, attempt}
}

// bearer is an http.RoundTripper that adds a job token, as the runner's ermite
// client does.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// client is a JobArtifactService client of the fake with a job token.
func client(url, token string) ermiteconnect.JobArtifactServiceClient {
	return ermiteconnect.NewJobArtifactServiceClient(&http.Client{Transport: bearer{token, http.DefaultTransport}}, url)
}

func (f *fakeErmite) streams() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blockStreams
}

// caller checks the token and the job id of a request. f.mu must be held.
func (f *fakeErmite) caller(h http.Header, jobID, method string) (fakeJob, error) {
	if err := f.failNext[method]; err != nil {
		delete(f.failNext, method)
		return fakeJob{}, err
	}
	tok, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	j, known := f.jobs[tok]
	if !ok || !known {
		return fakeJob{}, connect.NewError(connect.CodeUnauthenticated, errors.New("unknown job token"))
	}
	if jobID != j.job {
		return fakeJob{}, connect.NewError(connect.CodePermissionDenied, errors.New("job_id is not the job of the token"))
	}
	return j, nil
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func (f *fakeErmite) CreateJobArtifact(_ context.Context, req *connect.Request[ermite.CreateJobArtifactRequest]) (*connect.Response[ermite.CreateJobArtifactResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, err := f.caller(req.Header(), req.Msg.GetJobId(), "CreateJobArtifact")
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	if name == "" || len(name) > 256 || strings.ContainsAny(name, "\":<>|*?\\/") {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid artifact name %q", name))
	}
	for id, a := range f.arts {
		if a.a.GetRunId() != j.run || a.a.GetName() != name {
			continue
		}
		if a.state == finalized && a.a.GetAttempt() == j.attempt {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New(msgExists))
		}
		if a.state != finalized && a.a.GetJobId() == j.job {
			delete(f.arts, id) // a new create is a new upload
		}
	}
	mt := req.Msg.GetMediaType()
	if mt == "" {
		mt = "application/zip"
	}
	exp := f.now.Add(90 * 24 * time.Hour)
	if e := req.Msg.GetExpiresAt(); e != nil && e.AsTime().After(f.now) {
		exp = e.AsTime()
	}
	f.nextID++
	f.arts[f.nextID] = &fakeArt{a: &ermite.Artifact{
		Id: f.nextID, Repo: "bnema/app", RunId: j.run, Attempt: j.attempt, JobId: j.job, Name: name, MediaType: mt,
		CreatedAt: timestamppb.New(f.now), ExpiresAt: timestamppb.New(exp),
	}, blocks: map[string][]byte{}}
	return connect.NewResponse(&ermite.CreateJobArtifactResponse{ArtifactId: f.nextID, ExpiresAt: timestamppb.New(exp)}), nil
}

func (f *fakeErmite) UploadJobArtifactBlock(_ context.Context, st *connect.ClientStream[ermite.UploadJobArtifactBlockRequest]) (*connect.Response[ermite.UploadJobArtifactBlockResponse], error) {
	defer func() {
		f.mu.Lock()
		f.blockStreams++
		f.mu.Unlock()
	}()
	if !st.Receive() {
		if err := st.Err(); err != nil {
			return nil, err
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("empty stream"))
	}
	first := st.Msg()
	f.mu.Lock()
	j, err := f.caller(st.RequestHeader(), first.GetJobId(), "UploadJobArtifactBlock")
	var art *fakeArt
	var others int64
	if err == nil {
		art = f.arts[first.GetArtifactId()]
		switch {
		case art == nil || art.a.GetJobId() != j.job || art.state == finalized:
			err = connect.NewError(connect.CodeNotFound, errors.New("not a pending artifact of this job"))
		case art.state == committed:
			err = connect.NewError(connect.CodeFailedPrecondition, errors.New("the artifact is committed"))
		case len(first.GetBlockId()) == 0 || len(first.GetBlockId()) > 64:
			err = connect.NewError(connect.CodeInvalidArgument, errors.New("block_id must be 1 to 64 bytes"))
		}
		if art != nil {
			for id, b := range art.blocks {
				if id != string(first.GetBlockId()) {
					others += int64(len(b))
				}
			}
		}
	}
	limit := f.maxArtifact
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var data bytes.Buffer
	msg := first
	for {
		if len(msg.GetData()) > chunkSize {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("data over 1 MiB"))
		}
		data.Write(msg.GetData())
		if limit > 0 && others+int64(data.Len()) > limit {
			return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("the artifact is over the size of one artifact (%d bytes)", limit))
		}
		if !st.Receive() {
			break
		}
		msg = st.Msg()
		if msg.GetJobId() != "" || msg.GetArtifactId() != 0 || len(msg.GetBlockId()) != 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ids in a message after the first"))
		}
	}
	if err := st.Err(); err != nil {
		return nil, err // a broken stream stores nothing
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if art.state != pending || f.arts[art.a.GetId()] != art {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the artifact is no longer pending"))
	}
	art.blocks[string(first.GetBlockId())] = data.Bytes()
	return connect.NewResponse(&ermite.UploadJobArtifactBlockResponse{Size: int64(data.Len())}), nil
}

func sameList(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func (f *fakeErmite) CommitJobArtifact(_ context.Context, req *connect.Request[ermite.CommitJobArtifactRequest]) (*connect.Response[ermite.CommitJobArtifactResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, err := f.caller(req.Header(), req.Msg.GetJobId(), "CommitJobArtifact")
	if err != nil {
		return nil, err
	}
	art := f.arts[req.Msg.GetArtifactId()]
	if art == nil || art.a.GetJobId() != j.job || art.state == finalized {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("not a pending artifact of this job"))
	}
	ids := req.Msg.GetBlockIds()
	if art.state == committed {
		if !sameList(ids, art.list) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("committed with another block list"))
		}
		return connect.NewResponse(&ermite.CommitJobArtifactResponse{Size: art.a.GetSize(), Digest: art.a.GetDigest()}), nil
	}
	if len(ids) > maxBlocks {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("more than 50,000 block ids"))
	}
	var content []byte
	for _, id := range ids {
		b, ok := art.blocks[string(id)]
		if !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("block %x was never uploaded", id))
		}
		content = append(content, b...)
	}
	art.content, art.list, art.blocks, art.state = content, ids, nil, committed
	art.a.Size, art.a.Digest = int64(len(content)), digestOf(content)
	return connect.NewResponse(&ermite.CommitJobArtifactResponse{Size: art.a.GetSize(), Digest: art.a.GetDigest()}), nil
}

func (f *fakeErmite) FinalizeJobArtifact(_ context.Context, req *connect.Request[ermite.FinalizeJobArtifactRequest]) (*connect.Response[ermite.FinalizeJobArtifactResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, err := f.caller(req.Header(), req.Msg.GetJobId(), "FinalizeJobArtifact")
	if err != nil {
		return nil, err
	}
	var art *fakeArt
	for _, a := range f.arts {
		if a.a.GetJobId() == j.job && a.a.GetRunId() == j.run && a.a.GetName() == req.Msg.GetName() && (art == nil || a.a.GetId() > art.a.GetId()) {
			art = a
		}
	}
	switch {
	case art == nil:
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no pending artifact of that name"))
	case art.state == finalized:
		return connect.NewResponse(&ermite.FinalizeJobArtifactResponse{Artifact: proto.CloneOf(art.a)}), nil
	case art.state == pending:
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the artifact is not committed"))
	case req.Msg.GetDigest() != "" && req.Msg.GetDigest() != art.a.GetDigest():
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("digest differs from the content"))
	}
	for id, a := range f.arts {
		if a == art || a.state != finalized || a.a.GetRunId() != j.run || a.a.GetName() != art.a.GetName() {
			continue
		}
		if a.a.GetAttempt() == j.attempt {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New(msgExists))
		}
		delete(f.arts, id) // a re-run replaces the earlier attempt's artifact
	}
	art.state = finalized
	return connect.NewResponse(&ermite.FinalizeJobArtifactResponse{Artifact: proto.CloneOf(art.a)}), nil
}

func (f *fakeErmite) ListJobArtifacts(_ context.Context, req *connect.Request[ermite.ListJobArtifactsRequest]) (*connect.Response[ermite.ListJobArtifactsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, err := f.caller(req.Header(), req.Msg.GetJobId(), "ListJobArtifacts")
	if err != nil {
		return nil, err
	}
	out := &ermite.ListJobArtifactsResponse{}
	for _, a := range f.arts {
		if a.state != finalized || a.a.GetRunId() != j.run ||
			(req.Msg.GetName() != "" && a.a.GetName() != req.Msg.GetName()) ||
			(req.Msg.GetArtifactId() != 0 && a.a.GetId() != req.Msg.GetArtifactId()) {
			continue
		}
		out.Artifacts = append(out.Artifacts, proto.CloneOf(a.a))
	}
	sort.Slice(out.Artifacts, func(i, k int) bool { return out.Artifacts[i].GetId() < out.Artifacts[k].GetId() })
	return connect.NewResponse(out), nil
}

func (f *fakeErmite) DownloadJobArtifact(_ context.Context, req *connect.Request[ermite.DownloadJobArtifactRequest], st *connect.ServerStream[ermite.DownloadJobArtifactResponse]) error {
	f.mu.Lock()
	j, err := f.caller(req.Header(), req.Msg.GetJobId(), "DownloadJobArtifact")
	var art *fakeArt
	if err == nil {
		art = f.arts[req.Msg.GetArtifactId()]
		if art == nil || art.state != finalized || art.a.GetRunId() != j.run {
			err = connect.NewError(connect.CodeNotFound, errors.New("artifact not found"))
		}
	}
	var meta *ermite.Artifact
	var content []byte
	if err == nil {
		meta, content = proto.CloneOf(art.a), art.content
	}
	cut := f.cut
	f.mu.Unlock()
	if err != nil {
		return err
	}
	off := req.Msg.GetOffset()
	if off < 0 || off > int64(len(content)) {
		return connect.NewError(connect.CodeOutOfRange, errors.New("offset beyond the size"))
	}
	if err := st.Send(&ermite.DownloadJobArtifactResponse{Artifact: meta}); err != nil {
		return err
	}
	var sent int64
	for p := content[off:]; len(p) > 0; {
		if cut != nil {
			if err := cut(off, sent); err != nil {
				return err
			}
		}
		n := min(len(p), chunkSize)
		if err := st.Send(&ermite.DownloadJobArtifactResponse{Chunk: p[:n]}); err != nil {
			return err
		}
		sent += int64(n)
		p = p[n:]
	}
	return nil
}

func (f *fakeErmite) DeleteJobArtifact(_ context.Context, req *connect.Request[ermite.DeleteJobArtifactRequest]) (*connect.Response[ermite.DeleteJobArtifactResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, err := f.caller(req.Header(), req.Msg.GetJobId(), "DeleteJobArtifact")
	if err != nil {
		return nil, err
	}
	for id, a := range f.arts {
		if a.state == finalized && a.a.GetRunId() == j.run && a.a.GetName() == req.Msg.GetName() {
			delete(f.arts, id)
			return connect.NewResponse(&ermite.DeleteJobArtifactResponse{ArtifactId: id}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("artifact not found"))
}

// content returns the content of a finalized or committed artifact.
func (f *fakeErmite) content(id int64) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.arts[id]
	if a == nil || a.state == pending {
		return nil, false
	}
	return a.content, true
}

// pendingBlocks returns the number of blocks a pending artifact holds.
func (f *fakeErmite) pendingBlocks(id int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.arts[id]; a != nil {
		return len(a.blocks)
	}
	return -1
}
