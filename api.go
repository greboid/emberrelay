package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

const pageSize = 100

var errRateLimited = errors.New("rate limited")

type statusError struct {
	method, path string
	status       int
	body         string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.method, e.path, e.status, e.body)
}

func hasStatus(err error, statuses ...int) bool {
	if se, ok := errors.AsType[*statusError](err); ok {
		if slices.Contains(statuses, se.status) {
			return true
		}
	}
	return false
}

type apiClient struct {
	client  *http.Client
	baseURL string
	headers map[string]string
}

func (c *apiClient) do(ctx context.Context, method, path string, in, out any) (http.Header, error) {
	var reqBody io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode >= 300 {
		if (res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests) &&
			(res.Header.Get("Retry-After") != "" || res.Header.Get("X-RateLimit-Remaining") == "0") {
			return nil, fmt.Errorf("%s %s: %w", method, path, errRateLimited)
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, &statusError{method: method, path: path, status: res.StatusCode, body: string(body)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return res.Header, nil
	}
	return res.Header, json.NewDecoder(res.Body).Decode(out)
}

// list fetches every page. Only the presence of rel="next" is used, as Forgejo
// builds Link URLs from its ROOT_URL, which may differ from the configured URL.
func list[T any](ctx context.Context, c *apiClient, path string) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}

	var all []T
	for page := 1; ; page++ {
		var items []T
		url := fmt.Sprintf("%s%sper_page=%d&limit=%d&page=%d", path, sep, pageSize, pageSize, page)
		headers, err := c.do(ctx, http.MethodGet, url, nil, &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if !strings.Contains(headers.Get("Link"), `rel="next"`) {
			return all, nil
		}
	}
}
