// Command carman is the owner's way to the sessions of an installation.
//
//	carman go                 pick a session and go to it: those waiting on the owner first
//	carman go NAME            go to a session by name, AGENT/NAME, or the start of its id
//	carman go --list          print the sessions, as the picker shows them, and exit
//	carman go --list --names  only what each is gone to by, one to a line
//	carman go --print [NAME]  pick or resolve, print what it is gone to by, and go nowhere
//	carman go --keep ...      after a dropped connection, attach again, until it is left
//	carman go --new NAME      open a new session of that name in a standing agent's pod
//	                          (--in AGENT, --dir DIR, --model MODEL) and go to it
//
//	carman status [TASK-ID]   the model's usage, what runs and ran, usage by agent
//	carman plan [--json]      which account to hold and why, and each one's standing
//	carman queue              the owner's priorities and what waits, runs and is done under each
//	carman queue add 'WHAT'   add to the list (--in PRIORITY, --hard, --model MODEL, --first)
//	carman queue drop|first|again ID
//	carman inbox [--read]     what is unread for the owner; --read marks it all read
//	carman read ID...         mark messages read
//
// It needs no checkout. Its configuration is the environment:
// CARMAN_CONTEXT (the kubectl context of the installation's cluster),
// CARMAN_NAMESPACE (carman unless set), CARMAN_DISPATCHER_URL and
// CARMAN_DISPATCHER_TOKEN (DISPATCHER_URL and DISPATCHER_TOKEN unless set:
// the dispatcher, with the owner's machine's token), CARMAN_KUBECTL (kubectl
// unless set), and CARMAN_CHECKOUT (optional: a checkout whose
// scripts/deploy.sh resumes a job's kept conversation, ADR 0040),
// CARMAN_OWNER_TOKEN_FILE (the file with the owner's token for the
// dispatcher: status, plan, the inbox and marking read are the owner's)
// and CARMAN_DISPATCHER (the dispatcher's client, which prints status, plan
// and queue; dispatcher on PATH unless set or built in).
package main

import (
	"fmt"
	"os"
)

const usage = `carman go [--keep] [NAME | AGENT/NAME | ID-PREFIX]   go to a session; with no name, pick one
carman go --list [--names]                            print the sessions and exit
carman go --print [NAME]                              pick or resolve, and print what it is gone to by
carman go --new NAME [--in AGENT] [--dir DIR] [--model MODEL] [--keep]
                                                      open a new session in a standing agent's pod and go to it
carman status [TASK-ID]                               the model's usage, what runs and ran lately, usage by agent
carman plan [--json]                                  which account to hold and why; each account's standing
carman queue                                          the priorities, and what waits, runs and is done under each
carman queue add 'WHAT' [--in PRIORITY] [--hard] [--model MODEL] [--first]
                                                      add to the list, under the default priority unless --in names one
carman queue drop|first|again ID                      an item is not to be done; goes first; waits again after failing
carman inbox [--read]                                 what is unread for you; --read marks all of it read
carman read MESSAGE-ID...                             mark messages read

A new session: --in says which standing agent's pod (not needed when there is
one; asked for on a terminal when there are several). --dir is a directory in
the pod, the pod's ~/work when not given, and one that is not absolute is
under ~/work. --model is the session's model, the pod's own when not given.
The pod remembers the session and opens it again when it restarts. A name
that is taken there is refused, and nothing is opened: carman go NAME goes to
that one. The list shows the new session once its harness has started, a few
seconds after it is opened.

Ending a session for good: /exit in it, then exit in the shell it leaves.
That closes its window, and the pod no longer opens it again; its
conversation is kept. Closing your own window only leaves it, running.

Unread: going to a session marks what it sent you as read, with the owner's
token; carman inbox shows everything unread, also from sessions that are gone.
status, plan and queue are the dispatcher's client's own words (dispatcher
status, plan, priority list|add|drop|first|again); status, plan, the inbox and
marking read need the owner's token, queue takes the machine's when there is
no owner's. Priorities themselves are set from a checkout (dispatch priority set).

Configuration, from the environment: CARMAN_CONTEXT, CARMAN_NAMESPACE (carman),
CARMAN_DISPATCHER_URL and CARMAN_DISPATCHER_TOKEN (or DISPATCHER_URL and
DISPATCHER_TOKEN), CARMAN_KUBECTL (kubectl), CARMAN_CHECKOUT (optional),
CARMAN_OWNER_TOKEN_FILE (the file with the owner's token; a checkout's
.local/dispatcher-owner.token), CARMAN_DISPATCHER (the dispatcher's client).`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Println(usage)
		return
	}
	word, rest := os.Args[1], os.Args[2:]
	switch {
	case word == "go":
		os.Exit(goCmd(rest))
	case word == "inbox" || word == "read":
		os.Exit(inboxCmd(word, rest, os.Stdout))
	case views[word].Ask != nil:
		os.Exit(viewCmd(word, rest))
	}
	fmt.Fprintf(os.Stderr, "carman: unknown command %q\n%s\n", word, usage)
	os.Exit(2)
}
