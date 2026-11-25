package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"carman/internal/dispatch"
)

// What a Job the dispatcher made carries. The annotations hold the exact
// task and attempt; the labels, which allow only some characters, a form of
// them to select by.
const (
	AnnotationTask    = "dispatcher/task"
	AnnotationAttempt = "dispatcher/attempt"
	AnnotationClass   = "dispatcher/class"
	LabelTask         = "dispatcher/task"
	LabelAttempt      = "dispatcher/attempt"
)

// Payload is what a task's payload may say about its Job: environment
// variables for its first container, whose names start with TASK_, and
// arguments added after the template's own. It cannot replace anything the
// template sets.
type Payload struct {
	Env  map[string]string `json:"env,omitempty"`
	Args []string          `json:"args,omitempty"`
}

var (
	envName  = regexp.MustCompile(`^TASK_[A-Z0-9_]{1,60}$`)
	reserved = map[string]bool{"TASK_ID": true, "TASK_ATTEMPT": true, "TASK_CLASS": true, "TASK_WAKE": true}
)

// ParsePayload reads a task's payload and refuses what a Job may not be given.
func ParsePayload(raw json.RawMessage) (Payload, error) {
	var p Payload
	if len(raw) == 0 || string(raw) == "null" {
		return p, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("%w: payload: %v", dispatch.ErrInvalid, err)
	}
	if len(p.Env) > 32 || len(p.Args) > 16 {
		return p, fmt.Errorf("%w: payload: at most 32 variables and 16 arguments", dispatch.ErrInvalid)
	}
	for k, v := range p.Env {
		if !envName.MatchString(k) || reserved[k] {
			return p, fmt.Errorf("%w: payload: a variable's name is TASK_ and capitals, and not one the dispatcher sets; got %q", dispatch.ErrInvalid, k)
		}
		if len(v) > 4096 {
			return p, fmt.Errorf("%w: payload: %s is longer than 4096 bytes", dispatch.ErrInvalid, k)
		}
	}
	for _, a := range p.Args {
		if len(a) > 1024 {
			return p, fmt.Errorf("%w: payload: an argument is longer than 1024 bytes", dispatch.ErrInvalid)
		}
	}
	return p, nil
}

var notName = regexp.MustCompile(`[^a-z0-9-]+`)

// JobName is the name of an attempt's Job: the task's ID and the attempt's
// number when the ID is already a valid name, and otherwise as much of it as
// is valid with a hash of the whole, so that two attempts never share a name.
func JobName(taskID string, number int) string {
	suffix := "-" + strconv.Itoa(number)
	clean := strings.Trim(notName.ReplaceAllString(strings.ToLower(taskID), "-"), "-")
	if clean != "" && clean == taskID && len(clean)+len(suffix) <= 63 && !strings.Contains(clean, "--") {
		return clean + suffix
	}
	sum := sha256.Sum256([]byte(taskID))
	hash := "x" + hex.EncodeToString(sum[:])[:8]
	if room := 63 - len(suffix) - len(hash) - 1; len(clean) > room {
		clean = strings.TrimRight(clean[:room], "-")
	}
	if clean == "" {
		return hash + suffix
	}
	return clean + "-" + hash + suffix
}

// labelValue is a form of s that a label may hold.
func labelValue(s string) string {
	v := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(s, "-")
	if len(v) > 63 {
		sum := sha256.Sum256([]byte(s))
		v = v[:54] + "-" + hex.EncodeToString(sum[:])[:8]
	}
	return strings.Trim(v, "-._")
}

// Starter starts an attempt as a Job made from the job template of the
// CronJob its task's class names. It is idempotent by attempt: the Job is
// named for the attempt, and finding it there is success.
type Starter struct {
	Client *Client
}

var _ dispatch.Starter = (*Starter)(nil)

func (s *Starter) Start(ctx context.Context, a dispatch.Attempt) error {
	name := JobName(a.Task.ID, a.Number)
	var existing object
	err := s.Client.do(ctx, http.MethodGet, s.Client.path("jobs")+"/"+name, nil, &existing)
	if err == nil {
		return s.same(existing, a, name)
	}
	if !IsStatus(err, http.StatusNotFound) {
		return err
	}
	job, err := s.job(ctx, a, name)
	if err != nil {
		return err
	}
	err = s.Client.do(ctx, http.MethodPost, s.Client.path("jobs"), job, nil)
	if IsStatus(err, http.StatusConflict) {
		if err := s.Client.do(ctx, http.MethodGet, s.Client.path("jobs")+"/"+name, nil, &existing); err != nil {
			return err
		}
		return s.same(existing, a, name)
	}
	return err
}

