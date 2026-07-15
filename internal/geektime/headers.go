package geektime

import (
	"net/http"
	"strings"

	"github.com/go-resty/resty/v2"
)

const (
	// RefererHeader ...
	RefererHeader = "Referer"
	// AcceptHeader ...
	AcceptHeader = "Accept"
	// AcceptLanguageHeader ...
	AcceptLanguageHeader = "Accept-Language"
	// ContentTypeHeader ...
	ContentTypeHeader = "Content-Type"

	// DefaultReferer is the referer used by time.geekbang.org API calls.
	DefaultReferer = "https://time.geekbang.org/"
	// DefaultAccept ...
	DefaultAccept = "application/json, text/plain, */*"
	// DefaultAcceptLanguage ...
	DefaultAcceptLanguage = "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7"
	// DefaultUserAgent uses a recent Chrome UA to reduce bot blocking.
	DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// SERVERID optional session cookie from browser.
	SERVERID = "SERVERID"
)

// ApplyBrowserHeaders sets headers that geektime expects from browser clients.
func ApplyBrowserHeaders(c *resty.Client) {
	c.SetHeader(UserAgent, DefaultUserAgent)
	c.SetHeader(AcceptHeader, DefaultAccept)
	c.SetHeader(AcceptLanguageHeader, DefaultAcceptLanguage)
}

// CookieHeaderValue formats cookies for the Cookie request header.
func CookieHeaderValue(cookies []*http.Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		if c.Name == "" || c.Value == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}
