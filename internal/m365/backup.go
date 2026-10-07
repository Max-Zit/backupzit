package m365

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/user"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/max-zit/backupzit/internal/repo"
	"github.com/restic/chunker"
)

// Snapshots of Microsoft 365 hold one folder per account and one for
// SharePoint:
//
//	<account>/Mail/<mail folder>/.../<date> <subject> [<id>].eml
//	<account>/OneDrive/<folders>/<file>
//	<account>/Calendar/<calendar>/<start> <subject> [<id>].ics
//	<account>/Contacts/<folder>/.../<name> [<id>].vcf
//	SharePoint/<site>/<library>/<folders>/<file>
//
// Messages are stored in MIME format, as Outlook and other mail programs
// open them. A message or file whose modification time (and for files the
// size) equals the one in the previous backup reuses its stored data
// without downloading it again.

// Folder names below an account and at the top.
const (
	MailDir       = "Mail"
	OneDriveDir   = "OneDrive"
	CalendarDir   = "Calendar"
	ContactsDir   = "Contacts"
	SharePointDir = "SharePoint"
	// Tag marks Microsoft 365 snapshots.
	Tag = "m365"
)

// Options of a backup.
type Options struct {
	// Users are user principal names or addresses; none means every
	// account of the tenant that has a mailbox or a OneDrive.
	Users    []string
	Mail     bool
	OneDrive bool
	// Calendar backs up the calendars and contacts of the accounts.
	Calendar bool
	// SharePoint backs up the document libraries of Sites (addresses;
	// none means every site of the tenant).
	SharePoint bool
	Sites      []string
	// Parent is the previous backup of the same job (nil: full backup).
	Parent   *repo.Snapshot
	Hostname string
	Version  string
	Tags     []string
	// Workers download in parallel (default 4: Exchange Online allows four
	// concurrent requests per app and mailbox).
	Workers  int
	Progress func(s *repo.SnapshotStats)
}

func (o Options) accounts() bool { return o.Mail || o.OneDrive || o.Calendar }

// Result of a backup.
type Result struct {
	Snapshot *repo.Snapshot
	// Accounts are the accounts backed up; Skipped those of the tenant
	// without a mailbox or OneDrive (all accounts mode).
	Accounts []string
	Skipped  []string
	// Sites are the SharePoint sites backed up.
	Sites []string
}

type backup struct {
	r    *repo.Repository
	c    *Client
	opts Options

	mu    sync.Mutex
	stats repo.SnapshotStats
	start time.Time
}

