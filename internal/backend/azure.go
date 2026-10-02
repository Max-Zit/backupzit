package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

// Azure stores the repository in Azure Blob Storage.
//
// Location: azure://<account>/<container>[/<prefix>]. The service URL is
// https://<account>.blob.core.windows.net unless "?endpoint=URL" is given
// (Azure Stack, sovereign clouds, the Azurite emulator). Authenticate with
// the storage account key (Options.AzureKey) or a SAS token with read,
// write, delete and list rights (Options.AzureSAS).
type Azure struct {
	c      *container.Client
	prefix string
	loc    string
}

// OpenAzure connects to the container named in u.
func OpenAzure(ctx context.Context, u *url.URL, opts Options) (*Azure, error) {
	account := u.Host
	parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 2)
	if account == "" || parts[0] == "" {
		return nil, errors.New("azure: location must look like azure://account/container/folder")
	}
	cname, prefix := parts[0], ""
	if len(parts) == 2 {
		prefix = strings.Trim(parts[1], "/")
	}
	endpoint := u.Query().Get("endpoint")
	if endpoint == "" {
		endpoint = "https://" + account + ".blob.core.windows.net"
	}
	endpoint = strings.TrimRight(endpoint, "/")
	cu := endpoint + "/" + cname
	clientOpts := &container.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: 4}}}
	var c *container.Client
	var err error
	switch {
	case opts.AzureKey != "":
		cred, cerr := azblob.NewSharedKeyCredential(account, opts.AzureKey)
		if cerr != nil {
			return nil, fmt.Errorf("azure: account key: %w", cerr)
		}
		c, err = container.NewClientWithSharedKeyCredential(cu, cred, clientOpts)
	case opts.AzureSAS != "":
		c, err = container.NewClientWithNoCredential(cu+"?"+strings.TrimPrefix(opts.AzureSAS, "?"), clientOpts)
	default:
		return nil, errors.New("azure: storage account key or SAS token is required")
	}
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	if _, err := c.GetProperties(ctx, nil); err != nil {
		if bloberror.HasCode(err, bloberror.ContainerNotFound) {
			return nil, fmt.Errorf("azure: container %q does not exist in account %s", cname, account)
		}
		return nil, fmt.Errorf("azure: access container %q: %w", cname, shortAzureErr(err))
	}
	return &Azure{c: c, prefix: prefix, loc: fmt.Sprintf("azure://%s/%s/%s", account, cname, prefix)}, nil
}

// shortAzureErr keeps the first line of the SDK's verbose errors.
func shortAzureErr(err error) error {
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		return fmt.Errorf("%s (HTTP %d)", re.ErrorCode, re.StatusCode)
	}
	return err
}

func (a *Azure) key(name string) string {
	if a.prefix == "" {
		return name
	}
	return path.Join(a.prefix, name)
}

func (a *Azure) Location() string { return a.loc }
func (a *Azure) Close() error     { return nil }

func azNotFound(name string, err error) error {
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	var re *azcore.ResponseError
	if errors.As(err, &re) && re.StatusCode == 404 {
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return shortAzureErr(err)
}

// Save uploads a block blob; it becomes visible only when complete.
func (a *Azure) Save(ctx context.Context, name string, data []byte) error {
	_, err := a.c.NewBlockBlobClient(a.key(name)).UploadBuffer(ctx, data, &azblob.UploadBufferOptions{
		BlockSize: 8 << 20, Concurrency: 4,
	})
	if err != nil {
		return fmt.Errorf("azure put %s: %w", name, shortAzureErr(err))
	}
	return nil
}

func (a *Azure) download(ctx context.Context, name string, rng blob.HTTPRange) ([]byte, error) {
	resp, err := a.c.NewBlobClient(a.key(name)).DownloadStream(ctx, &blob.DownloadStreamOptions{Range: rng})
	if err != nil {
		return nil, azNotFound(name, err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	if _, err := io.Copy(&b, resp.Body); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func (a *Azure) Load(ctx context.Context, name string) ([]byte, error) {
	return a.download(ctx, name, blob.HTTPRange{})
}

func (a *Azure) LoadRange(ctx context.Context, name string, offset int64, length int) ([]byte, error) {
	if offset < 0 {
		size, err := a.Size(ctx, name)
		if err != nil {
			return nil, err
		}
		offset += size
	}
	if length <= 0 {
		return []byte{}, nil
	}
	b, err := a.download(ctx, name, blob.HTTPRange{Offset: offset, Count: int64(length)})
	if err != nil {
		return nil, err
	}
	if len(b) != length {
		return nil, fmt.Errorf("read %s@%d+%d: %w", name, offset, length, io.ErrUnexpectedEOF)
	}
	return b, nil
}

func (a *Azure) Size(ctx context.Context, name string) (int64, error) {
	p, err := a.c.NewBlobClient(a.key(name)).GetProperties(ctx, nil)
	if err != nil {
		return 0, azNotFound(name, err)
	}
	if p.ContentLength == nil {
		return 0, fmt.Errorf("%s: no content length", name)
	}
	return *p.ContentLength, nil
}

func (a *Azure) List(ctx context.Context, dir string) ([]string, error) {
	prefix := a.key(dir) + "/"
	var out []string
	pager := a.c.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &prefix})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure list %s: %w", dir, shortAzureErr(err))
		}
		for _, item := range page.Segment.BlobItems {
			if item.Name == nil {
				continue
			}
			n := *item.Name
			if a.prefix != "" {
				n = strings.TrimPrefix(n, a.prefix+"/")
			}
			out = append(out, n)
		}
	}
	return out, nil
}

func (a *Azure) Remove(ctx context.Context, name string) error {
	if _, err := a.c.NewBlobClient(a.key(name)).Delete(ctx, nil); err != nil {
		return azNotFound(name, err)
	}
	return nil
}
