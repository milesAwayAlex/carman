package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"carman/internal/dispatch/api"
	"carman/internal/dispatch/message"
)

// config is where the installation is, from the environment.
type config struct {
	Context, Namespace, Kubectl, Checkout string
	URL, Token                            string
	// Term and Host say what attaches, for the pod: the terminal's kind,
	// and a name for this place, so that a client a dropped connection
	// left behind is ended by the next attach from here.
	Term, Host string
}

func or(a ...string) string {
	for _, v := range a {
		if v != "" {
			return v
		}
	}
	return ""
}

func configFrom(getenv func(string) string) config {
	host, _ := os.Hostname()
	return config{
		Context:   getenv("CARMAN_CONTEXT"),
		Namespace: or(getenv("CARMAN_NAMESPACE"), "carman"),
		Kubectl:   or(getenv("CARMAN_KUBECTL"), "kubectl"),
		Checkout:  getenv("CARMAN_CHECKOUT"),
		URL:       or(getenv("CARMAN_DISPATCHER_URL"), getenv("DISPATCHER_URL")),
		Token:     or(getenv("CARMAN_DISPATCHER_TOKEN"), getenv("DISPATCHER_TOKEN")),
		Term:      getenv("TERM"),
		Host:      host,
	}
}

// Exit statuses. A view in the pod that cannot be had (no such window, a
// headless run) says so with viewRefused, which is never tried again.
const (
	exitLeft    = 0
	exitError   = 1
	exitNoMatch = 2
	viewRefused = 3
	// exitCancelled: the picker was left with nothing chosen (as a shell
	// says of an interrupt).
	exitCancelled = 130
)

var errCancelled = errors.New("nothing chosen")

// attach is what is run to go to a session: the command, and the
// directory to run it in when it matters.
func attach(c config, s session) (argv []string, dir string, err error) {
	if c.Context == "" {
		return nil, "", errors.New("CARMAN_CONTEXT is not set: the kubectl context of the installation's cluster")
	}
	switch {
	case s.Running && s.Mode != "interactive":
		return nil, "", fmt.Errorf("%s runs headless in %s: there is no terminal to go to. It can be gone to once it has ended, as a kept conversation", s.By(), s.Pod)
	case s.Running || s.Standing:
		if s.Pod == "" {
			return nil, "", fmt.Errorf("the dispatcher does not say which pod %s is in", s.By())
		}
		// A view of its own in the pod's tmux, at that session's window.
		argv = []string{c.Kubectl, "--context", c.Context, "--namespace", c.Namespace, "exec", "-it", s.Pod, "--",
			"env", "AGENT_TERM=" + c.Term, "AGENT_ATTACH_ID=" + c.Host + "/" + s.By(), "AGENT_VIEW_BEAT=1", "agent-view", s.ID, s.Name}
		return argv, "", nil
	}
	// A job's kept conversation goes on as its agent in a pod of its own
	// (ADR 0040): the checkout's script does that, until this program does.
	if c.Checkout == "" {
		return nil, "", fmt.Errorf("%s is a job's kept conversation: going on with it is done by a checkout's script for now. "+
			"Set CARMAN_CHECKOUT to a checkout of the repository, or run there: scripts/deploy.sh %s agent resume %s", s.By(), c.Context, short(s.ID))
	}
	return []string{filepath.Join(c.Checkout, "scripts", "deploy.sh"), c.Context, "agent", "resume", s.ID}, c.Checkout, nil
}

func fetch(ctx context.Context, c config) ([]session, error) {
	if c.URL == "" || c.Token == "" {
		return nil, errors.New("the dispatcher's address and token are not set: CARMAN_DISPATCHER_URL and CARMAN_DISPATCHER_TOKEN, or DISPATCHER_URL and DISPATCHER_TOKEN")
	}
	cl := &api.Client{URL: c.URL, Token: c.Token, HTTP: &http.Client{Timeout: 15 * time.Second}}
	var peers []message.Peer
	if err := cl.Do(ctx, http.MethodGet, "/v1/peers", nil, nil, &peers); err != nil {
		return nil, fmt.Errorf("the dispatcher at %s: %w", c.URL, err)
	}
	return sessions(peers), nil
}

