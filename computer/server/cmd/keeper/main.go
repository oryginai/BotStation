// keeper holds the computer's undo history and its audit log, in its own
// container. The agent drives the computer container (as root, through the
// terminal if nothing else), so anything stored there can be erased by the
// agent; keeper's volumes are mounted nowhere else. The computer can only
// append to the log and ask for a snapshot.
//
// Snapshots: rsync copies of the home folder (/root), hard-linked to the
// previous snapshot, so an unchanged file costs nothing. One is taken every
// SNAPSHOT_INTERVAL seconds if anything changed, and whenever desktopd asks
// (before agent shell commands, and before the first agent action after a
// quiet spell). Each one records what changed since the one before.
//
//	keeper serve                     the service (internal HTTP on :8090)
//	keeper list                      snapshots, newest first
//	keeper snapshot [reason]         take one now (if anything changed)
//	keeper show <id>                 what changed just before that snapshot
//	keeper diff <id> [path]          what restoring it would change
//	keeper restore <id> [path]       put the home folder (or one path) back
//	keeper log [-n 50] [-since id|time] [-json]   the audit log
//
// Internal HTTP (the computer container only):
//
//	POST /audit      one JSON record, appended as one line
//	POST /snapshot   {"reason", "max_age"?: seconds} -> {"id", "taken"}
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	homeDir  = env("KEEPER_HOME", "/root")
	snapDir  = env("KEEPER_SNAPSHOTS", "/snapshots")
	auditLog = env("KEEPER_AUDIT", "/audit/audit.jsonl")
	interval = time.Duration(envInt("SNAPSHOT_INTERVAL", 60)) * time.Second
	keep     = envInt("SNAPSHOT_KEEP", 100)
)

// Never worth restoring, and churns constantly
var excludes = []string{"--exclude=/.cache/"}

const idFormat = "2006-01-02_15-04-05"

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "list":
		err = cmdList()
	case "snapshot":
		reason := "by hand"
		if len(args) > 0 {
			reason = strings.Join(args, " ")
		}
		var id string
		var taken bool
		if id, taken, err = take(reason, 0); err == nil {
			fmt.Printf("%s (%s)\n", id, map[bool]string{true: "new", false: "nothing changed since"}[taken])
		}
	case "show":
		err = cmdShow(args)
	case "diff":
		err = cmdDiff(args)
	case "restore":
		err = cmdRestore(args)
	case "log":
		err = cmdLog(args)
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: keeper serve | list | snapshot [reason] | show <id> | diff <id> [path] | restore <id> [path] | log [-n N] [-since id|time] [-json]")
	os.Exit(2)
}

// --- snapshots ---

type meta struct {
	ID      string   `json:"id"`
	Time    string   `json:"time"`
	Reason  string   `json:"reason"`
	Changes []change `json:"changes"` // since the snapshot before
}

type change struct {
	Op   string `json:"op"` // added, changed, deleted
	Path string `json:"path"`
}

var (
	snapMu    sync.Mutex
	lastCheck time.Time // last time the home folder was compared with the newest snapshot
)

// take snapshots the home folder unless nothing changed since the newest
// snapshot, or that check is younger than maxAge. It returns the snapshot
// that now matches the home folder.
func take(reason string, maxAge time.Duration) (id string, taken bool, err error) {
	snapMu.Lock()
	defer snapMu.Unlock()
	// The service and keeper commands (restore, snapshot) may run at once
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		return "", false, err
	}
	lock, err := os.OpenFile(filepath.Join(snapDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", false, err
	}
	prev, _ := newest()
	if prev != "" && maxAge > 0 && time.Since(lastCheck) < maxAge {
		return prev, false, nil
	}
	var changes []change
	if prev != "" {
		// What changed since the newest snapshot (dry run, nothing copied)
		changes, err = rsyncChanges(homeDir+"/", filepath.Join(snapDir, prev, "files")+"/", "--dry-run")
		if err != nil {
			return "", false, err
		}
		if len(changes) == 0 {
			lastCheck = time.Now()
			return prev, false, nil
		}
	}

	now := time.Now()
	id = now.Format(idFormat)
	for n := 2; exists(filepath.Join(snapDir, id)); n++ {
		id = now.Format(idFormat) + "-" + strconv.Itoa(n)
	}
	tmp := filepath.Join(snapDir, ".tmp-"+id)
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", false, err
	}
	args := []string{"-a", "--delete"}
	if prev != "" {
		args = append(args, "--link-dest="+filepath.Join(snapDir, prev, "files"))
	}
	args = append(args, excludes...)
	args = append(args, homeDir+"/", filepath.Join(tmp, "files")+"/")
	if out, err := exec.Command("rsync", args...).CombinedOutput(); err != nil && !partial(err) {
		os.RemoveAll(tmp)
		return "", false, fmt.Errorf("rsync: %v: %s", err, out)
	}
	if prev == "" {
		changes = []change{{Op: "added", Path: "(first snapshot: everything)"}}
	}
	m := meta{ID: id, Time: now.Format(time.RFC3339), Reason: reason, Changes: changes}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "meta.json"), b, 0o600); err != nil {
		return "", false, err
	}
	if err := os.Rename(tmp, filepath.Join(snapDir, id)); err != nil {
		return "", false, err
	}
	lastCheck = now
	log.Printf("snapshot %s (%s): %d change(s)", id, reason, len(changes))
	prune()
	return id, true, nil
}

