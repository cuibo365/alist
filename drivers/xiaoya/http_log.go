package xiaoya

import (
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/go-resty/resty/v2"
)

// httpLogSnippet caps the response body echoed into the info log so a single
// exchange always stays a single line.
const httpLogSnippet = 256

// logResp logs one outbound HTTP exchange of the custom xiaoya-compatible
// drivers at info level: "[xiaoya/<tag>] <METHOD> <url> -> <status> <snippet>".
// Transport errors are logged too. Bodies are truncated and line breaks are
// flattened. Request headers (cookies / tokens) are never logged.
func logResp(tag string, res *resty.Response, err error) {
	if err != nil {
		log.Infof("[xiaoya/%s] HTTP error: %v", tag, err)
		return
	}
	if res == nil || res.RawResponse == nil {
		log.Infof("[xiaoya/%s] no response object", tag)
		return
	}
	method, url := "", ""
	if res.Request != nil {
		method, url = res.Request.Method, res.Request.URL
	}
	body := res.String()
	if len(body) > httpLogSnippet {
		body = body[:httpLogSnippet] + "...(truncated)"
	}
	body = strings.ReplaceAll(body, "\n", " ")
	log.Infof("[xiaoya/%s] %s %s -> %d %s", tag, method, url, res.RawResponse.StatusCode, body)
}

// logBodyInfo logs a manually-read response body (e.g. the decrypted 115
// initupload envelope) with the same format as logResp.
func logBodyInfo(tag, method, url string, status int, body string) {
	if len(body) > httpLogSnippet {
		body = body[:httpLogSnippet] + "...(truncated)"
	}
	body = strings.ReplaceAll(body, "\n", " ")
	log.Infof("[xiaoya/%s] %s %s -> %d %s", tag, method, url, status, body)
}
