// Package backgroundlock runs single-instance background ticks under a
// PostgreSQL session advisory lock, so two replicas that overlap during a
// zero-downtime rolling deploy never run the same tick at the same time.
//
// The lock is coordination, not the safety boundary: every tick it guards keeps
// its own row locks, compare-and-set claims and idempotent writes. The lock
// removes concurrent duplicates (double dispatches, racing provider calls,
// wasted passes) on top of them.
//
// One Locker holds at most one pooled connection, and only while one of its
// ticks runs: every key a process holds is taken on that one session. Homelab
// connects to PostgreSQL directly (no transaction pooler), so a session lock
// stays on the backend that took it. A lost session cancels the context of
// every tick it was guarding.
package backgroundlock

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

const (
	// keyNamespace separates these keys from every other advisory key in the
	// database. Single-bigint keys never collide with the two-int4 keys of
	// pg_advisory_xact_lock(hashtext(...), hashtext(...)).
	keyNamespace = "kombify.homelab/background-lock/v1\x00"
	// statementTimeout bounds every lock, unlock and liveness statement.
	statementTimeout = 5 * time.Second
	// heartbeatInterval is how often a session that guards a running tick
	// proves its backend is still the one that took the locks.
	heartbeatInterval = 10 * time.Second
)

var errNoTick = errors.New("backgroundlock: tick function is required")

// Key derives the deterministic advisory key of one named tick. Every replica
// of every revision derives the same key for the same name.
func Key(name string) int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(keyNamespace + name))
	return int64(hash.Sum64())
}

// Locker serializes named ticks across every process that shares the database.
// A nil Locker runs each tick directly: a deployment without a database (or a
// caller without a lock) keeps its in-process single-instance behavior.
type Locker struct {
	database  *sql.DB
	heartbeat time.Duration

	// mu guards the fields below and serializes every statement on the shared
	// session connection.
	mu      sync.Mutex
	session *session
	// running holds the key of every tick this process runs now, whichever
	// session took its lock, so a lost session can never let the same process
	// start a second copy of a tick that is still winding down.
	running map[int64]*session
}

type session struct {
	conn    *sql.Conn
	backend int64
	// keys maps each advisory key held on this session to the cancel function
	// of the tick it guards.
	keys   map[int64]context.CancelFunc
	stop   chan struct{}
	closed bool
}

// New returns a Locker over database, or nil when there is none. Pass the pool
// whose PostgreSQL database every replica shares; any login works, because the
// advisory lock functions are executable by PUBLIC and read no table.
func New(database *sql.DB) *Locker {
	if database == nil {
		return nil
	}
	return &Locker{database: database, heartbeat: heartbeatInterval, running: map[int64]*session{}}
}

// Run runs fn once when this process wins the named lock and reports whether it
// ran. A tick held by another replica (or still running here) is skipped with
// ran=false and a nil error. An error means the lock could not be decided; fn
// did not run. fn receives a context that ends when ctx ends or the session
// holding its lock is lost.
func (l *Locker) Run(ctx context.Context, name string, fn func(context.Context)) (bool, error) {
	if fn == nil {
		return false, errNoTick
	}
	if l == nil || l.database == nil {
		fn(ctx)
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	key := Key(name)
	tickCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	held, acquired, err := l.acquire(ctx, key, cancel)
	if err != nil {
		return false, fmt.Errorf("backgroundlock: acquire %s: %w", name, err)
	}
	if !acquired {
		return false, nil
	}
	defer l.release(held, key)
	fn(tickCtx)
	return true, nil
}

func (l *Locker) acquire(ctx context.Context, key int64, cancel context.CancelFunc) (*session, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, running := l.running[key]; running {
		return nil, false, nil
	}
	current, err := l.openSessionLocked(ctx)
	if err != nil {
		return nil, false, err
	}
	// The statement is detached from the caller's cancellation: the session is
	// shared, so one loop stopping must not discard the locks of the others.
	statementCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), statementTimeout)
	defer stop()
	var locked bool
	if lockErr := current.conn.QueryRowContext(statementCtx, `SELECT pg_catalog.pg_try_advisory_lock($1)`, key).Scan(&locked); lockErr != nil {
		// The server may have granted the lock although the reply was lost.
		// Discard the physical session so no pooled connection keeps it.
		l.endSessionLocked(current, true)
		return nil, false, lockErr
	}
	if !locked {
		l.closeIdleSessionLocked(current)
		return nil, false, nil
	}
	current.keys[key] = cancel
	l.running[key] = current
	return current, true, nil
}

