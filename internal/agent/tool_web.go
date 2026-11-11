package agent

import (
	"context"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/store"
)

var fetchTool = &Tool{
	Parallel: true,
	Def: llm.Tool{
		Name: "web_fetch",
		Description: "Fetch a URL and return its content as readable text (HTML is converted, scripts and styling removed). " +
			"Other file types (up to 10 MB) are saved to disk and the path returned. For pages that need JavaScript, a login or interaction, use `artus browser` instead.",
		Schema: schema(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`),
	},
	Run: runFetch,
}

var httpClient = &http.Client{Timeout: 60 * time.Second}

func runFetch(ctx context.Context, c *Call) Result {
	var in struct{ URL string }
	if err := c.args(&in); err != nil {
		return fail("%v", err)
	}
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fail("%q is not an http(s) URL.", in.URL)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Artus)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.9,*/*;q=0.5")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fail("Could not fetch %s: %v", u, err)
	}
	defer resp.Body.Close()
	const maxBytes = 10 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fail("Reading %s failed: %v", u, err)
	}
	truncated := len(body) > maxBytes
	body = body[:min(len(body), maxBytes)]

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = http.DetectContentType(body)
	}
	head := fmt.Sprintf("%s — HTTP %d, %s\n\n", resp.Request.URL, resp.StatusCode, mediaType)
	var text string
	switch {
	case strings.Contains(mediaType, "html"):
		text = htmlToText(string(body))
	case strings.HasPrefix(mediaType, "text/"), strings.Contains(mediaType, "json"), strings.Contains(mediaType, "xml"),
		strings.Contains(mediaType, "javascript"), strings.Contains(mediaType, "yaml"):
		text = string(body)
	default:
		name := path.Base(resp.Request.URL.Path)
		if name == "" || name == "/" || name == "." || !store.ValidID(strings.ReplaceAll(name, ".", "_")) {
			name = "download"
		}
		dst := c.e.Home.Path("tmp", store.RandomID(3)+"-"+name)
		if err := os.WriteFile(dst, body, 0o600); err != nil {
			return fail("%v", err)
		}
		if truncated {
			return Result{Text: fmt.Sprintf("%sStopped at 10 MB: the file is larger, and the first 10 MB are in %s. Download it with curl in bash instead.", head, c.e.display(dst)), IsError: true}
		}
		return Result{Text: fmt.Sprintf("%sSaved %d bytes to %s. Use read (images, PDFs) or bash to inspect it.", head, len(body), c.e.display(dst)), IsError: resp.StatusCode >= 400}
	}
	if truncated {
		text += "\n[truncated at 10 MB]"
	}
	if strings.TrimSpace(text) == "" {
		text = "(the page has no readable text; it may need JavaScript — try `artus browser`)"
	}
	return Result{Text: head + text, IsError: resp.StatusCode >= 400}
}

var (
	reDrop    = regexp.MustCompile(`(?is)<(script|style|noscript|svg|template|iframe|head)\b.*?</(script|style|noscript|svg|template|iframe|head)\s*>|<!--.*?-->`)
	reAnchor  = regexp.MustCompile(`(?is)<a\b[^>]*?href\s*=\s*["']?([^"'\s>]+)["']?[^>]*>(.*?)</a\s*>`)
	reHeading = regexp.MustCompile(`(?is)<h([1-6])\b[^>]*>(.*?)</h[1-6]\s*>`)
	reItem    = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	reCell    = regexp.MustCompile(`(?i)</t[dh]\s*>`)
	reBreak   = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|section|article|header|footer|main|aside|nav|tr|li|ul|ol|table|blockquote|pre|h[1-6]|form|figure|dd|dt)\s*>`)
	reTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces  = regexp.MustCompile(`[ \t\f\v\x{00a0}]+`)
	reBlank   = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)
)

// htmlToText reduces a page to readable text: headings and list items keep
// simple Markdown markers and links keep their target.
func htmlToText(src string) string {
	s := reDrop.ReplaceAllString(src, " ")
	s = reHeading.ReplaceAllStringFunc(s, func(m string) string {
		g := reHeading.FindStringSubmatch(m)
		return "\n\n" + strings.Repeat("#", int(g[1][0]-'0')) + " " + g[2] + "\n\n"
	})
	s = reAnchor.ReplaceAllStringFunc(s, func(m string) string {
		g := reAnchor.FindStringSubmatch(m)
		label := strings.TrimSpace(reTag.ReplaceAllString(g[2], " "))
		href := g[1]
		if label == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") {
			return label
		}
		return "[" + label + "](" + href + ")"
	})
	s = reItem.ReplaceAllString(s, "\n- ")
	s = reCell.ReplaceAllString(s, " | ")
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reSpaces.ReplaceAllString(s, " ")
	s = reBlank.ReplaceAllString(s, "\n\n")
	var out []string
	for _, line := range strings.Split(s, "\n") {
		out = append(out, strings.TrimSpace(line))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
