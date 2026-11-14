//go:build !kombify_full_local

package observability

import (
	"strconv"
	"strings"
	"sync"
	"time"

	sentry "github.com/getsentry/sentry-go"
)

type budgetKey struct {
	at         time.Time
	suppressed int
}

type budgetFingerprint struct {
	start    time.Time
	subjects int
	more     int
}

// ErrorBudget returns a BeforeSend hook that lets one error event per
// fingerprint and subject through per Window, so the Sentry event count scales
// with affected users, not with retries. Suppressed repeats are counted and
// attached to the next allowed event of the same key (tag error_budget.suppressed, context
// error_budget "suppressed_since_last"). Subjects beyond PerFingerprintPerWindow for one
// fingerprint are dropped and counted into error_budget.more_subjects on the
// next allowed event of that fingerprint. next runs first (PII scrubbing); a
// nil result is dropped. Transactions pass through after next.
func ErrorBudget(opts ErrorBudgetOptions, next func(*sentry.Event, *sentry.EventHint) *sentry.Event) func(*sentry.Event, *sentry.EventHint) *sentry.Event {
	if opts.Window <= 0 {
		opts.Window = time.Hour
	}
	if opts.MaxKeys <= 0 {
		opts.MaxKeys = 4096
	}
	if opts.PerFingerprintPerWindow <= 0 {
		opts.PerFingerprintPerWindow = 100
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	budget := &errorBudget{opts: opts, keys: map[string]*budgetKey{}, fps: map[string]*budgetFingerprint{}}
	return func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
		if next != nil {
			event = next(event, hint)
		}
		if event == nil || event.Type == "transaction" {
			return event
		}
		fp := eventFingerprint(event)
		if suppressed, more, ok := budget.admit(fp, fp+"\x00"+eventSubject(event)); ok {
			annotate(event, suppressed, more)
			return event
		}
		return nil
	}
}

type errorBudget struct {
	mu   sync.Mutex
	opts ErrorBudgetOptions
	keys map[string]*budgetKey
	fps  map[string]*budgetFingerprint
}

// admit decides whether the event with this fingerprint and key passes. When
// it does, it returns and resets the counters to attach.
func (b *errorBudget) admit(fp, key string) (suppressed, more int, ok bool) {
	now := b.opts.Now()
	b.mu.Lock()
	defer b.mu.Unlock()

	k, seen := b.keys[key]
	if seen && now.Sub(k.at) < b.opts.Window {
		k.suppressed++
		return 0, 0, false
	}
	f := b.fingerprint(fp, now)
	if f.subjects >= b.opts.PerFingerprintPerWindow {
		f.more++
		return 0, 0, false
	}
	f.subjects++
	if seen {
		suppressed = k.suppressed
		k.at, k.suppressed = now, 0
	} else {
		if len(b.keys) >= b.opts.MaxKeys {
			evictKeys(b.keys, now, b.opts.Window, b.opts.MaxKeys)
		}
		b.keys[key] = &budgetKey{at: now}
	}
	more, f.more = f.more, 0
	return suppressed, more, true
}

// fingerprint returns the fingerprint state, starting a new window when the
// old one expired. The more-subjects counter survives until it is reported.
func (b *errorBudget) fingerprint(fp string, now time.Time) *budgetFingerprint {
	f := b.fps[fp]
	if f != nil && now.Sub(f.start) < b.opts.Window {
		return f
	}
	more := 0
	if f != nil {
		more = f.more
	} else if len(b.fps) >= b.opts.MaxKeys {
		evictFingerprints(b.fps, now, b.opts.Window, b.opts.MaxKeys)
	}
	f = &budgetFingerprint{start: now, more: more}
	b.fps[fp] = f
	return f
}

func annotate(event *sentry.Event, suppressed, more int) {
	if suppressed == 0 && more == 0 {
		return
	}
	if event.Tags == nil {
		event.Tags = map[string]string{}
	}
	if event.Contexts == nil {
		event.Contexts = map[string]sentry.Context{}
	}
	ctx := sentry.Context{}
	if suppressed > 0 {
		event.Tags["error_budget.suppressed"] = strconv.Itoa(suppressed)
		ctx["suppressed_since_last"] = suppressed
	}
	if more > 0 {
		event.Tags["error_budget.more_subjects"] = strconv.Itoa(more)
		ctx["more_subjects"] = more
	}
	event.Contexts["error_budget"] = ctx
}

// evictKeys drops expired entries, then the oldest until there is room for one
// more entry.
func evictKeys(keys map[string]*budgetKey, now time.Time, window time.Duration, max int) {
	for k, v := range keys {
		if now.Sub(v.at) >= window {
			delete(keys, k)
		}
	}
	for len(keys) >= max {
		oldest, first := "", true
		var at time.Time
		for k, v := range keys {
			if first || v.at.Before(at) {
				oldest, at, first = k, v.at, false
			}
		}
		delete(keys, oldest)
	}
}

func evictFingerprints(fps map[string]*budgetFingerprint, now time.Time, window time.Duration, max int) {
	for k, v := range fps {
		if now.Sub(v.start) >= window && v.more == 0 {
			delete(fps, k)
		}
	}
	for len(fps) >= max {
		oldest, first := "", true
		var at time.Time
		for k, v := range fps {
			if first || v.start.Before(at) {
				oldest, at, first = k, v.start, false
			}
		}
		delete(fps, oldest)
	}
}

func eventSubject(event *sentry.Event) string {
	if event.User.ID != "" {
		return event.User.ID
	}
	for _, tag := range []string{"tenant_id", "organization_id", "org_id"} {
		if v := event.Tags[tag]; v != "" {
			return v
		}
	}
	return "system"
}

func eventFingerprint(event *sentry.Event) string {
	if len(event.Fingerprint) > 0 && (len(event.Fingerprint) != 1 || event.Fingerprint[0] != "{{ default }}") {
		return strings.Join(event.Fingerprint, "|")
	}
	if n := len(event.Exception); n > 0 {
		ex := event.Exception[n-1]
		parts := []string{ex.Type, NormalizeFingerprintText(ex.Value)}
		if ex.Stacktrace != nil && len(ex.Stacktrace.Frames) > 0 {
			frames := ex.Stacktrace.Frames
			// Frames are ordered oldest first; the last in-app frame is the one
			// closest to the throw.
			frame := frames[len(frames)-1]
			for k := len(frames) - 1; k >= 0; k-- {
				if frames[k].InApp {
					frame = frames[k]
					break
				}
			}
			parts = append(parts, frame.Module+"."+frame.Function)
		}
		return strings.Join(parts, "|")
	}
	return NormalizeFingerprintText(event.Message)
}
