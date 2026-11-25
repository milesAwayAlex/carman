// Package kube is the dispatcher's way to Kubernetes: a Starter that makes
// a Job from a CronJob's job template, and a Watcher that records in the
// log how every Job in the namespace ended. It speaks the API's JSON over
// net/http, as the pod's own account; nothing more is needed to create,
// read and watch Jobs.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
)

// Client is the least of a Kubernetes client: one namespace, JSON over HTTP.
type Client struct {
	Base      string // https://HOST:PORT
	Namespace string
	HTTP      *http.Client
	// Token returns the bearer token for a request. It is asked each time:
	// a pod's projected token is renewed on disk while the pod runs.
	Token func() (string, error)
}

const accountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// InCluster is a client as the pod's own account, in namespace (or the
// pod's own namespace when empty).
func InCluster(namespace string) (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not in a cluster: KUBERNETES_SERVICE_HOST is not set")
	}
	ca, err := os.ReadFile(accountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("the pod has no account token mounted: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("the account's ca.crt holds no certificate")
	}
	if namespace == "" {
		ns, err := os.ReadFile(accountDir + "/namespace")
		if err != nil {
			return nil, err
		}
		namespace = strings.TrimSpace(string(ns))
	}
	return &Client{
		Base:      "https://" + net.JoinHostPort(host, port),
		Namespace: namespace,
		HTTP:      &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
		Token: func() (string, error) {
			b, err := os.ReadFile(accountDir + "/token")
			return strings.TrimSpace(string(b)), err
		},
	}, nil
}

// APIError is a response other than success, with the API's Status.
type APIError struct {
	Code    int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kubernetes: %d %s: %s", e.Code, e.Reason, e.Message)
}

// IsStatus says whether err is an APIError with this HTTP code.
func IsStatus(err error, code int) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == code
}

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != nil {
		token, err := c.Token()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		var st struct{ Reason, Message string }
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &st) != nil {
			st.Message = strings.TrimSpace(string(b))
		}
		return nil, &APIError{Code: resp.StatusCode, Reason: st.Reason, Message: st.Message}
	}
	return resp, nil
}

// do sends body (when not nil) and decodes the answer into out (when not nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) path(resource string) string {
	group := "/apis/batch/v1"
	if resource == "pods" {
		group = "/api/v1"
	}
	return group + "/namespaces/" + c.Namespace + "/" + resource
}
