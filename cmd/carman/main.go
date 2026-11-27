// Command carman is the owner's way to the sessions of an installation.
//
//	carman go                 pick a session and go to it: those waiting on the owner first
//	carman go NAME            go to a session by name, AGENT/NAME, or the start of its id
//	carman go --list          print the sessions, as the picker shows them, and exit
//	carman go --list --names  only what each is gone to by, one to a line
//	carman go --print [NAME]  pick or resolve, print what it is gone to by, and go nowhere
//	carman go --keep ...      after a dropped connection, attach again, until it is left
//
// It needs no checkout. Its configuration is the environment:
// CARMAN_CONTEXT (the kubectl context of the installation's cluster),
// CARMAN_NAMESPACE (carman unless set), CARMAN_DISPATCHER_URL and
// CARMAN_DISPATCHER_TOKEN (DISPATCHER_URL and DISPATCHER_TOKEN unless set:
// the dispatcher, with the owner's machine's token), CARMAN_KUBECTL (kubectl
// unless set), and CARMAN_CHECKOUT (optional: a checkout whose
// scripts/deploy.sh resumes a job's kept conversation, ADR 0040).
package main

import (
	"fmt"
	"os"
)

const usage = `carman go [--keep] [NAME | AGENT/NAME | ID-PREFIX]   go to a session; with no name, pick one
carman go --list [--names] [--all]                    print the sessions and exit
carman go --print [NAME]                              pick or resolve, and print what it is gone to by

Configuration, from the environment: CARMAN_CONTEXT, CARMAN_NAMESPACE (carman),
CARMAN_DISPATCHER_URL and CARMAN_DISPATCHER_TOKEN (or DISPATCHER_URL and
DISPATCHER_TOKEN), CARMAN_KUBECTL (kubectl), CARMAN_CHECKOUT (optional).`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Println(usage)
		return
	}
	if os.Args[1] != "go" {
		fmt.Fprintf(os.Stderr, "carman: unknown command %q\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
	os.Exit(goCmd(os.Args[2:]))
}
