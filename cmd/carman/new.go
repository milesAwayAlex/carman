package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"carman/internal/dispatch/message"
)

// opening is what `carman go --new NAME` was asked: a session of that name,
// in a standing agent's pod, in a directory there, on a model.
type opening struct {
	Name, In, Dir, Model string
}

// standingAgent is an agent whose pod holds sessions, and that pod.
type standingAgent struct {
	Name, Pod string
}

// standing are the agents a session can be opened in, by name, from what
// the dispatcher says: an agent that says it is a standing one, and the
// agent of any session that says so (a dispatcher from before agents said
// it). The pod is the one its sessions were last seen in, or else the one
// pod of the agent's StatefulSet, as the chart names it. With a dispatcher
// from before agents said it, a standing agent that has had no session is
// not among them.
func standing(peers []message.Peer) (agents []standingAgent) {
	pods := map[string]string{}
	for _, p := range peers {
		switch {
		case p.Kind == "agent" && p.Standing:
			if _, ok := pods[p.Agent]; !ok {
				pods[p.Agent] = ""
			}
		case p.Kind == "session" && p.Standing && p.Agent != "":
			// Running sessions come first, each kind latest first: the first
			// pod said is the latest.
			if pods[p.Agent] == "" {
				pods[p.Agent] = p.Pod
			}
		}
	}
	for name, pod := range pods {
		agents = append(agents, standingAgent{Name: name, Pod: or(pod, podOf(name))})
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	return agents
}

// podOf is the pod of a standing agent: the one pod of its StatefulSet.
func podOf(agent string) string { return "agent-" + agent + "-0" }

// choose is the agent a new session is opened in: the one named; the only
// one, when one is all there is; with several and none named, the one the
// owner picks, or, with no terminal to ask on, an error that lists them.
func choose(peers []message.Peer, in string, ask io.Reader, out io.Writer, tty bool) (standingAgent, error) {
	agents := standing(peers)
	var names []string
	for _, a := range agents {
		names = append(names, a.Name)
	}
	list := strings.Join(names, ", ")
	if in != "" {
		for _, a := range agents {
			if a.Name == in {
				return a, nil
			}
		}
		if list == "" {
			return standingAgent{}, fmt.Errorf("%s is not a standing agent: the dispatcher knows of none", in)
		}
		return standingAgent{}, fmt.Errorf("%s is not a standing agent; sessions can be opened in: %s", in, list)
	}
	switch {
	case len(agents) == 0:
		return standingAgent{}, errors.New("the dispatcher knows of no standing agent to open a session in (an agent with interactive: true); --in AGENT names one")
	case len(agents) == 1:
		return agents[0], nil
	case !tty:
		return standingAgent{}, fmt.Errorf("say which agent with --in AGENT: sessions can be opened in %s", list)
	}
	for i, a := range agents {
		fmt.Fprintf(out, "%2d  %-24s %s\n", i+1, a.Name, a.Pod)
	}
	fmt.Fprint(out, "open it in (number, or nothing to leave): ")
	answer, _ := bufio.NewReader(ask).ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || n < 1 || n > len(agents) {
		return standingAgent{}, errCancelled
	}
	return agents[n-1], nil
}

// What agent-open takes for a name and for a model.
var (
	sessionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	modelName   = regexp.MustCompile(`^[][A-Za-z0-9._:-]+$`)
)

// nameTaken is how the pod says the name is a session of its own already.
const nameTaken = 5

// openInPod is what runs in the pod, with the name, the directory and the
// model as its arguments (an empty one is not given). The pod's sessions
// start in ~/work, and so does this: a directory that is not absolute is
// one there. A name the pod remembers, or has a window of, is refused
// before anything is changed: agent-open would give a remembered name the
// new directory and leave its window as it is. Then agent-open, as the
// pod's sessions are opened: the session is remembered, and opened again
// when the pod starts.
const openInPod = `cd "$HOME/work" 2>/dev/null || cd "$HOME" || exit 1
if agent-open | awk -v n="$1" '$1 == n {found = 1} END {exit !found}'; then
    echo "a session called $1 is remembered in this pod, in $(agent-open | awk -v n="$1" '$1 == n {print $2; exit}')" >&2
    exit 5
fi
tmux="${AGENT_TMUX:-$(cat /tmp/agent-tmux 2>/dev/null || echo tmux)}"
if "$tmux" -S "/tmp/tmux-$(id -u)/default" list-windows -t main -F '#{window_name}' 2>/dev/null | grep -qxF -- "$1"; then
    echo "a window called $1 is open in this pod" >&2
    exit 5
fi
[ -d "${2:-.}" ] || { echo "no directory $2 in this pod (from $(pwd))" >&2; exit 1; }
[ -z "$3" ] || exec agent-open --model "$3" "$1" "${2:-.}"
exec agent-open "$1" "${2:-.}"`

// openCommand is what is run to open the session: no terminal, the name,
// directory and model as arguments of the pod's script, never in its text.
func openCommand(c config, a standingAgent, o opening) []string {
	return []string{c.Kubectl, "--context", c.Context, "--namespace", c.Namespace, "exec", a.Pod, "--",
		"sh", "-c", openInPod, "agent-open", o.Name, o.Dir, o.Model}
}

// open opens a new named session in a standing agent's pod and gives it as
// a session to go to. Its id is not known yet (its harness tells the
// dispatcher of itself as it starts): it is gone to by its window's name.
//
// A name that is taken is refused, here when the dispatcher knows a
// running session of that agent and name, and in the pod when one is
// remembered or has a window there. Never gone to instead: whoever asked
// for a new session named a directory and a model, and would be put in
// another conversation, somewhere else, with nothing to say so.
func open(c config, peers []message.Peer, o opening, ask io.Reader, out io.Writer, tty bool) (session, error) {
	switch {
	case !sessionName.MatchString(o.Name):
		return session{}, fmt.Errorf("%q is no name for a session: letters, digits, dots, dashes and underscores", o.Name)
	case o.Model != "" && !modelName.MatchString(o.Model):
		return session{}, fmt.Errorf("%q is no model: letters, digits and ._:[]-", o.Model)
	case c.Context == "":
		return session{}, errors.New("CARMAN_CONTEXT is not set: the kubectl context of the installation's cluster")
	}
	a, err := choose(peers, o.In, ask, out, tty)
	if err != nil {
		return session{}, err
	}
	by := a.Name + "/" + o.Name
	for _, s := range sessions(peers) {
		if s.Agent == a.Name && s.Name == o.Name && s.Running {
			return session{}, fmt.Errorf("%s is a session already (%s): carman go %s goes to it. Nothing was opened", by, s.State, by)
		}
	}
	argv := openCommand(c, a, o)
	cmd := exec.Command(argv[0], argv[1:]...)
	// What the pod says is for the owner, not for a caller that reads this
	// program's output.
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == nameTaken {
			return session{}, fmt.Errorf("%s is taken in %s: carman go %s goes to it. Nothing was opened", by, a.Pod, by)
		}
		return session{}, fmt.Errorf("opening %s in %s: %w", by, a.Pod, err)
	}
	return session{Agent: a.Name, Name: o.Name, Pod: a.Pod, Mode: "interactive", State: "not running", Standing: true, Latest: true}, nil
}

// goNew is `carman go --new`: open the session, then go to it as `carman
// go NAME` does.
func goNew(ctx context.Context, c config, o opening, keep bool) int {
	peers, err := fetchPeers(ctx, c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "carman go:", err)
		return exitError
	}
	s, err := open(c, peers, o, os.Stdin, os.Stderr, isTerminal(os.Stdin) && isTerminal(os.Stderr))
	if errors.Is(err, errCancelled) {
		return exitCancelled
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "carman go:", err)
		return exitError
	}
	return stay(ctx, c, s, keep)
}
