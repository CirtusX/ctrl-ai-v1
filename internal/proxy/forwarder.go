package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
)

// hopByHopHeaders are HTTP headers that must not be forwarded through a proxy.
// These are connection-specific and only relevant for the single hop.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// forwardRequest sends the request to the upstream LLM provider and returns
// the raw response. The caller is responsible for reading and closing the
// response body.
//
// Design doc Section 4.4:
//   - Build upstream URL: config.Providers[providerKey].Upstream + apiPath
//   - Copy all headers except hop-by-hop
//   - Body passes through untouched
func forwardRequest(client *http.Client, upstream string, r *http.Request, body []byte) (*http.Response, error) {
	// Create the upstream request with the same method and body.
	upstreamReq, err := http.NewRequestWithContext(
		r.Context(),
		r.Method,
		upstream,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("creating upstream request: %w", err)
	}

	// Copy headers from the original request, skipping hop-by-hop headers.
	copyHeaders(upstreamReq.Header, r.Header)

	// Set Content-Length since we have the full body.
	upstreamReq.ContentLength = int64(len(body))

	// Send to upstream LLM.
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return nil, fmt.Errorf("forwarding to upstream %s: %w", upstream, err)
	}

	return resp, nil
}

// copyHeaders copies HTTP headers from src to dst, skipping hop-by-hop
// headers that should not be forwarded through a proxy.
func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if hopByHopHeaders[key] {
			continue
		}
		// Also skip the Host header — it will be set by the HTTP client
		// based on the upstream URL.
		if strings.EqualFold(key, "Host") {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

// copyResponseHeaders copies response headers from the upstream response
// to the client response writer, skipping hop-by-hop headers.
func copyResponseHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		if hopByHopHeaders[key] {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}
