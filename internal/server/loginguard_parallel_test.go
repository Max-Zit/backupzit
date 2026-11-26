package server_test

import (
	"net/http"
	"net/url"
	"sync"
	"testing"
)

// A7: parallel sign-ins from one address get no more guesses than the
// limit (5 by default): each check waits for the one before it, which has
// already counted its failure.
func TestBruteForceParallel(t *testing.T) {
	e := setup(t)
	const n = 20
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := newClient(t, e)
			<-start
			codes[i], _, _ = c.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"wrong-guess-1"}})
		}(i)
	}
	close(start)
	wg.Wait()
	checked, refused := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK: // a password check that failed
			checked++
		case http.StatusTooManyRequests:
			refused++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if checked > 5 || checked+refused != n {
		t.Errorf("%d passwords checked, %d refused; want at most 5 checked", checked, refused)
	}
}
