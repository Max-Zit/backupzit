package e2e

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// TestBackupRestoreAzure runs against Azure Blob Storage or Azurite, e.g.
//
//	BACKUPZIT_TEST_AZURE="azure://devstoreaccount1/?endpoint=http://host:10000/devstoreaccount1"
//	BACKUPZIT_TEST_AZURE_KEY=<account key> go test ./internal/e2e -run Azure
//
// A new container is created for each run.
func TestBackupRestoreAzure(t *testing.T) {
	loc := os.Getenv("BACKUPZIT_TEST_AZURE")
	if loc == "" {
		t.Skip("BACKUPZIT_TEST_AZURE not set")
	}
	ctx := context.Background()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	key := os.Getenv("BACKUPZIT_TEST_AZURE_KEY")
	cname := fmt.Sprintf("bz-e2e-%d", time.Now().UnixNano())
	endpoint := u.Query().Get("endpoint")
	if endpoint == "" {
		endpoint = "https://" + u.Host + ".blob.core.windows.net"
	}
	cred, err := azblob.NewSharedKeyCredential(u.Host, key)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := azblob.NewClientWithSharedKeyCredential(strings.TrimRight(endpoint, "/")+"/", cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateContainer(ctx, cname, nil); err != nil {
		t.Fatal(err)
	}
	defer svc.DeleteContainer(ctx, cname, nil)
	u.Path = "/" + cname + "/office/pc1"
	opts := backend.Options{AzureKey: key}
	if _, err := backend.Open(ctx, strings.Replace(u.String(), cname, cname+"-missing", 1), opts); err == nil {
		t.Error("missing container accepted")
	}
	if _, err := backend.Open(ctx, u.String(), backend.Options{AzureKey: "d3Jvbmc="}); err == nil {
		t.Error("wrong key accepted")
	}
	runCycle(t, func() backend.Backend {
		be, err := backend.Open(ctx, u.String(), opts)
		if err != nil {
			t.Fatal(err)
		}
		return be
	}, false, repo.Password("azure-test"))
}
