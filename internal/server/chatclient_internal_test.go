package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A18: webhooks follow no redirects and reach no link-local address.
func TestChatClientLimits(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect followed")
	}))
	defer inner.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL, http.StatusFound)
	}))
	defer redir.Close()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", redir.URL, nil)
	resp, err := chatClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status %d", resp.StatusCode)
	}
	for _, u := range []string{"http://169.254.169.254/latest/meta-data/", "http://[fe80::1]:80/"} {
		req, _ := http.NewRequestWithContext(context.Background(), "POST", u, nil)
		_, err := chatClient.Do(req)
		if err == nil || !strings.Contains(err.Error(), "link-local") {
			t.Errorf("%s: %v", u, err)
		}
	}
}
