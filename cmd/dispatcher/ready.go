package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// `dispatcher ready` waits until a pod just made can reach what it needs
// (ADR new-who-comes-in-and-a-new-pods-first-second). On this kind of
// cluster the network's policies know a new pod a moment after it starts
// (measured: 0.1 to 1.2 seconds): until then the services of its own
// installation refuse or drop its connections. Every pod that is a client
// runs this once, first, so that no client has to try again on its own.
//
//	dispatcher ready [--for 20s] [-- COMMAND ARG...]
//
// It asks each of DISPATCHER_URL and CMEM_URL for /healthz, and connects to
// the proxy HTTPS_PROXY names, until each has answered or the time is up.
// It never fails: what did not answer is said, and COMMAND, if given, is
// run in its place either way.
func readyCmd(ctx context.Context, args []string) error {
	wait := 20 * time.Second
	var command []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--":
			command = args[i+1:]
			i = len(args)
		case args[i] == "--for" && i+1 < len(args):
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return fmt.Errorf("--for: %w", err)
			}
			wait = d
			i++
		default:
			return fmt.Errorf("say: dispatcher ready [--for 20s] [-- COMMAND ARG...]")
		}
	}
	type target struct {
		name  string
		check func(context.Context) error
	}
	var targets []target
	get := func(base string) func(context.Context) error {
		return func(ctx context.Context) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/healthz", nil)
			if err != nil {
				return err
			}
			// Straight to it: these are inside the cluster.
			resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		}
	}
	for _, name := range []string{"DISPATCHER_URL", "CMEM_URL"} {
		if v := os.Getenv(name); v != "" {
			targets = append(targets, target{strings.ToLower(strings.TrimSuffix(name, "_URL")), get(v)})
		}
	}
	if v := os.Getenv("HTTPS_PROXY"); v != "" {
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			targets = append(targets, target{"the egress proxy", func(ctx context.Context) error {
				c, err := (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
				if err == nil {
					c.Close()
				}
				return err
			}})
		}
	}
	start, tries := time.Now(), 0
	for len(targets) > 0 && time.Since(start) < wait && ctx.Err() == nil {
		tries++
		var left []target
		for _, t := range targets {
			tctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			if err := t.check(tctx); err != nil {
				left = append(left, t)
			}
			cancel()
		}
		if targets = left; len(targets) > 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	switch {
	case len(targets) > 0:
		var names []string
		for _, t := range targets {
			names = append(names, t.name)
		}
		fmt.Fprintf(os.Stderr, "ready: after %s, still no answer from %s; going on\n", wait, strings.Join(names, ", "))
	case tries > 1:
		fmt.Fprintf(os.Stderr, "ready: reachable after %.1fs (a pod just made is not known to the network at once)\n", time.Since(start).Seconds())
	}
	if len(command) == 0 {
		return nil
	}
	path, err := exec.LookPath(command[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, command, os.Environ())
}