// release unlocks with a detached context, so a canceled tick still frees its
// key. When the unlock cannot be proven the session is discarded: closing the
// backend is the only release that cannot leave a lock behind.
func (l *Locker) release(held *session, key int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[key] == held {
		delete(l.running, key)
	}
	if held.closed {
		// The backend is gone and took the lock with it.
		return
	}
	delete(held.keys, key)
	statementCtx, stop := context.WithTimeout(context.Background(), statementTimeout)
	defer stop()
	var unlocked bool
	if err := held.conn.QueryRowContext(statementCtx, `SELECT pg_catalog.pg_advisory_unlock($1)`, key).Scan(&unlocked); err != nil || !unlocked {
		l.endSessionLocked(held, true)
		return
	}
	l.closeIdleSessionLocked(held)
}

func (l *Locker) openSessionLocked(ctx context.Context) (*session, error) {
	if l.session != nil && !l.session.closed {
		return l.session, nil
	}
	connectCtx, stop := context.WithTimeout(ctx, statementTimeout)
	defer stop()
	conn, err := l.database.Conn(connectCtx)
	if err != nil {
		return nil, err
	}
	var backend int64
	if pidErr := conn.QueryRowContext(connectCtx, `SELECT pg_catalog.pg_backend_pid()`).Scan(&backend); pidErr != nil {
		discard(conn)
		return nil, pidErr
	}
	opened := &session{conn: conn, backend: backend, keys: map[int64]context.CancelFunc{}, stop: make(chan struct{})}
	l.session = opened
	go l.watch(opened)
	return opened, nil
}

// watch proves, while the session guards a tick, that its backend still lives.
// A failed proof ends the session and cancels every tick it guarded, so a tick
// never keeps running after another replica could have taken its lock.
func (l *Locker) watch(watched *session) {
	ticker := time.NewTicker(l.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-watched.stop:
			return
		case <-ticker.C:
		}
		l.mu.Lock()
		if watched.closed {
			l.mu.Unlock()
			return
		}
		statementCtx, stop := context.WithTimeout(context.Background(), statementTimeout)
		var alive bool
		err := watched.conn.QueryRowContext(statementCtx, `SELECT pg_catalog.pg_backend_pid() = $1`, watched.backend).Scan(&alive)
		stop()
		if err != nil || !alive {
			l.endSessionLocked(watched, true)
		}
		l.mu.Unlock()
	}
}

// closeIdleSessionLocked returns a session that holds no key to the pool. With
// no key held it carries no lock, so the pooled connection is clean.
func (l *Locker) closeIdleSessionLocked(idle *session) {
	if len(idle.keys) == 0 {
		l.endSessionLocked(idle, false)
	}
}

// endSessionLocked stops the heartbeat and closes the connection. discard
// drops the physical backend instead of pooling it and cancels every tick the
// session still guarded.
func (l *Locker) endSessionLocked(ended *session, discardBackend bool) {
	if ended.closed {
		return
	}
	ended.closed = true
	close(ended.stop)
	for key, cancel := range ended.keys {
		cancel()
		delete(ended.keys, key)
	}
	if discardBackend {
		discard(ended.conn)
	} else {
		_ = ended.conn.Close()
	}
	if l.session == ended {
		l.session = nil
	}
}

// discard tells database/sql not to return the physical session to the pool,
// so a lock whose release cannot be proven ends with its backend.
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}
