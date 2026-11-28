package meter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"carman/internal/dispatch/message"
)

// TestR10MeterEndSendsWhatItHolds: a meter told to end (a deploy) lets an
// answer under way finish, counts it, and sends every tally it holds,
// though none is idle or old yet and the dispatcher refuses the first
// tries (it is restarting too).
func TestR10MeterEndSendsWhatItHolds(t *testing.T) {
	got, more := make(chan seen, 4), make(chan struct{})
	srv := model(t, got, more)
	up, _ := url.Parse(srv.URL)
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.DisableCompression = true
	ledger := &Ledger{Idle: time.Hour, Longest: time.Hour}
	p := &Proxy{Upstream: up, Transport: tr, CallerHeader: "X-Egress-Caller", Ledger: ledger,
		Credential: func() (string, error) { return "Bearer the-real-one", nil },
		Identify: func(context.Context, string) (message.Caller, bool) {
			return message.Caller{Agent: "dreamer", Pod: "dreamer-1"}, true
		}}
	var (
		mu    sync.Mutex
		tries int
		sent  []Record
	)
	s := &Sender{Ledger: ledger, Every: time.Hour, Meter: "egress-1", Retry: 20 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Emit: func(_ context.Context, r Record) error {
			mu.Lock()
			defer mu.Unlock()
			if tries++; tries <= 2 {
				return errors.New("the dispatcher is restarting")
			}
			sent = append(sent, r)
			return nil
		}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	ctx, end := context.WithCancel(context.Background())
	ended := make(chan error)
	go func() {
		ended <- Serve(ctx, s, 5*time.Second, 5*time.Second, Listener{Server: &http.Server{Handler: p}, Listener: ln})
	}()
	front := "http://" + ln.Addr().String()
	ask := func(path string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, front+path, strings.NewReader(`{"model":"claude-haiku-5-5"}`))
		req.Header.Set("X-Egress-Caller", "10.0.0.1")
		resp, err := http.DefaultTransport.RoundTrip(req)
		must(t, err)
		return resp
	}
	whole := ask("/v1/messages?whole=1")
	io.Copy(io.Discard, whole.Body)
	whole.Body.Close()
	<-got
	during := ask("/v1/messages")
	defer during.Body.Close()
	<-got

	end()
	time.Sleep(50 * time.Millisecond)
	close(more)
	b, err := io.ReadAll(during.Body)
	if err != nil || !strings.HasSuffix(string(b), streamRest) {
		t.Errorf("the answer under way was cut at the meter's end: %v", err)
	}
	select {
	case err := <-ended:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the meter did not end")
	}
	mu.Lock()
	defer mu.Unlock()
	var requests, out int64
	for _, r := range sent {
		requests += r.Requests
		out += r.Tokens["output_tokens"]
	}
	if len(sent) != 2 || requests != 2 || out != 3+42 {
		t.Errorf("sent at the end: %d records, %d requests, %d tokens out: %+v", len(sent), requests, out, sent)
	}
}

// TestR10MeterEndGivesUpInTime: a dispatcher that never takes the records
// does not keep the meter past its time; what it held is in its log.
func TestR10MeterEndGivesUpInTime(t *testing.T) {
	ledger := &Ledger{Idle: time.Hour, Longest: time.Hour}
	ledger.add(message.Caller{Agent: "dreamer", Pod: "dreamer-1"}, "claude-haiku-5-5", func(r *Record) { r.Requests++ })
	var logs syncBuf
	s := &Sender{Ledger: ledger, Every: time.Hour, Meter: "egress-1", Retry: 10 * time.Millisecond,
		Log:  slog.New(slog.NewTextHandler(&logs, nil)),
		Emit: func(context.Context, Record) error { return errors.New("no dispatcher") }}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	ctx, end := context.WithCancel(context.Background())
	end()
	start := time.Now()
	must(t, Serve(ctx, s, time.Second, 200*time.Millisecond, Listener{Server: &http.Server{}, Listener: ln}))
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s", d)
	}
	if l := logs.String(); !strings.Contains(l, "model use: dreamer") || !strings.Contains(l, "the meter is ending") {
		t.Errorf("log: %s", l)
	}
}

// TestR10MeterPartsCountedOnce: a long session's use is written in parts,
// each its own event; a part sent again is the same event, so adding the
// log's records up counts each request once.
func TestR10MeterPartsCountedOnce(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	ledger := &Ledger{Idle: 2 * time.Minute, Longest: 5 * time.Minute, Now: func() time.Time { return now }}
	who := message.Caller{Agent: "desk", Pod: "desk-0"}
	ask := func() { ledger.add(who, "claude-haiku-5-5", func(r *Record) { r.Requests++ }) }
	ask()
	now = now.Add(time.Minute)
	ask()
	now = now.Add(4 * time.Minute)
	first := ledger.Due(false)
	ask()
	now = now.Add(time.Minute)
	ask()
	second := ledger.Due(true)
	if len(first) != 1 || len(second) != 1 || first[0].Requests != 2 || second[0].Requests != 2 {
		t.Fatalf("parts: %+v %+v", first, second)
	}
	events := map[string]int64{}
	for _, r := range []Record{first[0], second[0], first[0]} { // the first sent twice
		r.Meter = "egress-1"
		events[EventID(r)] = r.Requests
	}
	var sum int64
	for _, n := range events {
		sum += n
	}
	if len(events) != 2 || sum != 4 {
		t.Errorf("events: %v", events)
	}
}

// TestR10TrialsCapOutlivesRestart: a meter that starts again takes up what
// the log says trials used in the day, so their cap is not given anew.
func TestR10TrialsCapOutlivesRestart(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	l := &TrialLimits{Models: []string{"claude-haiku-5-5"}, Requests: 3, Now: func() time.Time { return now }}
	trial := message.Caller{Agent: "trial/t1/caller"}
	l.SetLogged(TrialsTally([]Record{
		{Agent: "trial/t1/caller", Model: "claude-haiku-5-5", Requests: 2, To: now.Add(-time.Hour)},
		{Agent: "trial/t1/caller", Model: "claude-haiku-5-5", Requests: 9, To: now.Add(-25 * time.Hour)},
		{Agent: "dreamer", Model: "claude-haiku-5-5", Requests: 9, To: now.Add(-time.Hour)},
	}, now, 24*time.Hour, func(Record) bool { return false }))
	must(t, l.Admit(trial, "claude-haiku-5-5", true))
	err := l.Admit(trial, "claude-haiku-5-5", true)
	if err == nil || !strings.Contains(err.Error(), "11:00 UTC") {
		t.Errorf("the fourth request of the day: %v", err)
	}
	now = now.Add(23*time.Hour + time.Second)
	must(t, l.Admit(trial, "claude-haiku-5-5", true))

	none := &TrialLimits{Models: []string{"claude-haiku-5-5"}}
	if err := none.Admit(trial, "claude-haiku-5-5", true); err == nil {
		t.Error("a cap of none passed a request")
	}
}
