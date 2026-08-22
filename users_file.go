package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// reloadEvery is the minimum interval between users-file mtime checks on the
// request path. maybeReload never stats the file more often than this, even
// under heavy concurrent traffic.
const reloadEvery = 5 * time.Second

// usersFile is a JSON-encoded file of API-key users at path, in the shape
// {"users":[{"name":...,"group":...,"apiKey":...,"limits":{...}}]}.
type usersFile struct {
	path string
}

// newUsersFile returns a usersFile reading from path. It does not touch the
// filesystem — call load to read and parse the file.
func newUsersFile(path string) *usersFile {
	return &usersFile{path: path}
}

// usersFileDoc is the on-disk shape of a users file.
type usersFileDoc struct {
	Users []*UserConfig `json:"users"`
}

// load reads and parses uf's file. path is operator-supplied middleware
// configuration, not untrusted request input.
func (uf *usersFile) load() ([]*UserConfig, error) {
	b, err := os.ReadFile(uf.path) // #nosec G304 -- operator-supplied users file path from middleware config
	if err != nil {
		return nil, fmt.Errorf("llmgateway: cannot read users file %q: %w", uf.path, err)
	}
	var doc usersFileDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("llmgateway: cannot parse users file %q: %w", uf.path, err)
	}
	return doc.Users, nil
}

// gatewayLogger is the logging surface maybeReload needs. *Gateway
// implements it (see logger.go); authStore depends on this small interface
// rather than *Gateway directly, so the reload path can be tested without an
// HTTP handler chain.
type gatewayLogger interface {
	logf(format string, args ...any)
	errorf(format string, args ...any)
}

// maybeReload reloads a's file-sourced users when a.usersFile is configured,
// reloadEvery has elapsed since the last check, and the file's mtime differs
// from the last observed mtime (forward or backward). It runs on the request
// path (ServeHTTP calls it at entry), so it must stay cheap when nothing
// changed and must never panic: on any error — stat, load, or
// replaceFileUsers rejecting the new set — it logs via a.log.errorf and
// keeps the last good set. A successful reload that changes the
// file-sourced user count logs via a.log.logf.
//
// Returns true only when a reload actually replaced the user set (review
// fix, adversarial verification 2026-08-23) — every early-return path
// above (no file configured, throttled, unchanged mtime, a stat/load
// error, or replaceFileUsers itself rejecting the new set) returns
// false. ServeHTTP uses this to gate limiter.pruneRejections: that call
// walks every tracked rejection scope, which is cheap only relative to a
// real reload (itself throttled to at most once per reloadEvery and
// gated on the file's mtime actually changing) — running it on every
// request regardless, the way maybeReload itself must, would add real
// per-request cost to ServeHTTP's hottest path for a reason (deleted
// users hot-reloaded out of the file) that is rare by construction.
func (a *authStore) maybeReload() bool {
	if a.usersFile == nil {
		return false
	}

	// TryLock, not Lock: ServeHTTP calls maybeReload on every request, and a
	// reload's build phase can block on file I/O (resolveSecret's "file:"
	// reads inside replaceFileUsers). Blocking every concurrent request
	// behind one full reload would undo the auth.go buildMu/mu lock split's
	// benefit one layer up. A request that finds a reload already in flight
	// just skips this cycle — the next request's check (or the next
	// throttle window) covers it. Reload is deliberately best-effort and
	// throttled, not "every change lands on the very next request."
	if !a.reloadMu.TryLock() {
		return false
	}
	defer a.reloadMu.Unlock()

	now := a.nowFn()
	if now.Sub(a.lastCheck) < reloadEvery {
		return false
	}
	a.lastCheck = now

	info, err := os.Stat(a.usersFile.path)
	if err != nil {
		a.log.errorf("llmgateway: cannot stat users file %q: %v", a.usersFile.path, err)
		return false
	}
	// Equal, not After: an After-only check misses a backward mtime move —
	// a restore from backup, or a `cp -p` from an older file. Any
	// difference from the last observed mtime, forward or backward, must
	// trigger a reload; only an unchanged mtime skips it.
	if info.ModTime().Equal(a.lastModTime) {
		return false
	}

	users, err := a.usersFile.load()
	if err != nil {
		a.log.errorf("llmgateway: users file %q reload failed: %v", a.usersFile.path, err)
		return false
	}

	a.mu.RLock()
	prevCount := a.fileUserCount
	a.mu.RUnlock()

	if err := a.replaceFileUsers(users); err != nil {
		a.log.errorf("llmgateway: users file %q reload rejected: %v", a.usersFile.path, err)
		return false
	}
	a.lastModTime = info.ModTime()

	if len(users) != prevCount {
		a.log.logf("llmgateway: users file %q reload: user count changed from %d to %d", a.usersFile.path, prevCount, len(users))
	}
	return true
}
