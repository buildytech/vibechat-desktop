package provision

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Substeps of the long steps, in order (DESKTOP_PROVISION_SUBSTEPS of the Studio contract). The apply script of such
// a step calls `sub <id>` before each part; the line goes to stderr as SubMarker + " " + id and the wizard reports it
// (Hooks.Substep) while the script runs (StreamRunner) or, with a plain Runner, after it.
var Substeps = map[string][]string{
	StepIncus:     {"apt-update", "install-packages", "admin-init"},
	StepContainer: {"storage-pool", "bridge", "profile", "launch-image", "wait-dns"},
}

// SubMarker starts a substep line on the apply script's stderr (never part of an error message).
const SubMarker = "::vibechat-sub::"

const subPrelude = `sub() { printf '%s %s\n' '` + SubMarker + `' "$1" >&2; }
`

// StreamRunner is a Runner that also hands every stderr line to line while the script runs (SSHRunner does).
type StreamRunner interface {
	Runner
	RunStream(ctx context.Context, script string, stdin []byte, line func(string)) (Result, error)
}

func (w *wizard) sub(step, sub, state string) {
	if w.h.Substep != nil {
		w.h.Substep(step, sub, state)
	}
}

// skipSubs reports every substep of a step whose check passed as skipped.
func (w *wizard) skipSubs(step string) {
	for _, s := range Substeps[step] {
		w.sub(step, s, Skipped)
	}
}

// applySubsOf is applyOf for a step with substeps: the script's `sub` lines drive the substeps' progress and the log.
func (w *wizard) applySubsOf(id, body string, extra map[string]string) func(context.Context) error {
	return func(ctx context.Context) error {
		t := &subTracker{w: w, step: id}
		sc := script(id, "apply", w.vars(extra), subPrelude+strings.TrimLeft(body, "\n"))
		var res Result
		var err error
		if sr, ok := w.r.(StreamRunner); ok {
			res, err = sr.RunStream(ctx, sc, nil, t.line)
		} else {
			res, err = w.r.Run(ctx, sc, nil)
			for _, l := range strings.Split(string(res.Stderr), "\n") {
				t.line(l)
			}
		}
		if err == nil && res.Code != 0 {
			err = fmt.Errorf("exit %d: %s", res.Code, tail(res.Stderr))
		}
		if err != nil {
			t.fail()
			return err
		}
		t.finish()
		return nil
	}
}

type subTracker struct {
	w     *wizard
	step  string
	cur   string
	since time.Time
	seen  map[string]bool
}

func known(step, sub string) bool {
	for _, s := range Substeps[step] {
		if s == sub {
			return true
		}
	}
	return false
}

func (t *subTracker) line(l string) {
	name, ok := strings.CutPrefix(strings.TrimSpace(l), SubMarker+" ")
	if !ok || !known(t.step, name) || t.seen[name] {
		return
	}
	t.done()
	if t.seen == nil {
		t.seen = map[string]bool{}
	}
	t.seen[name] = true
	t.cur, t.since = name, time.Now()
	t.w.sub(t.step, name, Running)
	t.w.h.Log(fmt.Sprintf("provision: %s: %s: running", t.step, name))
}

func (t *subTracker) done() {
	if t.cur == "" {
		return
	}
	t.w.sub(t.step, t.cur, Done)
	t.w.h.Log(fmt.Sprintf("provision: %s: %s: done in %s", t.step, t.cur, time.Since(t.since).Round(100*time.Millisecond)))
	t.cur = ""
}

// finish: the script ran to its end, so every substep is done (a plain Runner reports the ones it did not see now).
func (t *subTracker) finish() {
	t.done()
	for _, s := range Substeps[t.step] {
		if !t.seen[s] {
			t.w.sub(t.step, s, Done)
		}
	}
}

func (t *subTracker) fail() {
	if t.cur != "" {
		t.w.sub(t.step, t.cur, Failed)
		t.w.h.Log(fmt.Sprintf("provision: %s: %s: failed", t.step, t.cur))
		t.cur = ""
	}
}
