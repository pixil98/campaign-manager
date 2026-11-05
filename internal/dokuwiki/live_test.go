package dokuwiki

import (
	"context"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"
)

// TestLive runs the client against a real wiki, when DOKUWIKI_URL and
// DOKUWIKI_TOKEN name one and a superuser's API token for it. It works in
// a namespace of its own and removes what it made.
func TestLive(t *testing.T) {
	base, token := os.Getenv("DOKUWIKI_URL"), os.Getenv("DOKUWIKI_TOKEN")
	if base == "" || token == "" {
		t.Skip("DOKUWIKI_URL and DOKUWIKI_TOKEN are not set")
	}
	c, err := New(base, token, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const (
		ns      = "campaign-manager-live-test"
		page    = ns + ":notes"
		subject = "@campaign-manager-live-test"
	)
	t.Cleanup(func() {
		var ok bool
		if err := c.call(ctx, "plugin.acl.delAcl", map[string]any{"scope": ns + ":*", "user": subject}, &ok); err != nil {
			t.Errorf("removing the test rule: %v", err)
		}
		// Saving empty text deletes a page.
		if err := c.call(ctx, "core.savePage", map[string]any{"page": page, "text": ""}, &ok); err != nil {
			t.Errorf("removing the test page: %v", err)
		}
	})

	if err := c.AddACL(ctx, ns+":*", subject, 1); err != nil {
		t.Fatalf("AddACL = %v", err)
	}
	if err := c.SavePage(ctx, page, "live test text", "campaign-manager live test"); err != nil {
		t.Fatalf("SavePage = %v", err)
	}
	pages, err := c.ListPages(ctx, ns)
	if err != nil || !slices.Contains(pages, page) {
		t.Errorf("ListPages(%s) = %v, %v; want it to include %s", ns, pages, err, page)
	}
	text, err := c.Page(ctx, page)
	if err != nil || text != "live test text" {
		t.Errorf("Page(%s) = %q, %v; want the saved text", page, text, err)
	}
}
