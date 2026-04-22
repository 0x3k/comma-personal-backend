package metrics

import (
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

// EchoMiddleware returns Echo middleware that records each request's
// method, route, status code, and latency.
//
// The route template (e.g. "/v1/route/:route_id") is preferred over the raw
// request path so label cardinality stays bounded. /metrics is skipped so
// requests to the metrics endpoint do not pollute the histograms.
func EchoMiddleware(m *Metrics) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Path() == "/metrics" {
				return next(c)
			}

			start := time.Now()
			err := next(c)
			dur := time.Since(start)

			status := c.Response().Status
			if err != nil {
				// Echo's HTTPError sets Response().Status via the default error
				// handler only after the middleware chain unwinds. Pull the code
				// from the error directly so we record 4xx/5xx accurately.
				if he, ok := err.(*echo.HTTPError); ok {
					status = he.Code
				} else if status == 0 {
					status = 500
				}
			}

			// Use the matched route template if available to keep cardinality
			// bounded. For unmatched routes c.Path() is the empty string.
			path := c.Path()
			if path == "" {
				path = "unmatched"
			}

			m.ObserveHTTPRequest(c.Request().Method, path, strconv.Itoa(status), dur)
			return err
		}
	}
}
