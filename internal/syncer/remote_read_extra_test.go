package syncer

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

func TestSessionLayoutRejectsInvalidIdentifiers(t *testing.T) {
	for _, values := range [][2]string{
		{"", "session"},
		{"project", ""},
		{"project/name", "session"},
		{"project", "session\\name"},
	} {
		if _, err := NewSessionLayout(values[0], values[1]); err == nil {
			t.Errorf("NewSessionLayout(%q) unexpectedly succeeded", values)
		}
	}
	if _, err := (SessionLayout{projectID: "", sessionID: "s"}).Prefix(); err == nil {
		t.Fatal("invalid project in SessionLayout unexpectedly succeeded")
	}
	if _, err := (SessionLayout{projectID: "p", sessionID: ""}).Prefix(); err == nil {
		t.Fatal("invalid session in SessionLayout unexpectedly succeeded")
	}
}

func TestFetchBranchesReportsListAndGetErrors(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	store := &remoteReadFake{objects: map[string][]byte{}, listErr: errors.New("list failed")}
	if _, err := FetchBranches(context.Background(), store, "project", "session", private); err == nil || !strings.Contains(err.Error(), "list failed") {
		t.Fatalf("list error = %v", err)
	}
	store.listErr = nil
	store.getErr = errors.New("get failed")
	key := "v1/projects/project/sessions/session/device/000001"
	store.list = []remote.ObjectInfo{{Key: key}}
	if _, err := FetchBranches(context.Background(), store, "project", "session", private); err == nil || !strings.Contains(err.Error(), "get failed") {
		t.Fatalf("get error = %v", err)
	}
}

func TestParseShardObjectKeyFiltersForeignShapes(t *testing.T) {
	prefix := "v1/projects/project/sessions/session"
	for _, key := range []string{
		"",
		prefix + "/device/000001/extra",
		prefix + "/Device/000001",
		prefix + "/device/not-a-shard",
		prefix + "/device/meta",
		"other/" + prefix + "/device/000001",
	} {
		if _, _, ok := parseShardObjectKey(prefix, key); ok {
			t.Errorf("parseShardObjectKey(%q) unexpectedly accepted", key)
		}
	}
	device, number, ok := parseShardObjectKey(prefix, prefix+"/device/000001")
	if !ok || device != "device" || number != 1 {
		t.Fatalf("valid shard key parsed as %q, %d, %v", device, number, ok)
	}
}

func TestReadRemoteShardHandlesReadCloseAndCancellationErrors(t *testing.T) {
	readErr := errors.New("read failed")
	closeErr := errors.New("close failed")
	for name, reader := range map[string]io.ReadCloser{
		"read":  &errorReadCloser{readErr: readErr},
		"close": &errorReadCloser{body: []byte("body"), closeErr: closeErr},
	} {
		t.Run(name, func(t *testing.T) {
			store := &singleReaderRemote{reader: reader}
			if _, err := readRemoteShard(context.Background(), store, "key"); err == nil {
				t.Fatal("readRemoteShard unexpectedly succeeded")
			}
		})
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	store := &singleReaderRemote{reader: &errorReadCloser{body: []byte("body")}}
	if _, err := readRemoteShard(cancelled, store, "key"); err == nil {
		t.Fatal("cancelled readRemoteShard unexpectedly succeeded")
	}
}

type errorReadCloser struct {
	body     []byte
	readErr  error
	closeErr error
}

func (r *errorReadCloser) Read(p []byte) (int, error) {
	if r.readErr != nil {
		return 0, r.readErr
	}
	if len(r.body) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.body)
	r.body = r.body[n:]
	return n, nil
}

func (r *errorReadCloser) Close() error { return r.closeErr }

type singleReaderRemote struct {
	reader io.ReadCloser
}

func (s *singleReaderRemote) Name() string { return "single-reader" }
func (s *singleReaderRemote) List(context.Context, string) ([]remote.ObjectInfo, error) {
	return nil, nil
}
func (s *singleReaderRemote) Get(context.Context, string) (io.ReadCloser, error) {
	return s.reader, nil
}
func (s *singleReaderRemote) Put(context.Context, string, io.Reader, int64) error { return nil }
func (s *singleReaderRemote) Delete(context.Context, string) error                { return nil }
func (s *singleReaderRemote) Stat(context.Context, string) (remote.ObjectInfo, error) {
	return remote.ObjectInfo{}, remote.ErrNotFound
}
