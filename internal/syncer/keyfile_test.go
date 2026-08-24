package syncer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

func TestKeyfileTransportPublishesOnceAndFetches(t *testing.T) {
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := &crypto.Keyfile{
		Version:              1,
		IdentityPublic:       []byte{1, 2, 3},
		WrappedByPassphrase:  []byte("passphrase-envelope"),
		WrappedByRecoveryKey: []byte("recovery-envelope"),
	}

	if err := PublishKeyfile(context.Background(), store, want); err != nil {
		t.Fatalf("PublishKeyfile: %v", err)
	}
	got, err := FetchKeyfile(context.Background(), store)
	if err != nil {
		t.Fatalf("FetchKeyfile: %v", err)
	}
	if !bytes.Equal(got.IdentityPublic, want.IdentityPublic) ||
		!bytes.Equal(got.WrappedByPassphrase, want.WrappedByPassphrase) ||
		!bytes.Equal(got.WrappedByRecoveryKey, want.WrappedByRecoveryKey) {
		t.Errorf("fetched keyfile differs: got %+v, want %+v", got, want)
	}

	replacement := &crypto.Keyfile{Version: 1, WrappedByPassphrase: []byte("replacement")}
	if err := PublishKeyfile(context.Background(), store, replacement); !errors.Is(err, ErrRemoteKeyfileExists) {
		t.Fatalf("second PublishKeyfile = %v, want ErrRemoteKeyfileExists", err)
	}
	still, err := FetchKeyfile(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(still.IdentityPublic, want.IdentityPublic) {
		t.Error("a rejected publication replaced the existing keyfile")
	}
}

func TestFetchKeyfileDistinguishesMissingAndDamagedObjects(t *testing.T) {
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := FetchKeyfile(context.Background(), store); !errors.Is(err, ErrNoRemoteKeyfile) {
		t.Fatalf("missing keyfile = %v, want ErrNoRemoteKeyfile", err)
	}
	if err := store.Put(context.Background(), crypto.KeyfilePath(), strings.NewReader("not-json"), int64(len("not-json"))); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchKeyfile(context.Background(), store); err == nil || errors.Is(err, ErrNoRemoteKeyfile) {
		t.Fatalf("damaged keyfile = %v, want a non-missing error", err)
	}
}

func TestFetchKeyfileRejectsOversizedObjects(t *testing.T) {
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{'x'}, maxKeyfileBytes+1)
	if err := store.Put(context.Background(), crypto.KeyfilePath(), bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchKeyfile(context.Background(), store); !errors.Is(err, ErrRemoteKeyfileTooLarge) {
		t.Fatalf("oversized keyfile = %v, want ErrRemoteKeyfileTooLarge", err)
	}
}

func TestKeyfileTransportRequiresContextAndStore(t *testing.T) {
	keyfile := &crypto.Keyfile{Version: 1, WrappedByPassphrase: []byte("wrapped")}
	if err := PublishKeyfile(nil, nil, keyfile); err == nil {
		t.Error("PublishKeyfile accepted nil context and store")
	}
	if _, err := FetchKeyfile(nil, nil); err == nil {
		t.Error("FetchKeyfile accepted nil context and store")
	}
	if err := PublishKeyfile(context.Background(), nil, keyfile); err == nil {
		t.Error("PublishKeyfile accepted nil store")
	}
	if _, err := FetchKeyfile(context.Background(), nil); err == nil {
		t.Error("FetchKeyfile accepted nil store")
	}
}