// Backup reads the accounts from Microsoft 365 and saves a snapshot.
func Backup(ctx context.Context, r *repo.Repository, c *Client, opts Options) (*Result, error) {
	if !opts.accounts() && !opts.SharePoint {
		return nil, errors.New("choose mail, OneDrive, calendars and contacts or SharePoint")
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	b := &backup{r: r, c: c, opts: opts, start: time.Now()}
	before := r.Stats()
	res := &Result{}

	var accounts []User
	all := len(opts.Users) == 0
	switch {
	case !opts.accounts():
	case all:
		us, err := c.Users(ctx)
		if err != nil {
			return nil, fmt.Errorf("list the accounts: %w", err)
		}
		accounts = us
	default:
		for _, name := range opts.Users {
			u, err := c.User(ctx, name)
			if err != nil {
				if IsNotFound(err) {
					b.addError(name, errors.New("no such account in the tenant"))
					continue
				}
				return nil, fmt.Errorf("account %s: %w", name, err)
			}
			accounts = append(accounts, u)
		}
	}
	sort.Slice(accounts, func(i, j int) bool {
		return strings.ToLower(accounts[i].UserPrincipalName) < strings.ToLower(accounts[j].UserPrincipalName)
	})

	var ptree *repo.Tree
	if opts.Parent != nil {
		t, err := r.LoadTree(ctx, opts.Parent.Tree)
		if err != nil {
			return nil, fmt.Errorf("load the previous backup: %w", err)
		}
		ptree = t
	}
	root := &repo.Tree{}
	for _, u := range accounts {
		name := cleanName(strings.ToLower(u.UserPrincipalName), 200)
		n, found, err := b.account(ctx, u, b.subtree(ctx, ptree, name), !all)
		if err != nil {
			return nil, err
		}
		if !found {
			res.Skipped = append(res.Skipped, u.UserPrincipalName)
			continue
		}
		n.Name = name
		root.Nodes = append(root.Nodes, n)
		res.Accounts = append(res.Accounts, u.UserPrincipalName)
	}
	if opts.SharePoint {
		n, sites, err := b.sharePoint(ctx, b.subtree(ctx, ptree, SharePointDir))
		if err != nil {
			return nil, err
		}
		if len(sites) > 0 {
			root.Nodes = append(root.Nodes, n)
			res.Sites = sites
		}
	}
	if len(root.Nodes) == 0 {
		if len(b.stats.Errors) > 0 {
			return nil, errors.New(b.stats.Errors[0])
		}
		return nil, errors.New("none of the accounts has a mailbox or a OneDrive (check the licenses and the permissions Mail.Read and Files.Read.All)")
	}
	root.Sort()
	treeID, err := r.SaveTree(ctx, root)
	if err != nil {
		return nil, err
	}
	if err := r.Flush(ctx); err != nil {
		return nil, err
	}
	after := r.Stats()
	b.stats.BytesAdded = after.RawBytes - before.RawBytes
	b.stats.BytesStored = after.StoredBytes - before.StoredBytes
	b.stats.Duration = time.Since(b.start)
	paths := make([]string, 0, len(res.Accounts)+1)
	for _, a := range res.Accounts {
		paths = append(paths, "/"+strings.ToLower(a))
	}
	if len(res.Sites) > 0 {
		paths = append(paths, "/"+SharePointDir)
	}
	sn := &repo.Snapshot{
		Time:           b.start.UTC(),
		Hostname:       opts.Hostname,
		Paths:          paths,
		Tags:           append(append([]string(nil), opts.Tags...), Tag),
		Tree:           treeID,
		Stats:          b.stats,
		ProgramVersion: opts.Version,
	}
	if cu, err := user.Current(); err == nil {
		sn.Username = cu.Username
	}
	if opts.Parent != nil {
		pid := opts.Parent.ID
		sn.Parent = &pid
	}
	if _, err := r.SaveSnapshot(ctx, sn); err != nil {
		return nil, fmt.Errorf("save snapshot: %w", err)
	}
	res.Snapshot = sn
	return res, nil
}

// account backs up the mailbox, OneDrive, calendars and contacts of one
// account. found is false when it has none of them (no license); explicit
// reports that as an error, for accounts the job names.
func (b *backup) account(ctx context.Context, u User, ptree *repo.Tree, explicit bool) (repo.Node, bool, error) {
	t := &repo.Tree{}
	found := false
	if b.opts.Mail {
		n, ok, err := b.mailbox(ctx, u, b.subtree(ctx, ptree, MailDir))
		if err != nil {
			return repo.Node{}, false, err
		}
		if ok {
			t.Nodes = append(t.Nodes, n)
			found = true
		} else if explicit {
			b.addError(u.UserPrincipalName, errors.New("the account has no Exchange Online mailbox"))
		}
	}
	if b.opts.OneDrive {
		n, ok, err := b.drive(ctx, u, b.subtree(ctx, ptree, OneDriveDir))
		if err != nil {
			return repo.Node{}, false, err
		}
		if ok {
			t.Nodes = append(t.Nodes, n)
			found = true
		} else if explicit && !b.opts.Mail {
			b.addError(u.UserPrincipalName, errors.New("the account has no OneDrive"))
		}
	}
	if b.opts.Calendar {
		n, ok, err := b.calendars(ctx, u, b.subtree(ctx, ptree, CalendarDir))
		if err != nil {
			return repo.Node{}, false, err
		}
		if ok {
			t.Nodes = append(t.Nodes, n)
			found = true
		} else if explicit && !b.opts.Mail {
			b.addError(u.UserPrincipalName, errors.New("the account has no Exchange Online mailbox"))
		}
		n, ok, err = b.contactBook(ctx, u, b.subtree(ctx, ptree, ContactsDir))
		if err != nil {
			return repo.Node{}, false, err
		}
		if ok {
			t.Nodes = append(t.Nodes, n)
			found = true
		}
	}
	if !found {
		return repo.Node{}, false, nil
	}
	n, err := b.dirNode(ctx, "", t)
	return n, true, err
}

func (b *backup) mailbox(ctx context.Context, u User, ptree *repo.Tree) (repo.Node, bool, error) {
	folders, err := b.c.mailFolders(ctx, u.ID, "")
	if err != nil {
		if IsNotFound(err) {
			return repo.Node{}, false, nil
		}
		if isDenied(err) {
			return repo.Node{}, false, fmt.Errorf("read the mailbox of %s: %w (grant the application permission Mail.Read with admin consent)", u.UserPrincipalName, err)
		}
		b.addError(u.UserPrincipalName+"/"+MailDir, err)
		return repo.Node{}, false, nil
	}
	t, err := b.mailFolderList(ctx, u, u.UserPrincipalName+"/"+MailDir, folders, ptree)
	if err != nil {
		return repo.Node{}, false, err
	}
	n, err := b.dirNode(ctx, MailDir, t)
	return n, true, err
}

// mailFolderList stores sibling folders (and what they hold) as a tree.
func (b *backup) mailFolderList(ctx context.Context, u User, at string, folders []MailFolder, ptree *repo.Tree) (*repo.Tree, error) {
	sort.Slice(folders, func(i, j int) bool { return folders[i].DisplayName < folders[j].DisplayName })
	t := &repo.Tree{}
	used := map[string]bool{}
	for _, f := range folders {
		name := uniqueName(used, cleanName(f.DisplayName, 120))
		n, err := b.mailFolder(ctx, u, at+"/"+name, f, b.subtree(ctx, ptree, name))
		if err != nil {
			return nil, err
		}
		n.Name = name
		t.Nodes = append(t.Nodes, n)
	}
	return t, nil
}

func (b *backup) mailFolder(ctx context.Context, u User, at string, f MailFolder, ptree *repo.Tree) (repo.Node, error) {
	t := &repo.Tree{}
	if f.ChildCount > 0 {
		children, err := b.c.mailFolders(ctx, u.ID, f.ID)
		if err != nil {
			b.addError(at, err)
		} else {
			ct, err := b.mailFolderList(ctx, u, at, children, ptree)
			if err != nil {
				return repo.Node{}, err
			}
			t.Nodes = append(t.Nodes, ct.Nodes...)
		}
	}
	var msgs []Message
	if err := b.c.messages(ctx, u.ID, f.ID, func(m []Message) error { msgs = append(msgs, m...); return nil }); err != nil {
		if ctx.Err() != nil {
			return repo.Node{}, ctx.Err()
		}
		b.addError(at, err)
	}
	items := make([]item, len(msgs))
	used := map[string]bool{}
	for _, n := range t.Nodes {
		used[n.Name] = true
	}
	for i, m := range msgs {
		items[i] = item{
			name:  uniqueName(used, messageName(m)),
			mtime: m.LastModified,
			open:  func(ctx context.Context) (io.ReadCloser, error) { return b.c.MIME(ctx, u.ID, m.ID) },
		}
	}
	nodes, err := b.files(ctx, at, items, ptree)
	if err != nil {
		return repo.Node{}, err
	}
	t.Nodes = append(t.Nodes, nodes...)
	return b.dirNode(ctx, "", t)
}

func (b *backup) drive(ctx context.Context, u User, ptree *repo.Tree) (repo.Node, bool, error) {
	d, err := b.c.Drive(ctx, u.ID)
	if err != nil {
		if IsNotFound(err) {
			return repo.Node{}, false, nil
		}
		if isDenied(err) {
			return repo.Node{}, false, fmt.Errorf("read the OneDrive of %s: %w (grant the application permission Files.Read.All with admin consent)", u.UserPrincipalName, err)
		}
		b.addError(u.UserPrincipalName+"/"+OneDriveDir, err)
		return repo.Node{}, false, nil
	}
	t, err := b.driveFolder(ctx, d, "root", u.UserPrincipalName+"/"+OneDriveDir, ptree)
	if err != nil {
		return repo.Node{}, false, err
	}
	n, err := b.dirNode(ctx, OneDriveDir, t)
	return n, true, err
}

func (b *backup) driveFolder(ctx context.Context, drive, id, at string, ptree *repo.Tree) (*repo.Tree, error) {
	children, err := b.c.children(ctx, drive, id)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		b.addError(at, err)
		return &repo.Tree{}, nil
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	t := &repo.Tree{}
	used := map[string]bool{}
	var files []item
	for _, c := range children {
		name := uniqueName(used, cleanName(c.Name, 250))
		if c.Folder != nil || c.Package != nil {
			sub, err := b.driveFolder(ctx, drive, c.ID, at+"/"+name, b.subtree(ctx, ptree, name))
			if err != nil {
				return nil, err
			}
			n, err := b.dirNode(ctx, name, sub)
			if err != nil {
				return nil, err
			}
			n.ModTime = c.LastModified.UTC()
			t.Nodes = append(t.Nodes, n)
			continue
		}
		if c.File == nil {
			continue
		}
		files = append(files, item{
			name: name, mtime: c.LastModified, size: c.Size, sized: true,
			open: func(ctx context.Context) (io.ReadCloser, error) { return b.c.Content(ctx, drive, c.ID) },
		})
	}
	nodes, err := b.files(ctx, at, files, ptree)
	if err != nil {
		return nil, err
	}
	t.Nodes = append(t.Nodes, nodes...)
	return t, nil
}

// item is a message or file to store.
type item struct {
	name  string
	mtime time.Time
	size  int64
	sized bool // size is known before reading
	open  func(context.Context) (io.ReadCloser, error)
}

// files stores the items of one folder, downloading those that changed
// since the previous backup in parallel.
func (b *backup) files(ctx context.Context, at string, items []item, ptree *repo.Tree) ([]repo.Node, error) {
	nodes := make([]repo.Node, len(items))
	ok := make([]bool, len(items))
	var todo []int
	for i, it := range items {
		n := repo.Node{Name: it.name, Type: repo.NodeFile, Mode: 0o644, ModTime: it.mtime.UTC()}
		if ptree != nil {
			if p := ptree.Find(it.name); p != nil && p.Type == repo.NodeFile && p.ModTime.Equal(n.ModTime) &&
				(!it.sized || p.Size == uint64(it.size)) && b.allKnown(p.Content) {
				n.Content, n.Size = p.Content, p.Size
				nodes[i], ok[i] = n, true
				b.count(func(s *repo.SnapshotStats) { s.Files++; s.FilesSkipped++; s.Bytes += n.Size })
				continue
			}
		}
		nodes[i] = n
		todo = append(todo, i)
	}
	if len(todo) > 0 {
		work := make(chan int)
		var wg sync.WaitGroup
		var fatal error
		var fatalOnce sync.Once
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		for w := 0; w < min(b.opts.Workers, len(todo)); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				buf := make([]byte, chunker.MaxSize)
				for i := range work {
					it := items[i]
					content, size, err := b.store(wctx, it, buf)
					if err != nil {
						var re *repoError
						if errors.As(err, &re) || wctx.Err() != nil {
							fatalOnce.Do(func() {
								fatal = err
								if re != nil {
									fatal = re.err
								}
								cancel()
							})
							continue
						}
						b.addError(at+"/"+it.name, err)
						continue
					}
					nodes[i].Content, nodes[i].Size = content, size
					ok[i] = true
					isNew := ptree == nil || ptree.Find(it.name) == nil
					b.count(func(s *repo.SnapshotStats) {
						s.Files++
						s.Bytes += size
						s.BytesRead += size
						if isNew {
							s.FilesNew++
						} else {
							s.FilesChanged++
						}
					})
				}
			}()
		}
	feed:
		for _, i := range todo {
			select {
			case work <- i:
			case <-wctx.Done():
				break feed
			}
		}
		close(work)
		wg.Wait()
		if fatal != nil {
			return nil, fatal
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	out := nodes[:0]
	for i, n := range nodes {
		if ok[i] {
			out = append(out, n)
		}
	}
	return out, nil
}

// repoError marks failures of the repository, which end the backup.
type repoError struct{ err error }

func (e *repoError) Error() string { return e.err.Error() }

// store downloads one item into the repository.
func (b *backup) store(ctx context.Context, it item, buf []byte) ([]repo.ID, uint64, error) {
	rc, err := it.open(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer rc.Close()
	chk := chunker.New(rc, b.r.ChunkerPolynomial())
	var ids []repo.ID
	var size uint64
	for {
		c, err := chk.Next(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		size += uint64(c.Length)
		id, _, err := b.r.SaveBlob(ctx, repo.DataBlob, c.Data)
		if err != nil {
			return nil, 0, &repoError{err}
		}
		ids = append(ids, id)
	}
	return ids, size, nil
}

func (b *backup) allKnown(ids []repo.ID) bool {
	idx := b.r.Index()
	for _, id := range ids {
		if !idx.Has(repo.BlobHandle{Type: repo.DataBlob, ID: id}) {
			return false
		}
	}
	return true
}

func (b *backup) count(fn func(*repo.SnapshotStats)) {
	b.mu.Lock()
	fn(&b.stats)
	s := b.stats
	b.mu.Unlock()
	if b.opts.Progress != nil {
		b.opts.Progress(&s)
	}
}

func (b *backup) addError(at string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stats.Errors = append(b.stats.Errors, fmt.Sprintf("%s: %v", at, err))
}

// dirNode saves a folder's tree.
func (b *backup) dirNode(ctx context.Context, name string, t *repo.Tree) (repo.Node, error) {
	t.Sort()
	id, err := b.r.SaveTree(ctx, t)
	if err != nil {
		return repo.Node{}, err
	}
	b.count(func(s *repo.SnapshotStats) { s.Dirs++ })
	return repo.Node{Name: name, Type: repo.NodeDir, Mode: uint32(fs.ModeDir | 0o755), ModTime: b.start.UTC(), Subtree: &id}, nil
}

// subtree is the folder name in a tree of the previous backup, or nil.
func (b *backup) subtree(ctx context.Context, t *repo.Tree, name string) *repo.Tree {
	if t == nil {
		return nil
	}
	n := t.Find(name)
	if n == nil || n.Type != repo.NodeDir || n.Subtree == nil {
		return nil
	}
	st, err := b.r.LoadTree(ctx, *n.Subtree)
	if err != nil {
		return nil
	}
	return st
}

func isDenied(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == 401 || e.Status == 403)
}

// messageName names a message file: when it arrived, its subject and a
// short hash of its ID, which keeps the name unique and the same in every
// backup while the message stays in its folder.
func messageName(m Message) string {
	t := m.Received
	if t.IsZero() {
		t = m.Created
	}
	subj := cleanName(m.Subject, 80)
	if strings.TrimSpace(m.Subject) == "" {
		subj = "(no subject)"
	}
	return fmt.Sprintf("%s %s [%s].eml", t.UTC().Format("2006-01-02 1504"), subj, idHash(m.ID))
}

// idHash is a short hash of a Graph ID, which keeps names unique.
func idHash(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])[:10]
}

// cleanName makes a name valid as a file name on Windows and Linux and
// at most max characters long.
func cleanName(s string, max int) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r < 32 || r == 0x7f || r == utf8.RuneError:
			sb.WriteRune(' ')
		case strings.ContainsRune(`<>:"/\|?*`, r):
			sb.WriteRune('_')
		default:
			sb.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(sb.String()), " ")
	if utf8.RuneCountInString(out) > max {
		out = string([]rune(out)[:max])
	}
	out = strings.TrimRight(out, ". ")
	switch strings.ToUpper(out) {
	case "", ".", "..":
		return "_"
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "LPT1", "LPT2", "LPT3":
		return out + "_"
	}
	return out
}

// uniqueName adds " (2)", " (3)"... to a name already used in a folder.
func uniqueName(used map[string]bool, name string) string {
	key := strings.ToLower(name)
	if !used[key] {
		used[key] = true
		return name
	}
	ext := ""
	if i := strings.LastIndex(name, "."); i > 0 {
		name, ext = name[:i], name[i:]
	}
	for k := 2; ; k++ {
		c := fmt.Sprintf("%s (%d)%s", name, k, ext)
		if !used[strings.ToLower(c)] {
			used[strings.ToLower(c)] = true
			return c
		}
	}
}
