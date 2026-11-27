package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"carman/internal/dispatch"
	"carman/internal/secret"
)

// Event types and the source of the watcher's events.
const (
	// TypeJobEnded is one Job's end, whoever started it. Its subject is
	// what ran (the CronJob, or the task's class); its data a JobEnded.
	TypeJobEnded = "job.ended"
	SourceJobs   = "job-watcher"
)

// JobEnded is the data of a job.ended event.
type JobEnded struct {
	Job      string     `json:"job"`
	Template string     `json:"template,omitempty"`  // what ran: the CronJob or class
	By       string     `json:"started_by"`          // cronjob, dispatcher, or by-hand
	Outcome  string     `json:"outcome"`             // succeeded or failed
	Reason   string     `json:"reason,omitempty"`    // the Job's, when it failed
	Started  *time.Time `json:"started,omitempty"`   //
	Ended    time.Time  `json:"ended"`               //
	ExitCode *int       `json:"exit_code,omitempty"` // of its last pod, when one is left
	Task     string     `json:"task,omitempty"`      // when the dispatcher started it
	Attempt  string     `json:"attempt,omitempty"`   //
	// The Job says it tells the owner of its own failure
	// (AnnotationNotifiesOwner).
	NotifiesOwner bool `json:"notifies_owner,omitempty"`
	// Why it failed, as far as the cluster knows.
	Why *Why `json:"why,omitempty"`
}

// Why is what the cluster says of a failed Job and its last pod. Its log's
// lines are for the dispatcher's log only, never for a notice (ADR 0048):
// a line shaped like a credential is withheld, and the whole is bounded.
type Why struct {
	Message            string   `json:"message,omitempty"`             // the Job's failed condition's
	Pod                string   `json:"pod,omitempty"`                 // its last pod
	PodReason          string   `json:"pod_reason,omitempty"`          // the pod's: Evicted, DeadlineExceeded
	PodMessage         string   `json:"pod_message,omitempty"`         //
	Termination        string   `json:"termination,omitempty"`         // the container's: Error, OOMKilled
	TerminationMessage string   `json:"termination_message,omitempty"` // what it wrote to its termination log
	Killed             string   `json:"killed,omitempty"`              // memory or deadline, when it was
	LogTail            []string `json:"log_tail,omitempty"`            // its log's last lines
}

// Bounds of what Why keeps of a pod's log and messages.
const (
	logTailLines = 20
	logTailBytes = 8 << 10
	lineMax      = 300
)

// AnnotationNotifiesOwner, "true" on a Job (put on its CronJob's job
// template), says that the program it runs tells the owner itself when it
// fails, so that a failure it reported is not told twice.
const AnnotationNotifiesOwner = "dispatcher/notifies-owner"

// object is what the watcher reads of a Job or a Pod.
type object struct {
	Metadata struct {
		Name              string            `json:"name"`
		UID               string            `json:"uid"`
		ResourceVersion   string            `json:"resourceVersion"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
		OwnerReferences   []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Status struct {
		StartTime      *time.Time `json:"startTime"`
		CompletionTime *time.Time `json:"completionTime"`
		Conditions     []struct {
			Type               string    `json:"type"`
			Status             string    `json:"status"`
			Reason             string    `json:"reason"`
			Message            string    `json:"message"`
			LastTransitionTime time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
		// A pod's.
		Reason            string `json:"reason"`
		Message           string `json:"message"`
		ContainerStatuses []struct {
			Name      string         `json:"name"`
			State     containerState `json:"state"`
			LastState containerState `json:"lastState"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type containerState struct {
	Terminated *struct {
		ExitCode int    `json:"exitCode"`
		Reason   string `json:"reason"`
		Message  string `json:"message"`
	} `json:"terminated"`
}

type list struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Items []object `json:"items"`
}

// Watcher records in the log, once, the end of every Job in the namespace,
// and for a Job the dispatcher started, the task.finished of its attempt.
// Each event's ID is derived from the Job's UID, so that the log refuses a
// second one however often the watcher restarts. On start, and every
// Resync, it lists what the cluster still has: a Job that ended while
// nothing watched is found then.
type Watcher struct {
	Client *Client
	Emit   func(context.Context, dispatch.Event) error
	Log    *slog.Logger
	// Resync is how often the whole list is read again; ten minutes when zero.
	Resync time.Duration
	// Retry is how long to wait after a failure; five seconds when zero.
	Retry time.Duration

	mu   sync.Mutex
	done map[string]bool // UIDs whose end this process has recorded
}

// Run watches until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	resync, retry := w.Resync, w.Retry
	if resync <= 0 {
		resync = 10 * time.Minute
	}
	if retry <= 0 {
		retry = 5 * time.Second
	}
	for ctx.Err() == nil {
		rv, err := w.Sync(ctx)
		until := time.Now().Add(resync)
		for err == nil && ctx.Err() == nil && time.Now().Before(until) {
			rv, err = w.watch(ctx, rv, time.Until(until))
		}
		if err != nil && ctx.Err() == nil {
			w.Log.Warn("watching jobs", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(retry):
			}
		}
	}
}