// pick lets the owner choose: fzf when it is here and there is a terminal,
// a numbered list otherwise. No choice is an error.
func pick(all []session, in io.Reader, out io.Writer, tty bool) (session, error) {
	if len(all) == 0 {
		return session{}, errors.New("no session to go to")
	}
	now := time.Now()
	lines := make([]string, len(all))
	for i, s := range all {
		lines[i] = s.line(now)
	}
	if fzf, err := exec.LookPath("fzf"); err == nil && tty {
		cmd := exec.Command(fzf, "--no-sort", "--no-multi", "--height=40%", "--reverse", "--prompt=go to> ")
		cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
		cmd.Stderr = os.Stderr
		chosen, err := cmd.Output()
		if err != nil || len(strings.Fields(string(chosen))) == 0 {
			return session{}, errCancelled
		}
		return resolve(all, strings.Fields(string(chosen))[0])
	}
	for i, l := range lines {
		fmt.Fprintf(out, "%2d  %s\n", i+1, l)
	}
	fmt.Fprint(out, "go to (number, or nothing to leave): ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || n < 1 || n > len(all) {
		return session{}, errCancelled
	}
	return all[n-1], nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func goCmd(args []string) int {
	var keep, list, names, print bool
	var word string
	for _, a := range args {
		switch a {
		case "--keep":
			keep = true
		case "--list":
			list = true
		case "--names":
			names = true
		case "--print":
			print = true
		default:
			if strings.HasPrefix(a, "-") || word != "" {
				fmt.Fprintf(os.Stderr, "carman go: %q is not understood\n%s\n", a, usage)
				return exitNoMatch
			}
			word = a
		}
	}
	c := configFrom(os.Getenv)
	fail := func(code int, err error) int {
		fmt.Fprintln(os.Stderr, "carman go:", err)
		return code
	}
	ctx := context.Background()
	all, err := fetch(ctx, c)
	if err != nil {
		return fail(exitError, err)
	}
	if list {
		now := time.Now()
		for _, s := range all {
			if names {
				fmt.Println(s.By())
			} else {
				fmt.Println(s.line(now))
			}
		}
		return exitLeft
	}
	var s session
	if word != "" {
		if s, err = resolve(all, word); err != nil {
			return fail(exitNoMatch, err)
		}
	} else if s, err = pick(all, os.Stdin, os.Stderr, isTerminal(os.Stdin) && isTerminal(os.Stderr)); errors.Is(err, errCancelled) {
		return exitCancelled
	} else if err != nil {
		return fail(exitNoMatch, err)
	}
	if print {
		fmt.Println(s.By())
		return exitLeft
	}
	return stay(ctx, c, s, keep)
}

// stay goes to the session and, with keep, goes again after a dropped
// connection until the owner leaves.
//
// How a deliberate leave is told from a drop: by how the attach ended. The
// pod's side ends with status 0 when its view was left (detached, or the
// session's window closed), and with viewRefused when there is nothing to
// show; kubectl gives those back. Anything else (kubectl's own failure
// when the connection goes, a signal) is a drop. A signal to this program
// itself (the terminal closed, an interrupt while it waits) is a leave.
func stay(ctx context.Context, c config, s session, keep bool) int {
	left := make(chan os.Signal, 2)
	signal.Notify(left, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	by := s.By()
	wait, quick := firstWait, 0
	for {
		argv, dir, err := attach(c, s)
		if err != nil {
			fmt.Fprintln(os.Stderr, "carman go:", err)
			return exitError
		}
		began := time.Now()
		code, gone := run(argv, dir, left)
		if gone {
			return exitLeft
		}
		switch {
		case code == 0:
			return exitLeft
		case code == viewRefused:
			return viewRefused
		case !keep:
			fmt.Fprintf(os.Stderr, "carman go: the way to %s ended (status %d); carman go --keep %s attaches again by itself\n", by, code, by)
			return exitError
		}
		// A drop. Attach again, unless it keeps failing at once.
		if time.Since(began) < quickFailure {
			if quick++; quick >= 5 {
				fmt.Fprintf(os.Stderr, "carman go: the way to %s failed five times at once; giving up\n", by)
				return exitError
			}
		} else {
			wait, quick = firstWait, 0
		}
		fmt.Fprintf(os.Stderr, "\r\ncarman go: the connection to %s dropped; attaching again in %s (interrupt to leave)\r\n", by, wait)
		select {
		case <-left:
			return exitLeft
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
		// It may be another session of that name by now, in another pod.
		if all, err := fetch(ctx, c); err == nil {
			if again, err := resolve(all, by); err == nil {
				s = again
			}
		}
	}
}

// How long before attaching again after a drop (doubling to half a minute),
// and how short an attach counts as having failed at once.
var (
	firstWait    = 2 * time.Second
	quickFailure = 5 * time.Second
)

// beat is the command that tells the pod, beside an attach, that this end
// is there (agent-view --beat): the attach's own, with no terminal, and
// --beat in place of the session. Nothing for what is not a view.
func beat(argv []string) []string {
	i := len(argv) - 3
	if i < 0 || argv[i] != "agent-view" {
		return nil
	}
	var out []string
	for _, a := range argv[:i] {
		switch a {
		case "-it":
			a = "-i"
		case "AGENT_VIEW_BEAT=1":
			continue
		}
		out = append(out, a)
	}
	return append(out, "agent-view", "--beat")
}

// beatEvery is how often the pod is told this end is there. A view whose
// far end is silent for a minute and a half is ended there, so that one a
// dropped connection left does not stay attached to nothing.
var beatEvery = 20 * time.Second

// run runs a command on this terminal and gives its exit status; one
// that was killed, or could not start, is 255. Beside a view, it keeps
// the pod told that this end is there.
//
// When this end goes (a signal on left: the terminal was closed, or the
// program told to end; or the terminal itself is no longer there), what
// it runs is ended with it, and gone says so: kubectl does not end by
// itself when its terminal is hung up (seen 2026-11-27: it stayed behind
// a closed window, and its view stayed in the pod).
func run(argv []string, dir string, left <-chan os.Signal) (code int, gone bool) {
	// bye tells the pod this end has left, so that the view goes at once
	// and not when its beat has been silent long enough.
	bye := func() {}
	if b := beat(argv); b != nil {
		hb := exec.Command(b[0], b[1:]...)
		if in, err := hb.StdinPipe(); err == nil && hb.Start() == nil {
			// Waited for at once: one that ends early (a pod from before
			// the beat) is not left as a dead child for the attach's length.
			ended := make(chan struct{})
			go func() { hb.Wait(); close(ended) }()
			stop := make(chan struct{})
			go func() {
				for {
					if _, err := io.WriteString(in, "\n"); err != nil {
						return
					}
					select {
					case <-stop:
						return
					case <-time.After(beatEvery):
					}
				}
			}()
			bye = func() {
				if _, err := io.WriteString(in, "bye\n"); err != nil {
					return
				}
				select {
				case <-ended:
				case <-time.After(3 * time.Second):
				}
			}
			defer func() {
				close(stop)
				in.Close()
				hb.Process.Kill()
				<-ended
			}()
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "carman go:", err)
		return 255, false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	hadTerminal := terminalThere()
	look := time.NewTicker(terminalLook)
	defer look.Stop()
	for {
		select {
		case err := <-done:
			var ee *exec.ExitError
			switch {
			case err == nil:
				return 0, false
			case errors.As(err, &ee) && ee.ExitCode() >= 0:
				return ee.ExitCode(), false
			}
			return 255, false
		case <-left:
			bye()
			end(cmd, done)
			return 0, true
		case <-look.C:
			if hadTerminal && !terminalThere() {
				bye()
				end(cmd, done)
				return 0, true
			}
		}
	}
}

// terminalLook is how often it is checked that the terminal is still there.
var terminalLook = 3 * time.Second

// terminalThere says whether this process still has its terminal: one
// that was hung up can no longer be opened.
func terminalThere() bool {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// end stops a command: asked first, then made to.
func end(cmd *exec.Cmd, done <-chan error) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}
