package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.Header().Set("Server", "testsrv")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>ok</html>")
	}))
	defer srv.Close()

	out, err := Probe(context.Background(), []string{srv.URL + "/redir", srv.URL + "/x"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"200", "Server: testsrv", "redirected to", "missing security headers"} {
		if !strings.Contains(out, want) {
			t.Errorf("probe output missing %q:\n%s", want, out)
		}
	}
}

func TestCrawl(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<title>Home</title><a href="/a">a</a><a href="/b">b</a>`)
		case "/a":
			fmt.Fprint(w, `<title>A</title><a href="/c">c</a>`)
		default:
			fmt.Fprint(w, `<title>Page</title>`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out, err := Crawl(context.Background(), srv.URL+"/", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/a", "/b", "Home"} {
		if !strings.Contains(out, want) {
			t.Errorf("crawl output missing %q:\n%s", want, out)
		}
	}
}

// TestCrawlShapeCap: a page linking to many URLs of the same shape (?page=N)
// must not crawl more than maxPerShape of them — guards frontier explosion.
func TestCrawlShapeCap(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			var b strings.Builder
			b.WriteString("<title>Home</title>")
			for i := 0; i < 30; i++ { // 30 same-shape parameterized links
				fmt.Fprintf(&b, `<a href="/list?page=%d">p%d</a>`, i, i)
			}
			fmt.Fprint(w, b.String())
			return
		}
		fmt.Fprintf(w, "<title>List %s</title>", r.URL.RawQuery)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out, err := Crawl(context.Background(), srv.URL+"/", 100)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Count(out, "/list?page=")
	if got > maxPerShape {
		t.Errorf("crawled %d same-shape URLs, want <= %d:\n%s", got, maxPerShape, out)
	}
	if got == 0 {
		t.Errorf("expected at least one /list?page= URL crawled:\n%s", out)
	}
}

// TestCrawlValueRoutingNotCollapsed: value-routed endpoints (?page=login.php,
// ?page=admin.php, …) are distinct shapes and must NOT be collapsed by the
// numeric-pagination cap — regression for router-style under-crawl.
func TestCrawlValueRoutingNotCollapsed(t *testing.T) {
	routes := []string{"login.php", "admin.php", "user-info.php", "add-blog.php",
		"view-blog.php", "secret.php", "upload.php", "search.php"}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.php" || r.URL.Path == "/" {
			if r.URL.RawQuery == "" {
				var b strings.Builder
				b.WriteString("<title>Home</title>")
				for _, p := range routes {
					fmt.Fprintf(&b, `<a href="/index.php?page=%s">%s</a>`, p, p)
				}
				fmt.Fprint(w, b.String())
				return
			}
		}
		fmt.Fprintf(w, "<title>%s</title>", r.URL.Query().Get("page"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out, err := Crawl(context.Background(), srv.URL+"/index.php", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range routes {
		if !strings.Contains(out, "page="+p) {
			t.Errorf("router endpoint page=%s was collapsed/dropped:\n%s", p, out)
		}
	}
}