// Sync reads every Job and records those that ended. It returns the list's
// resource version, from which a watch carries on.
func (w *Watcher) Sync(ctx context.Context) (string, error) {
	var l list
	if err := w.Client.do(ctx, http.MethodGet, w.Client.list("jobs"), nil, &l); err != nil {
		return "", err
	}
	var errs []error
	for _, job := range l.Items {
		errs = append(errs, w.handle(ctx, job))
	}
	return l.Metadata.ResourceVersion, errors.Join(errs...)
}

var errGone = errors.New("the watch's resource version is too old")

// watch follows changes from rv for at most d, and returns the last
// version it saw.
func (w *Watcher) watch(ctx context.Context, rv string, d time.Duration) (string, error) {
	q := url.Values{"watch": {"1"}, "resourceVersion": {rv}, "allowWatchBookmarks": {"true"},
		"timeoutSeconds": {strconv.Itoa(max(int(d.Seconds()), 1))}}
	if w.Client.Selector != "" {
		q.Set("labelSelector", w.Client.Selector)
	}
	resp, err := w.Client.request(ctx, http.MethodGet, w.Client.path("jobs")+"?"+q.Encode(), nil)
	if err != nil {
		return rv, err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var ev struct {
			Type   string          `json:"type"`
			Object json.RawMessage `json:"object"`
		}
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return rv, ctx.Err()
			}
			return rv, nil // the server ended the watch; carry on from rv
		}
		if ev.Type == "ERROR" {
			var st struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(ev.Object, &st)
			if st.Code == http.StatusGone {
				return rv, errGone
			}
			return rv, fmt.Errorf("watch: %d %s", st.Code, st.Message)
		}
		var job object
		if err := json.Unmarshal(ev.Object, &job); err != nil {
			return rv, err
		}
		if job.Metadata.ResourceVersion != "" {
			rv = job.Metadata.ResourceVersion
		}
		if ev.Type == "BOOKMARK" {
			continue
		}
		if err := w.handle(ctx, job); err != nil {
			return rv, err
		}
		if ev.Type == "DELETED" {
			w.mu.Lock()
			delete(w.done, job.Metadata.UID) // it will not be listed again
			w.mu.Unlock()
		}
	}
}

// handle records a Job's end, if it has ended and this process has not
// recorded it yet. The log refuses what an earlier process recorded.
func (w *Watcher) handle(ctx context.Context, job object) error {
	ended, ok := endOf(job)
	if !ok {
		return nil
	}
	w.mu.Lock()
	seen := w.done[job.Metadata.UID]
	w.mu.Unlock()
	if seen {
		return nil
	}
	ended.ExitCode, ended.Why = w.lastPod(ctx, job.Metadata.Name, ended.Outcome != "succeeded")
	if ended.Why != nil {
		ended.Why.Message = clip(failedMessage(job))
		ended.Why.Killed = killed(ended)
	}
	data, err := json.Marshal(ended)
	if err != nil {
		return err
	}
	err = w.Emit(ctx, dispatch.Event{
		ID: TypeJobEnded + "/" + job.Metadata.UID, Type: TypeJobEnded, Source: SourceJobs,
		Subject: ended.Template, Time: ended.Ended, Data: data,
	})
	if err != nil {
		return err
	}
	if ended.Task != "" && ended.Attempt != "" {
		f := dispatch.Finished{Attempt: ended.Attempt, Outcome: dispatch.OutcomeDone, Message: "job " + ended.Job + " succeeded"}
		if ended.Outcome != "succeeded" {
			f.Outcome = dispatch.OutcomeFailed
			f.Message = "job " + ended.Job + " failed: " + ended.Reason
			if ended.ExitCode != nil {
				f.Message += fmt.Sprintf(", exit code %d", *ended.ExitCode)
			}
			if ended.Why != nil && ended.Why.Killed != "" {
				f.Message += ", killed for its " + ended.Why.Killed
			}
		}
		data, _ := json.Marshal(f)
		err = w.Emit(ctx, dispatch.Event{
			ID: dispatch.TypeFinished + "/" + job.Metadata.UID, Type: dispatch.TypeFinished, Source: SourceJobs,
			Subject: ended.Task, Time: ended.Ended, Data: data,
		})
		if err != nil {
			return err
		}
	}
	w.mu.Lock()
	if w.done == nil {
		w.done = map[string]bool{}
	}
	w.done[job.Metadata.UID] = true
	w.mu.Unlock()
	return nil
}

