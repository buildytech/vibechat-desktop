package provision

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// streamFake is the fake host as a StreamRunner: it hands the script's `sub` calls to line in order, like the host's
// stderr would, and fails a script at failAt (a substep id) after reporting it.
type streamFake struct {
	*fakeHost
	failAt string
}

var subCallRe = regexp.MustCompile(`(?m)^sub ([a-z-]+)$`)

func (s *streamFake) RunStream(ctx context.Context, script string, stdin []byte, line func(string)) (Result, error) {
	for _, m := range subCallRe.FindAllStringSubmatch(script, -1) {
		line(SubMarker + " " + m[1])
		if m[1] == s.failAt {
			return Result{Code: 1, Stderr: []byte(SubMarker + " " + m[1] + "\nE: Unable to locate package incus\n")}, nil
		}
	}
	return s.fakeHost.Run(ctx, script, stdin)
}

type subEvents struct{ got []string }

func (e *subEvents) hook(step, sub, state string) { e.got = append(e.got, step+"/"+sub+":"+state) }

func TestSubstepsFollowTheScript(t *testing.T) {
	for id, subs := range Substeps {
		body := map[string]string{StepIncus: incusApply, StepContainer: containerApply}[id]
		var calls []string
		for _, m := range subCallRe.FindAllStringSubmatch(body, -1) {
			calls = append(calls, m[1])
		}
		if strings.Join(calls, ",") != strings.Join(subs, ",") {
			t.Errorf("%s: the script calls sub %v, Substeps %v", id, calls, subs)
		}
	}
	f := &streamFake{fakeHost: newFake(t)}
	a := &app{}
	ev := &subEvents{}
	h := a.hooks(f)
	h.Substep = ev.hook
	if err := Run(context.Background(), cfg(), h); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, id := range []string{StepIncus, StepContainer} {
		for _, s := range Substeps[id] {
			want = append(want, id+"/"+s+":running", id+"/"+s+":done")
		}
	}
	if strings.Join(ev.got, " ") != strings.Join(want, " ") {
		t.Fatalf("substeps\n got %v\nwant %v", ev.got, want)
	}
	logs := strings.Join(a.logs, "\n")
	if !strings.Contains(logs, "provision: incus: apt-update: running") || !strings.Contains(logs, "provision: container: wait-dns: done in ") {
		t.Errorf("log:\n%s", logs)
	}
	// The second run: both steps' checks pass, every substep is skipped and no apply runs again (idempotent).
	ev.got, f.calls = nil, nil
	h = a.hooks(f)
	h.Substep = ev.hook
	if err := Run(context.Background(), cfg(), h); err != nil {
		t.Fatal(err)
	}
	want = nil
	for _, id := range []string{StepIncus, StepContainer} {
		for _, s := range Substeps[id] {
			want = append(want, id+"/"+s+":skipped")
		}
	}
	if strings.Join(ev.got, " ") != strings.Join(want, " ") {
		t.Fatalf("rerun substeps %v", ev.got)
	}
	if got := f.applied(); len(got) != 0 {
		t.Fatalf("rerun applied %v", got)
	}
}

func TestSubstepFailureIsReported(t *testing.T) {
	f := &streamFake{fakeHost: newFake(t), failAt: "install-packages"}
	a := &app{}
	ev := &subEvents{}
	h := a.hooks(f)
	h.Substep = ev.hook
	err := Run(context.Background(), cfg(), h)
	var pe *Error
	if !errors.As(err, &pe) || pe.Step != StepIncus {
		t.Fatalf("%v", err)
	}
	if strings.Contains(pe.Message, SubMarker) || !strings.Contains(pe.Message, "Unable to locate package") {
		t.Errorf("message %q", pe.Message)
	}
	want := "incus/apt-update:running incus/apt-update:done incus/install-packages:running incus/install-packages:failed"
	if strings.Join(ev.got, " ") != want {
		t.Errorf("substeps %v", ev.got)
	}
	// Fixed: the rerun continues with the incus step.
	f.failAt, f.calls = "", nil
	ev.got = nil
	h = a.hooks(f)
	h.Substep = ev.hook
	if err := Run(context.Background(), cfg(), h); err != nil {
		t.Fatal(err)
	}
	if got := f.applied(); len(got) == 0 || got[0] != StepIncus {
		t.Errorf("rerun applied %v", got)
	}
}

// A plain Runner (no stream) still reports every substep done once the script succeeded.
func TestSubstepsWithAPlainRunner(t *testing.T) {
	f := newFake(t)
	a := &app{}
	ev := &subEvents{}
	h := a.hooks(f)
	h.Substep = ev.hook
	if err := Run(context.Background(), cfg(), h); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(ev.got, " "), ":done"); n != len(Substeps[StepIncus])+len(Substeps[StepContainer]) {
		t.Errorf("substeps %v", ev.got)
	}
}

func TestLineWriterSplitsLines(t *testing.T) {
	var got []string
	lw := &lineWriter{fn: func(s string) { got = append(got, s) }}
	_, _ = lw.Write([]byte("a\nb"))
	_, _ = lw.Write([]byte("c\n\nd"))
	lw.flush()
	if strings.Join(got, "|") != "a|bc||d" {
		t.Errorf("%q", got)
	}
}