// same says whether the Job found under the attempt's name is the attempt's.
func (s *Starter) same(job object, a dispatch.Attempt, name string) error {
	if job.Metadata.Annotations[AnnotationAttempt] != a.Name {
		return fmt.Errorf("a job named %s exists and is not attempt %s's", name, a.Name)
	}
	return nil
}

// Check says whether a task could be started: its class names a CronJob
// here and its payload is one a Job may be given.
func (s *Starter) Check(ctx context.Context, t dispatch.TaskSpec) error {
	if _, err := ParsePayload(t.Payload); err != nil {
		return err
	}
	_, err := s.template(ctx, t.Class)
	return err
}

func (s *Starter) template(ctx context.Context, class string) (map[string]any, error) {
	var cj map[string]any
	err := s.Client.do(ctx, http.MethodGet, s.Client.path("cronjobs")+"/"+class, nil, &cj)
	if IsStatus(err, http.StatusNotFound) {
		return nil, fmt.Errorf("%w: no template: there is no CronJob %q in namespace %s", dispatch.ErrInvalid, class, s.Client.Namespace)
	}
	if err != nil {
		return nil, err
	}
	tmpl, _ := dig(cj, "spec", "jobTemplate").(map[string]any)
	if tmpl == nil {
		return nil, fmt.Errorf("the CronJob %s has no job template", class)
	}
	return tmpl, nil
}

// job is the Job for an attempt: the template's metadata and spec as they
// are, with the task's labels, annotations, variables and arguments added.
func (s *Starter) job(ctx context.Context, a dispatch.Attempt, name string) (map[string]any, error) {
	p, err := ParsePayload(a.Task.Payload)
	if err != nil {
		return nil, err
	}
	tmpl, err := s.template(ctx, a.Task.Class)
	if err != nil {
		return nil, err
	}
	meta, _ := tmpl["metadata"].(map[string]any)
	spec, _ := tmpl["spec"].(map[string]any)
	if spec == nil {
		return nil, fmt.Errorf("the CronJob %s's job template has no spec", a.Task.Class)
	}
	labels := map[string]any{LabelTask: labelValue(a.Task.ID), LabelAttempt: strconv.Itoa(a.Number)}
	annotations := map[string]any{AnnotationTask: a.Task.ID, AnnotationAttempt: a.Name, AnnotationClass: a.Task.Class}
	jobMeta := map[string]any{
		"name":        name,
		"namespace":   s.Client.Namespace,
		"labels":      merge(meta["labels"], labels),
		"annotations": merge(meta["annotations"], annotations),
	}
	podMeta, _ := dig(spec, "template", "metadata").(map[string]any)
	if podMeta == nil {
		podMeta = map[string]any{}
		tpl, _ := spec["template"].(map[string]any)
		if tpl == nil {
			return nil, fmt.Errorf("the CronJob %s's job template has no pod template", a.Task.Class)
		}
		tpl["metadata"] = podMeta
	}
	podMeta["labels"] = merge(podMeta["labels"], labels)
	podMeta["annotations"] = merge(podMeta["annotations"], annotations)

	containers, _ := dig(spec, "template", "spec", "containers").([]any)
	if len(containers) == 0 {
		return nil, fmt.Errorf("the CronJob %s's job template has no container", a.Task.Class)
	}
	c, _ := containers[0].(map[string]any)
	env, _ := c["env"].([]any)
	set := map[string]bool{}
	for _, e := range env {
		if m, ok := e.(map[string]any); ok {
			set[fmt.Sprint(m["name"])] = true
		}
	}
	vars := map[string]string{"TASK_ID": a.Task.ID, "TASK_ATTEMPT": a.Name, "TASK_CLASS": a.Task.Class}
	if a.Wake != nil {
		b, _ := json.Marshal(a.Wake)
		vars["TASK_WAKE"] = string(b)
	}
	for k, v := range p.Env {
		if set[k] {
			return nil, fmt.Errorf("%w: payload: the template sets %s itself", dispatch.ErrInvalid, k)
		}
		vars[k] = v
	}
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		env = append(env, map[string]any{"name": k, "value": vars[k]})
	}
	c["env"] = env
	if len(p.Args) > 0 {
		args, _ := c["args"].([]any)
		for _, arg := range p.Args {
			args = append(args, arg)
		}
		c["args"] = args
	}
	return map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": jobMeta, "spec": spec}, nil
}

func dig(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[k]
	}
	return v
}

func merge(base any, add map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := base.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	for k, v := range add {
		out[k] = v
	}
	return out
}
