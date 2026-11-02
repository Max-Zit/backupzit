// Command uiwalk signs in to a running console and reads every page the way
// a browser does (package uicheck): submit buttons inside their forms,
// unique ids, no template leftovers and, with -save, every edit page sent
// back unchanged keeps its values. Run it against the QA console before a
// candidate is published. Development tool; not shipped.
//
//	BZ_PASSWORD=... uiwalk -url https://192.168.101.154:8443 -user admin -save
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"

	"github.com/max-zit/backupzit/internal/uicheck"
)

type client struct {
	c    *http.Client
	base string
}

func (c *client) Do(method, p string, form url.Values) (int, string, string) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, c.base+p, body)
	if err != nil {
		return 0, "", ""
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Origin", c.base)
	resp, err := c.c.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "uiwalk:", err)
		return 0, "", ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

func main() {
	base := flag.String("url", "", "console address, e.g. https://192.168.101.154:8443")
	user := flag.String("user", "admin", "user name (password from BZ_PASSWORD)")
	save := flag.Bool("save", false, "send every edit page back unchanged and compare")
	flag.Parse()
	pw := os.Getenv("BZ_PASSWORD")
	if *base == "" || pw == "" {
		fmt.Fprintln(os.Stderr, "usage: BZ_PASSWORD=... uiwalk -url https://console:8443 [-user admin] [-save]")
		os.Exit(2)
	}
	jar, _ := cookiejar.New(nil)
	c := &client{base: strings.TrimRight(*base, "/"), c: &http.Client{Jar: jar,
		// Lab consoles have self-signed certificates.
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	// One sign-in only: failed sign-ins lock the address out.
	if _, loc, _ := c.Do("POST", "/login", url.Values{"username": {*user}, "password": {pw}}); loc != "/" {
		fmt.Fprintln(os.Stderr, "uiwalk: sign-in failed")
		os.Exit(1)
	}
	r := uicheck.Walk(c, uicheck.Options{RoundTrip: *save, Skip: []string{"m365_tenant"}})
	for _, p := range r.Problems {
		fmt.Println("FAIL ", p)
	}
	fmt.Printf("%d pages, %d edit pages, %d sent back unchanged, %d problems\n", r.Pages, len(r.Edits), len(r.Saved), len(r.Problems))
	if len(r.Problems) > 0 {
		os.Exit(1)
	}
}
