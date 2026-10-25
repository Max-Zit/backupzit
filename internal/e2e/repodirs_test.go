package e2e

import (
	"context"
	"reflect"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/testutil"
)

// The console finds the repositories below a storage location on S3 and
// on a hardened repository (which can only list everything).
func TestRepositoryDirsRemote(t *testing.T) {
	ctx := context.Background()
	srv, opts := startHardened(t)
	s3, err := testutil.StartS3Server("bucket")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s3.Close)
	for name, c := range map[string]struct {
		loc  func(dir string) string
		opts backend.Options
	}{
		"hardened": {func(dir string) string { return "hardened://" + srv.Host + "/office" + dir }, opts},
		"s3":       {func(dir string) string { return "s3://" + s3.Host + "/bucket/office" + dir + "?tls=false" }, backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"}},
	} {
		for _, pc := range []string{"pc-b_2", "pc-a_1"} {
			be, err := backend.Open(ctx, c.loc("/"+pc), c.opts)
			if err != nil {
				t.Fatal(err)
			}
			r, err := repo.Init(ctx, be, repo.Password("x"))
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
		}
		be, err := backend.Open(ctx, c.loc(""), c.opts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := backend.RepositoryDirs(ctx, be)
		if want := []string{"pc-a_1", "pc-b_2"}; err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v %v, want %v", name, got, err, want)
		}
	}
}
