package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Probe concurrently inspects URLs: status, timing, size, redirect chain, key
// response/security headers and (for https) the leaf TLS certificate. Pass many
// URLs to use it as a batch matrix. Goroutines + net/http/tls — Go's sweet spot.
func Probe(ctx context.Context, urls []string, timeoutSec int) (string, error) {
	if len(urls) == 0 {
		return "", fmt.Errorf("no urls")
	}
	if timeoutSec <= 0 {
		timeoutSec = 15
	}
	results := make([]string, len(urls))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = probeOne(ctx, u, timeoutSec)
		}(i, u)
	}
	wg.Wait()
	return strings.Join(results, "\n\n"), nil
}

func probeOne(ctx context.Context, u string, timeoutSec int) string {
	var chain []string
	client := &http.Client{
		Timeout: time.Duration(timeoutSec) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			chain = append(chain, req.URL.String())
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Sprintf("%s\n  error: %v", u, err)
	}
	req.Header.Set("User-Agent", browserUA)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("%s\n  error: %v", u, err)
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	ms := time.Since(start).Milliseconds()

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n  status: %s  (%dms, %d bytes)", u, resp.Status, ms, n)
	if len(chain) > 0 {
		fmt.Fprintf(&b, "\n  redirected to: %s", resp.Request.URL.String())
	}
	for _, h := range []string{"Server", "Content-Type", "Location", "Set-Cookie"} {
		if v := resp.Header.Get(h); v != "" {
			fmt.Fprintf(&b, "\n  %s: %s", h, clip(v, 120))
		}
	}
	var missing []string
	for _, h := range []string{"Strict-Transport-Security", "Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if resp.Header.Get(h) == "" {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, "\n  missing security headers: %s", strings.Join(missing, ", "))
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		c := resp.TLS.PeerCertificates[0]
		fmt.Fprintf(&b, "\n  tls: issuer=%q expires=%s", c.Issuer.CommonName, c.NotAfter.Format("2006-01-02"))
	}
	return b.String()
}

var (
	reHref   = regexp.MustCompile(`(?i)href=["']([^"'#>]+)`)
	reAction = regexp.MustCompile(`(?i)action=["']([^"'#>]+)`) // form POST/GET targets
	reSrc    = regexp.MustCompile(`(?i)\bsrc=["']([^"'#>]+)`)  // script/img/iframe sources
	reTitle  = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reLink   = []*regexp.Regexp{reHref, reAction, reSrc}
)

// maxPerShape bounds how many distinct URLs that share one canonical shape
// (path + query-param names) the crawler will enqueue, so parameterized URLs
// don't blow up the frontier.
const maxPerShape = 5

// canonShape collapses a URL to host+path plus its SORTED query params. A param
// with a numeric/empty value (pagination, ids: ?page=1..N) collapses by NAME so
// it can't explode the frontier; a param with a non-numeric value is treated as
// ROUTING (?page=login.php, ?action=admin) and kept by name=value so distinct
// endpoints are NOT collapsed — preserving recon coverage on router-style apps.
func canonShape(u *url.URL) string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v := q.Get(k); numericish(v) {
			parts = append(parts, k)
		} else {
			parts = append(parts, k+"="+v)
		}
	}
	return u.Host + u.Path + "?" + strings.Join(parts, "&")
}

// numericish reports whether a query value looks like pagination/an id (digits
// only, or empty) rather than a route token.
func numericish(v string) bool {
	if v == "" {
		return true
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Crawl does a bounded, concurrent same-host breadth-first crawl from start,
// returning discovered URLs with page titles (a quick site map).
func Crawl(ctx context.Context, start string, maxPages int) (string, error) {
	if maxPages <= 0 {
		maxPages = 20
	}
	base, err := url.Parse(start)
	if err != nil {
		return "", fmt.Errorf("bad start url: %w", err)
	}
	client := &http.Client{Timeout: 15 * time.Second}

	visited := map[string]bool{}
	enqueued := map[string]bool{}   // exact URLs already placed on the frontier
	shapeCount := map[string]int{}  // canonical shape → # of distinct URLs admitted
	type page struct{ url, title string }
	var out []page
	var mu sync.Mutex
	frontier := []string{start}
	enqueued[start] = true
	shapeCount[canonShape(base)]++ // the seed consumes one unit of its own shape budget

	for len(frontier) > 0 {
		// Bail the moment the run is cancelled/deadlined — otherwise the whole
		// remaining frontier is drained as fast-failing cancelled requests
		// instead of returning what was gathered.
		if ctx.Err() != nil {
			break
		}
		mu.Lock()
		done := len(out) >= maxPages
		mu.Unlock()
		if done {
			break
		}
		batch := frontier
		frontier = nil
		sem := make(chan struct{}, 8)
		var wg sync.WaitGroup
		for _, pu := range batch {
			mu.Lock()
			skip := visited[pu] || len(out)+len(visited) > maxPages*4
			visited[pu] = true
			mu.Unlock()
			if skip {
				continue
			}
			wg.Add(1)
			go func(pu string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				body := fetchBody(ctx, client, pu)
				title := ""
				if m := reTitle.FindStringSubmatch(body); m != nil {
					title = clip(cleanText(m[1]), 80)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(out) < maxPages {
					out = append(out, page{pu, title})
				}
				for _, re := range reLink {
					for _, m := range re.FindAllStringSubmatch(body, -1) {
						link := resolveURL(pu, m[1])
						lu, err := url.Parse(link)
						if err != nil || lu.Host != base.Host || visited[link] || enqueued[link] {
							continue
						}
						// Cap distinct URLs sharing one shape (same path + query-param
						// NAMES) so e.g. ?page=1..N or ?id=… can't explode the frontier,
						// while genuinely distinct endpoints stay distinct.
						shape := canonShape(lu)
						if shapeCount[shape] >= maxPerShape {
							continue
						}
						shapeCount[shape]++
						enqueued[link] = true
						frontier = append(frontier, link)
					}
				}
			}(pu)
		}
		wg.Wait()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "crawled %d pages from %s:\n", len(out), start)
	for _, p := range out {
		if p.title != "" {
			fmt.Fprintf(&b, "  %s — %s\n", p.url, p.title)
		} else {
			fmt.Fprintf(&b, "  %s\n", p.url)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func fetchBody(ctx context.Context, client *http.Client, u string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return string(data)
}

func resolveURL(base, href string) string {
	b, err := url.Parse(base)
	if err != nil {
		return href
	}
	r, err := url.Parse(href)
	if err != nil {
		return href
	}
	return b.ResolveReference(r).String()
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
