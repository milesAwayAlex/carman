package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Client calls the service as one role.
type Client struct {
	URL   string
	Token string
	// TokenFile, when set, is read at each call and its content used in
	// place of Token: a pod's token for the dispatcher is replaced by the
	// cluster before it expires, and a command that runs for hours (a
	// session's listener, the watcher) must not go on showing the first.
	TokenFile string
	HTTP      *http.Client
}

// token is what this call shows: the file's content now, else Token.
func (c *Client) token() string {
	if c.TokenFile != "" {
		if b, err := os.ReadFile(c.TokenFile); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" {
				return t
			}
		}
	}
	return c.Token
}

// Do sends body (when not nil) and decodes the answer into out (when not
// nil); an answer that is not a success is an error with the service's reason.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.Raw(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err := io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Raw is Do without decoding: the caller reads and closes the body.
func (c *Client) Raw(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	u := strings.TrimRight(c.URL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, err := h.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		var e struct{ Error string }
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, e.Error)
	}
	return resp, nil
}
