// Package tools implements the side-effecting actions exposed to the agent.
package tools

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"

func httpGet(ctx context.Context, rawurl string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return string(data), nil
}

var (
	reResultLink    = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reResultSnippet = regexp.MustCompile(`(?s)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	reTag           = regexp.MustCompile(`<[^>]+>`)
	reWS            = regexp.MustCompile(`[ \t\r\f\v]+`)
)

var reStripBlocks = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`),
	regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`),
	regexp.MustCompile(`(?is)<noscript[^>]*>.*?</noscript>`),
	regexp.MustCompile(`(?is)<!--.*?-->`),
}

// SearchDDG queries DuckDuckGo's HTML endpoint and returns ranked results.
func SearchDDG(ctx context.Context, query string, maxResults int) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("empty query")
	}
	if maxResults <= 0 {
		maxResults = 5
	}
	body, err := httpGet(ctx, "https://html.duckduckgo.com/html/?q="+url.QueryEscape(query))
	if err != nil {
		return "", err
	}

	links := reResultLink.FindAllStringSubmatch(body, -1)
	snippets := reResultSnippet.FindAllStringSubmatch(body, -1)
	if len(links) == 0 {
		return "No results found.", nil
	}

	var b strings.Builder
	for i := 0; i < len(links) && i < maxResults; i++ {
		title := cleanText(links[i][2])
		href := decodeDDGURL(links[i][1])
		snippet := ""
		if i < len(snippets) {
			snippet = cleanText(snippets[i][1])
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, title, href, snippet)
	}
	return strings.TrimSpace(b.String()), nil
}

// FetchPage fetches a URL and returns its text with markup stripped.
func FetchPage(ctx context.Context, rawurl string, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = 6000
	}
	body, err := httpGet(ctx, rawurl)
	if err != nil {
		return "", err
	}
	for _, re := range reStripBlocks {
		body = re.ReplaceAllString(body, " ")
	}
	body = reTag.ReplaceAllString(body, " ")
	body = html.UnescapeString(body)
	body = reWS.ReplaceAllString(body, " ")

	fields := strings.Fields(body)
	text := strings.Join(fields, " ")
	if len(text) > maxChars {
		text = text[:maxChars] + "\n…[truncated]"
	}
	if text == "" {
		return "(no extractable text)", nil
	}
	return text, nil
}

// decodeDDGURL unwraps DuckDuckGo's /l/?uddg= redirect into the real URL.
func decodeDDGURL(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if real := u.Query().Get("uddg"); real != "" {
		return real
	}
	return href
}

func cleanText(s string) string {
	s = reTag.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}