// endOf says how a Job ended, if it has.
func endOf(job object) (JobEnded, bool) {
	e := JobEnded{Job: job.Metadata.Name, Started: job.Status.StartTime}
	for _, c := range job.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			e.Outcome = "succeeded"
			e.Ended = c.LastTransitionTime
			if job.Status.CompletionTime != nil {
				e.Ended = *job.Status.CompletionTime
			}
		case "Failed":
			e.Outcome = "failed"
			e.Reason = c.Reason
			e.Ended = c.LastTransitionTime
		}
	}
	if e.Outcome == "" {
		return e, false
	}
	m := job.Metadata
	e.Task, e.Attempt = m.Annotations[AnnotationTask], m.Annotations[AnnotationAttempt]
	e.NotifiesOwner = m.Annotations[AnnotationNotifiesOwner] == "true"
	switch {
	case e.Attempt != "":
		e.By, e.Template = "dispatcher", m.Annotations[AnnotationClass]
	case m.Annotations["cronjob.kubernetes.io/instantiate"] == "manual":
		e.By = "by-hand"
	default:
		e.By = "cronjob"
	}
	for _, o := range m.OwnerReferences {
		if o.Kind == "CronJob" && e.Template == "" {
			e.Template = o.Name
		}
	}
	if e.Template == "" {
		e.Template = m.Labels["app"]
	}
	if e.Template == "" {
		e.Template, e.By = m.Name, "by-hand"
	}
	return e, true
}

// lastPod reads the Job's last pod, if the cluster still has one: the exit
// code of its first container and, for a Job that failed, why it ended.
func (w *Watcher) lastPod(ctx context.Context, job string, failed bool) (*int, *Why) {
	var why *Why
	if failed {
		why = &Why{}
	}
	var l list
	q := url.Values{"labelSelector": {"job-name=" + job}}
	if err := w.Client.do(ctx, http.MethodGet, w.Client.path("pods")+"?"+q.Encode(), nil, &l); err != nil {
		w.Log.Warn("reading a job's pods", "job", job, "error", err)
		return nil, why
	}
	var last *object
	for i := range l.Items {
		if last == nil || l.Items[i].Metadata.CreationTimestamp.After(last.Metadata.CreationTimestamp) {
			last = &l.Items[i]
		}
	}
	if last == nil {
		return nil, why
	}
	if why != nil {
		why.Pod, why.PodReason, why.PodMessage = last.Metadata.Name, last.Status.Reason, clip(last.Status.Message)
	}
	if len(last.Status.ContainerStatuses) == 0 {
		return nil, why
	}
	c := last.Status.ContainerStatuses[0]
	end := c.State.Terminated
	if end == nil {
		end = c.LastState.Terminated // restarted, and waiting again
	}
	if why != nil {
		if end != nil {
			why.Termination, why.TerminationMessage = end.Reason, withheld(clip(end.Message))
		}
		why.LogTail = w.logTail(ctx, last.Metadata.Name, c.Name)
	}
	if c.State.Terminated == nil {
		return nil, why
	}
	code := c.State.Terminated.ExitCode
	return &code, why
}

// logTail is the last lines of a container's log, bounded, each line that
// looks like a credential withheld. None when the log cannot be read.
func (w *Watcher) logTail(ctx context.Context, pod, container string) []string {
	q := url.Values{"tailLines": {strconv.Itoa(logTailLines)}, "limitBytes": {strconv.Itoa(logTailBytes)}}
	if container != "" {
		q.Set("container", container)
	}
	resp, err := w.Client.request(ctx, http.MethodGet, w.Client.path("pods")+"/"+url.PathEscape(pod)+"/log?"+q.Encode(), nil)
	if err != nil {
		w.Log.Warn("reading a job's log", "pod", pod, "error", err)
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, logTailBytes))
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			lines = append(lines, withheld(clip(line)))
		}
	}
	return lines[max(len(lines)-logTailLines, 0):]
}

// failedMessage is the message of the Job's Failed condition.
func failedMessage(job object) string {
	for _, c := range job.Status.Conditions {
		if c.Type == "Failed" && c.Status == "True" {
			return c.Message
		}
	}
	return ""
}

// killed says what the Job was killed for, if the cluster says: its memory
// (the kernel's OOM killer), or its deadline (the Job's or the pod's).
func killed(e JobEnded) string {
	switch {
	case e.Why.Termination == "OOMKilled":
		return "memory"
	case e.Reason == "DeadlineExceeded" || e.Why.PodReason == "DeadlineExceeded":
		return "deadline"
	}
	return ""
}

func clip(s string) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "?")
	if len(s) <= lineMax {
		return s
	}
	cut := lineMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// withheld is the text, or what stands for it when it looks like a credential.
func withheld(s string) string {
	if kind, found := secret.Find(s); found {
		return "[withheld: it looks like it contains " + kind + "]"
	}
	return s
}
