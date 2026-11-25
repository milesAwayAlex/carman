package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Most of what `cmem` does is decided by the service, not by the binary that
// was typed. The binary sends its arguments to POST /v1/cli; the service runs
// the same command logic there, with the caller's token, and sends back what
// to print and the exit code. So a change to what a command prints, to its
// flags, to the session-start text or to when a hook speaks up reaches every
// machine when the service is deployed, with no client to update.
//
// What stays in the binary is what only the caller's machine can do: read the
// repository it is in to find its scope, read standard input, and say that
// the service could not be reached; and what the service must not be able to
// change, which is the form of a hook's output (see relayHook). A binary talking to a service too old to
// have /v1/cli runs the command with its own copy of the logic.

// RemoteRequest is the body of POST /v1/cli.
type RemoteRequest struct {
	Args  []string          `json:"args"`
	Stdin string            `json:"stdin,omitempty"`
	Env   map[string]string `json:"env,omitempty"`   // the caller's CMEM_* settings, minus its credentials
	Scope []string          `json:"scope,omitempty"` // the scope of the caller's session, worked out on its machine
}

// RemoteResponse is what the caller prints and exits with.
type RemoteResponse struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Code   int    `json:"code"`
}

// localOnly are the commands that act on the caller's own machine.
var localOnly = map[string]bool{"serve": true, "check": true, "scope": true, "import": true, "routine": true, "backup": true, "restore": true}

// readsStdin are the commands whose input may arrive on standard input.
var readsStdin = map[string]bool{"hook": true, "record": true, "flashback": true}

// notForwarded are settings that mean something only on the caller's machine,
// or that must not travel as plain settings.
var notForwarded = map[string]bool{
	"CMEM_URL": true, "CMEM_TOKEN": true, "CMEM_TOKENS": true, "CMEM_LIBRARY": true,
	"CMEM_LOCAL": true, "CMEM_DIR": true, "CMEM_BRIEF_CMD": true,
}

// runRemote sends the command to the service. handled is false when the
// command has to run here after all: it is local by nature, the caller asked
// for that, or the service does not know /v1/cli.
func runRemote(ctx context.Context, args []string, getenv func(string) string, environ []string, stdin io.Reader, stdout, stderr io.Writer) (code int, handled bool) {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	base, token := getenv("CMEM_URL"), getenv("CMEM_TOKEN")
	if localOnly[name] || getenv("CMEM_LOCAL") == "1" || base == "" || token == "" {
		return 0, false
	}
	// A brief is asked of a model by a command on this machine; the service
	// cannot run it.
	if name == "hook" && getenv("CMEM_BRIEF_CMD") != "" {
		return 0, false
	}
	req := RemoteRequest{Args: args, Env: map[string]string{}}
	for _, pair := range environ {
		key, value, _ := strings.Cut(pair, "=")
		if strings.HasPrefix(key, "CMEM_") && !notForwarded[key] {
			req.Env[key] = value
		}
	}
	dir := getenv("CMEM_DIR")
	if readsStdin[name] {
		data, _ := io.ReadAll(stdin)
		req.Stdin = string(data)
		if name == "hook" {
			var event struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(data, &event) == nil && event.Cwd != "" {
				dir = event.Cwd
			}
		}
	}
	// A hook is told its session's directory; without one the session has no
	// scope. Any other command belongs to the directory it was typed in.
	if dir == "" && name != "hook" {
		dir, _ = os.Getwd()
	}
	local := &command{getenv: getenv}
	if dir != "" {
		req.Scope = local.sessionScope(dir)
	}

	timeout := 30 * time.Second
	if name == "hook" {
		timeout = 3 * time.Second
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/cli", bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(httpReq)
	if err != nil {
		if name == "hook" {
			// A hook must never break its session. The one thing worth
			// saying is said once, when the session starts.
			unreachableNotice(req.Stdin, err, getenv("CMEM_UNREACHABLE_HINT"), stdout)
			return 0, true
		}
		fmt.Fprintf(stderr, "cmem %s: cannot reach the memory service at %s: %v\n", name, base, err)
		return 1, true
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		// A service from before /v1/cli. Standard input was consumed above;
		// hand it on to the local run.
		return runLocal(ctx, args, getenv, strings.NewReader(req.Stdin), stdout, stderr), true
	}
	var out RemoteResponse
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		if name == "hook" {
			return 0, true
		}
		fmt.Fprintf(stderr, "cmem %s: the memory service answered %s\n", name, resp.Status)
		return 1, true
	}
	if name == "hook" {
		relayHook(req.Stdin, out.Stdout, stdout)
		return 0, true
	}
	io.WriteString(stdout, out.Stdout)
	io.WriteString(stderr, out.Stderr)
	return out.Code, true
}