// partial: rsync exit 24 means files vanished while copying (an app was
// writing); the rest of the copy is fine
func partial(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 24
}

// rsyncChanges runs rsync with --itemize-changes and returns what it did
// (or, with --dry-run, would do) to dst
func rsyncChanges(src, dst string, extra ...string) ([]change, error) {
	args := append([]string{"-a", "--delete", "--itemize-changes"}, extra...)
	args = append(args, excludes...)
	args = append(args, src, dst)
	out, err := exec.Command("rsync", args...).Output()
	if err != nil && !partial(err) {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("rsync: %v: %s", err, ee.Stderr)
		}
		return nil, err
	}
	return parseItemized(string(out)), nil
}

// parseItemized reads rsync's --itemize-changes lines ("YXcstpoguax path",
// "*deleting   path"). A directory whose only change is its timestamp is
// left out: it just means something inside it changed.
func parseItemized(out string) []change {
	var cs []change
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 13 {
			continue
		}
		item, path := line[:11], strings.TrimSpace(line[12:])
		switch {
		case path == "./" || path == "." || strings.HasPrefix(line, "created directory "):
			continue
		case strings.HasPrefix(item, "*deleting"):
			cs = append(cs, change{"deleted", path})
		case strings.Contains(item, "+++++++"):
			cs = append(cs, change{"added", path})
		case item[1] == 'd' && item[0] == '.':
			continue
		default:
			cs = append(cs, change{"changed", path})
		}
	}
	return cs
}

// snapshots returns the snapshot ids, oldest first
func snapshots() []string {
	ents, _ := os.ReadDir(snapDir)
	var ids []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && exists(filepath.Join(snapDir, e.Name(), "meta.json")) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids
}

func newest() (string, bool) {
	ids := snapshots()
	if len(ids) == 0 {
		return "", false
	}
	return ids[len(ids)-1], true
}

func prune() {
	ids := snapshots()
	for len(ids) > keep {
		os.RemoveAll(filepath.Join(snapDir, ids[0]))
		ids = ids[1:]
	}
}

func readMeta(id string) (meta, error) {
	var m meta
	b, err := os.ReadFile(filepath.Join(snapDir, id, "meta.json"))
	if err != nil {
		return m, fmt.Errorf("no snapshot %q (keeper list shows them)", id)
	}
	return m, json.Unmarshal(b, &m)
}

// restorePaths returns rsync's source and destination for restoring path
// (relative to the home folder; "" = all of it) from snapshot id
func restorePaths(id, path string) (src, dst string, err error) {
	if _, err := readMeta(id); err != nil {
		return "", "", err
	}
	path = strings.TrimPrefix(strings.TrimPrefix(path, homeDir), "/root/")
	clean := filepath.Clean("/" + path)[1:] // no way out of the home folder
	src = filepath.Join(snapDir, id, "files", clean)
	dst = filepath.Join(homeDir, clean)
	fi, err := os.Lstat(src)
	if err != nil {
		return "", "", fmt.Errorf("%s is not in snapshot %s", "/root/"+clean, id)
	}
	if fi.IsDir() {
		src, dst = src+"/", dst+"/"
	}
	return src, dst, nil
}

// --- audit log ---

var auditMu sync.Mutex

