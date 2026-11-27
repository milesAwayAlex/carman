package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"carman/internal/dispatch/trigger"
)

// A watch's job (ADR 0063): ask a source, keep what a filter keeps, and
// tell the dispatcher its digest. No model, and nothing of the source's
// content leaves this process but the digest and a count.

// watchSpec is the WATCH variable: a JSON object the chart writes.
type watchSpec struct {
	Name string `json:"name"`
	// URL is asked with GET, through the proxy the environment names; or
	// Command is run and its standard output is what was seen.
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Command []string          `json:"command"`
	// Keep is a regular expression: every match is kept, in order, and
	// the digest is of the matches. Empty keeps everything.
	Keep string `json:"keep"`
	// AtLeast is how many matches (or bytes, with no Keep) a reading must
	// have to count: a page that came back as an error page is not a change.
	AtLeast int    `json:"atLeast"`
	Timeout string `json:"timeout"`
}

// look asks the source and returns the digest of what was kept, and how much.
func look(ctx context.Context, w watchSpec) (trigger.Observation, error) {
	var ob trigger.Observation
	timeout, err := parseDuration(w.Timeout, 2*time.Minute)
	if err != nil {
		return ob, fmt.Errorf("timeout: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body []byte
	switch {
	case (w.URL != "") == (len(w.Command) > 0):
		return ob, errors.New("a watch has a url or a command, one of them")
	case w.URL != "":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.URL, nil)
		if err != nil {
			return ob, err
		}
		for k, v := range w.Headers {
			req.Header.Set(k, v)
		}
		resp, err := sourceClient().Do(req)
		if err != nil {
			return ob, err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return ob, fmt.Errorf("the source answered %s", resp.Status)
		}
		if body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20)); err != nil {
			return ob, err
		}
	default:
		cmd := exec.CommandContext(ctx, w.Command[0], w.Command[1:]...)
		cmd.Stderr = os.Stderr
		if body, err = cmd.Output(); err != nil {
			return ob, fmt.Errorf("the command: %w", err)
		}
	}
	h := sha256.New()
	if w.Keep == "" {
		h.Write(body)
		ob.Kept = len(body)
	} else {
		re, err := regexp.Compile(w.Keep)
		if err != nil {
			return ob, fmt.Errorf("keep: %w", err)
		}
		for _, m := range re.FindAll(body, -1) {
			h.Write(m)
			h.Write([]byte{'\n'})
			ob.Kept++
		}
	}
	if ob.Kept < w.AtLeast {
		return ob, fmt.Errorf("the reading kept %d, fewer than the %d a reading must have to count; nothing is said of it", ob.Kept, w.AtLeast)
	}
	ob.Digest = hex.EncodeToString(h.Sum(nil))[:32]
	return ob, nil
}

// sourceCmd is `dispatcher source [--print]`: one look at the watch WATCH
// describes, told to the dispatcher (or, with --print, only printed).
func sourceCmd(ctx context.Context, flags map[string][]string) error {
	var w watchSpec
	dec := json.NewDecoder(strings.NewReader(os.Getenv("WATCH")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil || w.Name == "" {
		return fmt.Errorf("WATCH is a JSON object: name, url or command, keep, atLeast, timeout (%v)", err)
	}
	// A pod just made may be refused its first connections, until the
	// cluster's network knows it: a look that could not connect is tried
	// again for a little while. A source that answers badly is not.
	var ob trigger.Observation
	err := retry(ctx, func() (err error) { ob, err = look(ctx, w); return err })
	if err != nil {
		return fmt.Errorf("watch %s: %w", w.Name, err)
	}
	if _, ok := flags["print"]; ok {
		fmt.Printf("watch %s: kept %d, digest %s\n", w.Name, ob.Kept, ob.Digest)
		return nil
	}
	c, err := client()
	if err != nil {
		return err
	}
	var out struct{ Changed bool }
	if err := retry(ctx, func() error {
		return c.Do(ctx, http.MethodPost, "/v1/watches/"+url.PathEscape(w.Name), nil, ob, &out)
	}); err != nil {
		return err
	}
	said := "as it was"
	if out.Changed {
		said = "CHANGED"
	}
	fmt.Printf("watch %s: kept %d, digest %s: %s\n", w.Name, ob.Kept, ob.Digest, said)
	return nil
}

// sourceClient asks through the proxy the environment names, and trusts,
// beside the system's authorities, the egress proxy's own (EGRESS_CA): the
// proxy answers in the name of a host it holds a credential for.
func sourceClient() *http.Client {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if f := os.Getenv("EGRESS_CA"); f != "" {
		if pem, err := os.ReadFile(f); err == nil {
			pool.AppendCertsFromPEM(pem)
		}
	}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{RootCAs: pool}}}
}

// retry runs f again, a few times two seconds apart, while it fails for
// not connecting.
func retry(ctx context.Context, f func() error) error {
	var err error
	for try := 0; try < 6; try++ {
		var netErr *net.OpError
		if err = f(); err == nil || !errors.As(err, &netErr) || netErr.Op != "dial" {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(2 * time.Second):
		}
	}
	return err
}
