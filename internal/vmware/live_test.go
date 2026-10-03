package vmware

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"testing"
	"time"
)

// Live tests against a real ESXi host:
//
//	BACKUPZIT_TEST_ESXI=192.168.101.100 BACKUPZIT_TEST_ESXI_PASSWORD=… go test ./internal/vmware -run Live
func liveClient(t *testing.T) *Client {
	host := os.Getenv("BACKUPZIT_TEST_ESXI")
	if host == "" {
		t.Skip("BACKUPZIT_TEST_ESXI not set")
	}
	ctx := context.Background()
	c, err := Connect(ctx, Conn{Host: host, User: "root", Password: os.Getenv("BACKUPZIT_TEST_ESXI_PASSWORD")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Logout)
	return c
}

func TestLiveDiskWriter(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dss, err := c.Datastores(ctx)
	if err != nil || len(dss) == 0 {
		t.Fatal(dss, err)
	}
	p := dsPath{DS: dss[0].Name, Path: tmpDir + "/livetest.vmdk"}
	c.run(ctx, "mkdir -p "+sq("/vmfs/volumes/"+p.DS+"/"+tmpDir)+"; vmkfstools -U "+sq(p.local())+" 2>/dev/null")
	if _, err := c.run(ctx, "vmkfstools -c 16M -d thin "+sq(p.local())); err != nil {
		t.Fatal(err)
	}
	defer c.run(context.Background(), "vmkfstools -U "+sq(p.local()))
	flat, err := c.flatFile(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3<<20)
	rand.New(rand.NewSource(1)).Read(data)
	w, err := c.openWriter(ctx, flat)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := w.WriteAt(data, 5<<20); err != nil || n != len(data) {
		t.Fatal(n, err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("close: %v out=%q", err, w.out.String())
	}
	got := make([]byte, len(data))
	if _, err := (&fileReader{ctx: ctx, c: c, url: c.fileURL(flat)}).ReadAt(got, 5<<20); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("data read back differs")
	}
}