func appendAudit(rec map[string]any) error {
	b, err := json.Marshal(rec) // one line: Marshal never writes newlines
	if err != nil {
		return err
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(auditLog), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(auditLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// --- service ---

func serve() error {
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		return err
	}
	// Leftovers of a snapshot cut off by a stop
	if tmps, _ := filepath.Glob(filepath.Join(snapDir, ".tmp-*")); len(tmps) > 0 {
		for _, t := range tmps {
			os.RemoveAll(t)
		}
	}
	go func() {
		if _, _, err := take("startup", 0); err != nil {
			log.Printf("snapshot: %v", err)
		}
		for range time.Tick(interval) {
			if _, _, err := take("every "+interval.String(), 0); err != nil {
				log.Printf("snapshot: %v", err)
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /audit", func(w http.ResponseWriter, r *http.Request) {
		var rec map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&rec); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		rec["received"] = time.Now().Format(time.RFC3339Nano)
		if err := appendAudit(rec); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /snapshot", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Reason string  `json:"reason"`
			MaxAge float64 `json:"max_age"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		if req.Reason == "" {
			req.Reason = "requested"
		}
		id, taken, err := take(req.Reason, time.Duration(req.MaxAge*float64(time.Second)))
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "taken": taken})
	})
	addr := env("KEEPER_LISTEN", ":8090")
	log.Printf("keeper on %s: snapshots of %s every %s if changed (keeping %d), audit log %s", addr, homeDir, interval, keep, auditLog)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

// --- commands ---

func cmdList() error {
	ids := snapshots()
	if len(ids) == 0 {
		fmt.Println("No snapshots yet.")
		return nil
	}
	for i := len(ids) - 1; i >= 0; i-- {
		m, err := readMeta(ids[i])
		if err != nil {
			continue
		}
		fmt.Printf("%-22s %-28s %s\n", m.ID, m.Reason, summarize(m.Changes))
	}
	return nil
}

func summarize(cs []change) string {
	if len(cs) == 0 {
		return "no changes"
	}
	n := map[string]int{}
	for _, c := range cs {
		n[c.Op]++
	}
	var parts []string
	for _, op := range []string{"added", "changed", "deleted"} {
		if n[op] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[op], op))
		}
	}
	first := cs[0].Path
	if len(cs) > 1 {
		first += ", ..."
	}
	return strings.Join(parts, ", ") + ": " + first
}

func cmdShow(args []string) error {
	if len(args) != 1 {
		usage()
	}
	m, err := readMeta(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("Snapshot %s (%s), taken %s. Changes since the snapshot before:\n", m.ID, m.Reason, m.Time)
	printChanges(m.Changes, "")
	return nil
}

func printChanges(cs []change, prefix string) {
	if len(cs) == 0 {
		fmt.Println("  (none)")
	}
	for _, c := range cs {
		fmt.Printf("  %-8s /root/%s\n", c.Op, prefix+c.Path)
	}
}

// restoreChanges lists what restoring would do, worded for the person
// deciding: rsync's view of the home folder, from the snapshot's side
func restoreChanges(id, path string) ([]change, string, error) {
	src, dst, err := restorePaths(id, path)
	if err != nil {
		return nil, "", err
	}
	cs, err := rsyncChanges(src, dst, "--dry-run")
	if err != nil {
		return nil, "", err
	}
	prefix := strings.TrimPrefix(strings.TrimPrefix(dst, homeDir), "/")
	if !strings.HasSuffix(dst, "/") { // a single file: rsync prints its name only
		prefix = strings.TrimSuffix(prefix, filepath.Base(prefix))
	}
	for i, c := range cs {
		switch c.Op {
		case "added":
			cs[i].Op = "bring back"
		case "changed":
			cs[i].Op = "revert"
		case "deleted":
			cs[i].Op = "remove" // created after the snapshot
		}
	}
	return cs, prefix, nil
}

func cmdDiff(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		usage()
	}
	path := ""
	if len(args) == 2 {
		path = args[1]
	}
	cs, prefix, err := restoreChanges(args[0], path)
	if err != nil {
		return err
	}
	_, dst, _ := restorePaths(args[0], path)
	fmt.Printf("Restoring %s from %s would:\n", shown(dst), args[0])
	for _, c := range cs {
		fmt.Printf("  %-10s /root/%s\n", c.Op, prefix+c.Path)
	}
	if len(cs) == 0 {
		fmt.Println("  change nothing (it already matches)")
	}
	return nil
}

func cmdRestore(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		usage()
	}
	id, path := args[0], ""
	if len(args) == 2 {
		path = args[1]
	}
	cs, prefix, err := restoreChanges(id, path)
	if err != nil {
		return err
	}
	// The current state first, so the restore itself can be undone
	before, _, err := take("before restoring "+id, 0)
	if err != nil {
		return fmt.Errorf("snapshot before restoring: %v", err)
	}
	src, dst, _ := restorePaths(id, path)
	start := time.Now()
	if err := os.MkdirAll(filepath.Dir(strings.TrimSuffix(dst, "/")), 0o755); err != nil {
		return err
	}
	out, err := exec.Command("rsync", append(append([]string{"-a", "--delete"}, excludes...), src, dst)...).CombinedOutput()
	if err != nil && !partial(err) {
		return fmt.Errorf("rsync: %v: %s", err, out)
	}
	took := time.Since(start)
	paths := make([]string, 0, len(cs))
	for _, c := range cs {
		paths = append(paths, c.Op+" /root/"+prefix+c.Path)
	}
	appendAudit(map[string]any{
		"ts": time.Now().Format(time.RFC3339Nano), "source": "operator", "event": "restore",
		"snapshot": id, "path": shown(dst), "undo_with": before, "changes": paths, "ms": took.Milliseconds(),
	})
	fmt.Printf("Restored %s from %s in %d ms (%d change(s)):\n", shown(dst), id, took.Milliseconds(), len(cs))
	for _, c := range cs {
		fmt.Printf("  %-10s /root/%s\n", c.Op, prefix+c.Path)
	}
	fmt.Printf("To undo this restore: keeper restore %s\n", before)
	return nil
}

func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	n := fs.Int("n", 50, "show the last n records (0 = all)")
	since := fs.String("since", "", "only records from this time on (RFC 3339, or a snapshot id)")
	asJSON := fs.Bool("json", false, "print the raw records")
	fs.Parse(args)

	var from time.Time
	if *since != "" {
		var err error
		if from, err = time.Parse(time.RFC3339, *since); err != nil {
			m, merr := readMeta(*since)
			if merr != nil {
				return fmt.Errorf("-since takes an RFC 3339 time or a snapshot id")
			}
			from, _ = time.Parse(time.RFC3339, m.Time)
		}
	}
	f, err := os.Open(auditLog)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("The audit log is empty.")
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := bytes.Clone(sc.Bytes())
		if !from.IsZero() {
			var r struct{ TS string }
			json.Unmarshal(line, &r)
			if t, err := time.Parse(time.RFC3339Nano, r.TS); err == nil && t.Before(from) {
				continue
			}
		}
		lines = append(lines, line)
	}
	if *n > 0 && len(lines) > *n {
		lines = lines[len(lines)-*n:]
	}
	for _, line := range lines {
		if *asJSON {
			os.Stdout.Write(append(line, '\n'))
			continue
		}
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil {
			fmt.Println(formatRecord(rec))
		}
	}
	return nil
}

// formatRecord turns one audit record into one readable line
func formatRecord(r map[string]any) string {
	ts, _ := r["ts"].(string)
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		ts = t.Local().Format("15:04:05.000")
	}
	str := func(k string) string { s, _ := r[k].(string); return s }
	var what string
	if str("event") == "restore" {
		what = fmt.Sprintf("RESTORE %s from %s (%v change(s); undo with %s)", str("path"), str("snapshot"), lenOf(r["changes"]), str("undo_with"))
		return fmt.Sprintf("%s %-8s %s", ts, "operator", what)
	}
	body, _ := r["body"].(map[string]any)
	path := strings.TrimPrefix(str("path"), "/agent")
	switch path {
	case "/actions":
		var steps []string
		acts, _ := body["actions"].([]any)
		for _, a := range acts {
			if m, ok := a.(map[string]any); ok {
				steps = append(steps, formatAction(m))
			}
		}
		what = "actions: " + strings.Join(steps, "; ")
	case "/exec":
		what = fmt.Sprintf("exec %q", body["command"])
	case "/launch":
		what = fmt.Sprintf("launch %v", compact(body))
	case "/screenshot", "/status", "/windows", "/browser/tabs", "/browser/snapshot", "/clipboard":
		what = strings.ToLower(str("method")) + " " + strings.TrimPrefix(path, "/")
	default:
		what = strings.TrimPrefix(path, "/") + " " + compact(body)
	}
	if notes, ok := r["notes"].(map[string]any); ok && len(notes) > 0 {
		what += " -> " + compact(notes)
	}
	if w := str("window"); w != "" {
		what += fmt.Sprintf("  [in %q]", w)
	}
	status := fmt.Sprint(r["status"])
	if e := str("error"); e != "" {
		status += " " + e
	}
	line := fmt.Sprintf("%s %-8s %s  (%s)", ts, str("source"), what, status)
	if alerts, ok := r["alerts"].([]any); ok && len(alerts) > 0 {
		line = "!! " + line + "  ALERT: " + fmt.Sprint(alerts...)
	}
	return line
}

func formatAction(a map[string]any) string {
	at := func() string {
		if x, ok := a["x"]; ok {
			return fmt.Sprintf(" %v,%v", x, a["y"])
		}
		return ""
	}
	switch t, _ := a["type"].(string); t {
	case "type":
		return fmt.Sprintf("type %q", a["text"])
	case "key", "key_down", "key_up":
		return fmt.Sprintf("%s %v", t, a["keys"])
	case "click", "double_click", "move", "down", "up":
		return t + at()
	case "wait":
		return "wait"
	default:
		return t + at()
	}
}

func compact(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func lenOf(v any) int {
	l, _ := v.([]any)
	return len(l)
}

// --- helpers ---

// shown is a path in the home folder as the computer sees it
func shown(p string) string {
	return "/root" + strings.TrimPrefix(p, homeDir)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}
