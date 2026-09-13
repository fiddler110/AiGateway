// Package httpclient builds the single shared *http.Client used for every
// upstream call (chat forwarding, health checks, semantic-cache embedding
// calls), so connection pooling is shared across all of them rather than
// creating a new client (and connection pool) per call.
package httpclient

import "net/http"

// New builds a shared client with a connection pool sized for a home-lab
// gateway serving a modest number of upstreams and clients. Individual
// calls set their own per-request timeout via context, since upstream
// timeouts vary (short for health checks, long/unbounded for streaming).
func New() *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     100,
	}
	return &http.Client{Transport: transport}
}
