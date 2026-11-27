package trigger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"carman/internal/dispatch"
)

var (
	ErrUnknown   = errors.New("no such watch or hook")
	ErrForbidden = errors.New("not allowed")
	ErrInvalid   = dispatch.ErrInvalid
)

// Observation is what a watch's job tells the dispatcher it saw: a digest
// of what its filter kept, and how much that was. Never the content.
type Observation struct {
	Digest string `json:"digest"`
	Kept   int    `json:"kept"`
}

// Changed is the data of watch.changed and watch.first.
type Changed struct {
	Watch    string `json:"watch"`
	Digest   string `json:"digest"`
	Kept     int    `json:"kept"`
	Previous string `json:"previous,omitempty"`
	// Since is when the previous digest was last seen: the change came
	// between then and this event's time.
	Since time.Time `json:"since,omitzero"`
}

// seen is what the dispatcher remembers of a watch.
type seen struct {
	Digest string    `json:"digest"`
	At     time.Time `json:"at"`
}

// Marks is where what was last seen is kept.
type Marks interface {
	Mark(ctx context.Context, name string, v any) (bool, error)
	SetMark(ctx context.Context, name string, v any) error
	Emit(ctx context.Context, e dispatch.Event) error
}

// Outside is the way in for what happens outside: watches and hooks.
type Outside struct {
	Store Marks
	// Watches are the names of the installation's watches.
	Watches []string
	// Hooks are its hooks, by name.
	Hooks map[string]Hook
	Now   func() time.Time
}

func (o *Outside) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

var digestRe = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// WatcherName is the name the job of a watch has as a caller.
func WatcherName(watch string) string { return "watch-" + watch }

// Observed takes a watch's observation: the first is remembered, a digest
// that differs from the last is recorded as watch.changed, and the same
// digest only moves the time it was last seen. It says whether it changed.
func (o *Outside) Observed(ctx context.Context, watch string, ob Observation) (bool, error) {
	known := false
	for _, w := range o.Watches {
		known = known || w == watch
	}
	if !known {
		return false, fmt.Errorf("%w: this installation has no watch %q", ErrUnknown, watch)
	}
	if !digestRe.MatchString(ob.Digest) || ob.Kept < 0 {
		return false, fmt.Errorf("%w: an observation is a digest in hex and how much was kept", ErrInvalid)
	}
	now := o.now().UTC()
	var last seen
	found, err := o.Store.Mark(ctx, "watch/"+watch, &last)
	if err != nil {
		return false, err
	}
	changed := found && last.Digest != ob.Digest
	if !found || changed {
		c := Changed{Watch: watch, Digest: ob.Digest, Kept: ob.Kept}
		typ := TypeWatchFirst
		if changed {
			typ, c.Previous, c.Since = TypeWatchChanged, last.Digest, last.At
		}
		b, _ := json.Marshal(c)
		// The digest is in the id: the same change told twice (a job
		// that was started again) is one event.
		id := fmt.Sprintf("%s/%s/%s/%s", typ, watch, last.Digest, ob.Digest)
		if err := o.Store.Emit(ctx, dispatch.Event{ID: id, Type: typ, Source: Source, Subject: watch, Time: now, Data: b}); err != nil {
			return false, err
		}
	}
	return changed, o.Store.SetMark(ctx, "watch/"+watch, seen{Digest: ob.Digest, At: now})
}

// Hook is a door for a source that can call in.
type Hook struct {
	// Secret signs the caller's requests: the hex HMAC-SHA256 of the body,
	// in Header, with or without "sha256=" before it (the code hosts' way).
	Secret string `json:"-"`
	Header string `json:"header"` // X-Hub-Signature-256 when empty
	// Pick names the fields of the call's JSON kept in the event: the name
	// in the event, and the dotted path in the body. A field that is not a
	// token is left out and named in "dropped".
	Pick map[string]string `json:"pick"`
}

// ParseHooks reads the installation's hooks (a JSON object by name), and
// the secret of each from secret(name).
func ParseHooks(raw string, secret func(name string) string) (map[string]Hook, error) {
	hooks := map[string]Hook{}
	if strings.TrimSpace(raw) == "" {
		return hooks, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&hooks); err != nil {
		return nil, fmt.Errorf("the hooks: %w", err)
	}
	for name, h := range hooks {
		if !nameRe.MatchString(name) {
			return nil, fmt.Errorf("hook %q: a hook's name is lowercase letters, digits and -", name)
		}
		if h.Secret = secret(name); len(h.Secret) < 16 {
			return nil, fmt.Errorf("hook %q: it has no secret of at least 16 characters; a door nobody is checked at is not opened", name)
		}
		if len(h.Pick) > 16 {
			return nil, fmt.Errorf("hook %q: picks at most 16 fields", name)
		}
		for k, path := range h.Pick {
			if envName(k) == "" || !fieldRe.MatchString(path) {
				return nil, fmt.Errorf("hook %q: pick %q: %q is not a field's name or path", name, k, path)
			}
		}
		hooks[name] = h
	}
	return hooks, nil
}

// Received takes a call at a hook: checked against the hook's secret, and
// recorded as hook.received with the fields picked, the body's digest and
// its length. The body is not kept. delivery, when the caller gives one
// (the code hosts do), makes the same call told twice one event.
func (o *Outside) Received(ctx context.Context, name, signature, delivery string, body []byte) (string, error) {
	h, ok := o.Hooks[name]
	if !ok {
		return "", fmt.Errorf("%w: this installation has no hook %q", ErrUnknown, name)
	}
	mac := hmac.New(sha256.New, []byte(h.Secret))
	mac.Write(body)
	given, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(signature), "sha256="))
	if err != nil || !hmac.Equal(given, mac.Sum(nil)) {
		return "", fmt.Errorf("%w: the call is not signed with this hook's secret", ErrForbidden)
	}
	sum := sha256.Sum256(body)
	data := map[string]any{"hook": name, "digest": hex.EncodeToString(sum[:8]), "bytes": len(body)}
	var parsed any
	var dropped []string
	if json.Unmarshal(body, &parsed) == nil {
		for k, path := range h.Pick {
			v, ok := field(parsed, path)
			if s, token := Token(v); ok && token {
				data[k] = s
			} else {
				dropped = append(dropped, k)
			}
		}
	}
	if len(dropped) > 0 {
		data["dropped"] = dropped
	}
	// With no word from the caller on which call this is, every call is
	// an event: two calls with the same body may be two things happening.
	key := fmt.Sprintf("%s-%d", hex.EncodeToString(sum[:4]), o.now().UnixNano())
	if d, ok := Token(delivery); ok {
		key = d
	}
	b, _ := json.Marshal(data)
	id := TypeHookReceived + "/" + name + "/" + key
	return id, o.Store.Emit(ctx, dispatch.Event{ID: id, Type: TypeHookReceived, Source: Source, Subject: name, Data: b})
}
