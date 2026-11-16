package ledgerstream

import (
	"context"
	"io"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"
)

// storeReadTimeout bounds one datastore read, body included. The SDK's S3 client sets no response or body timeout, and
// its fetch worker retries only when a read RETURNS, so a request that hangs would hold the walk forever. A ledger
// object is a few MB at most; a variable only so tests can shorten it.
var storeReadTimeout = 60 * time.Second

// deadlineStore gives every read on the wrapped store a deadline. It must expire as context.DeadlineExceeded, never
// context.Canceled: the SDK's fetch worker treats Canceled as shutdown and exits without reporting an error.
type deadlineStore struct {
	datastore.DataStore
	timeout time.Duration
}

// withReadDeadline wraps store so no single read can outlast timeout.
func withReadDeadline(store datastore.DataStore, timeout time.Duration) datastore.DataStore {
	return &deadlineStore{DataStore: store, timeout: timeout}
}

// GetFile holds the deadline open until the returned body is closed, so it covers the body read as well as the headers.
func (d *deadlineStore) GetFile(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	body, size, err := d.DataStore.GetFile(ctx, path)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	return &deadlineBody{ReadCloser: body, cancel: cancel}, size, nil
}

type deadlineBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *deadlineBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func (d *deadlineStore) GetFileMetadata(ctx context.Context, path string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	return d.DataStore.GetFileMetadata(ctx, path)
}

func (d *deadlineStore) GetFileLastModified(ctx context.Context, path string) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	return d.DataStore.GetFileLastModified(ctx, path)
}

func (d *deadlineStore) Exists(ctx context.Context, path string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	return d.DataStore.Exists(ctx, path)
}

func (d *deadlineStore) Size(ctx context.Context, path string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	return d.DataStore.Size(ctx, path)
}

func (d *deadlineStore) ListFilePaths(ctx context.Context, options datastore.ListFileOptions) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	return d.DataStore.ListFilePaths(ctx, options)
}