// relayHook prints a hook's answer. The service decides what text a hook
// adds to the session and when; this binary decides what a hook is. A
// harness reads more from a hook's output than text: Claude Code lets one
// allow or deny a tool call, rewrite its input, or block a prompt, and reads
// a hook's exit code as a verdict. None of that is memory's business, so the
// only thing taken from the service's answer is the text, and the output is
// built here. Whatever answers at the service's address can then say things
// to a session, as memory always could, and can do nothing else to it.
func relayHook(event, answer string, stdout io.Writer) {
	var in claudeCodeInput
	var said struct {
		Output struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(event), &in) != nil || in.Event == "" ||
		json.Unmarshal([]byte(answer), &said) != nil || strings.TrimSpace(said.Output.Context) == "" {
		return
	}
	text := said.Output.Context
	if runes := []rune(text); len(runes) > relayedHookLimit {
		text = string(runes[:relayedHookLimit])
	}
	json.NewEncoder(stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]string{"hookEventName": in.Event, "additionalContext": text},
	})
}

// relayedHookLimit is the most text a hook passes on from the service: what
// Claude Code keeps of one hook's context.
const relayedHookLimit = 10000

// unreachableNotice prints, for a session that is starting, the line that
// tells the agent its memory is not there.
func unreachableNotice(event string, cause error, hint string, stdout io.Writer) {
	var in claudeCodeInput
	if json.Unmarshal([]byte(event), &in) != nil || in.Event != "SessionStart" || in.Source == "resume" {
		return
	}
	json.NewEncoder(stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]string{"hookEventName": in.Event, "additionalContext": unreachableText(cause, hint)},
	})
}

// unreachableText is what a starting session is told. hint is whatever the
// machine's own setup wants said about getting the service back
// (CMEM_UNREACHABLE_HINT): the command that diagnoses it, for instance.
func unreachableText(cause error, hint string) string {
	text := "Long-term memory (cmem) could not be reached at the start of this session, so nothing was " +
		"recalled and `cmem` commands may fail. Tell the user once, briefly. (" + summaryLine(cause.Error()) + ")"
	if hint = strings.TrimSpace(hint); hint != "" {
		text += " " + hint
	}
	return text + "\n"
}

// remotelyRunnable are the commands the service will run for a caller.
// Nothing that opens a library by path or starts a process.
var remotelyRunnable = map[string]bool{
	"help": true, "-h": true, "--help": true,
	"recall": true, "get": true, "note": true, "flag": true, "record": true, "index": true, "flashback": true,
	"hook": true, "add": true, "supersede": true, "retire": true, "relabel": true, "consolidate": true,
}

// RemoteHandler serves POST /v1/cli. Every request the command makes goes to
// api in this process, carrying the caller's own token, so the caller can do
// through this door exactly what it could do through the others.
func RemoteHandler(api http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RemoteRequest
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		var out, errOut bytes.Buffer
		resp := RemoteResponse{}
		switch {
		case len(req.Args) > 0 && localOnly[req.Args[0]]:
			fmt.Fprintf(&errOut, "cmem: %s runs on your own machine, not through the service.\n", req.Args[0])
			resp.Code = 2
		case len(req.Args) > 0 && !remotelyRunnable[req.Args[0]]:
			fmt.Fprintf(&errOut, "cmem: unknown command %q. Run cmem help.\n", req.Args[0])
			resp.Code = 2
		default:
			env := map[string]string{}
			for key, value := range req.Env {
				if strings.HasPrefix(key, "CMEM_") && !notForwarded[key] {
					env[key] = value
				}
			}
			env["CMEM_URL"], env["CMEM_TOKEN"] = "http://cmem.in-process", token
			if len(req.Scope) > 0 {
				env["CMEM_SCOPE"] = strings.Join(req.Scope, ",")
			}
			getenv := func(key string) string { return env[key] }
			resp.Code = run(r.Context(), req.Args, getenv, strings.NewReader(req.Stdin), &out, &errOut, api)
		}
		resp.Stdout, resp.Stderr = out.String(), errOut.String()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
}

// ServiceHandler is everything the service answers: the API, and the door
// through which a caller's whole command is run here.
func ServiceHandler(api http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", api)
	mux.Handle("POST /v1/cli", RemoteHandler(api))
	return mux
}

// inProcess answers one request by calling the handler directly.
func inProcess(handler http.Handler, req *http.Request) (status int, body []byte) {
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	handler.ServeHTTP(rec, req)
	return rec.status, rec.body.Bytes()
}

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(status int)      { r.status = status }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

var errNoService = errors.New("CMEM_URL and CMEM_TOKEN must be set")
