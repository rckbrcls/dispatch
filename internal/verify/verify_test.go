package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DISPATCH_HOME", home)
	return home
}

type gitRepo struct {
	t    *testing.T
	root string
}

func newRepo(t *testing.T) *gitRepo {
	t.Helper()
	g := &gitRepo{t: t, root: t.TempDir()}
	g.git("init", "-q", "-b", "main")
	g.git("config", "user.name", "test")
	g.git("config", "user.email", "test@localhost")
	return g
}

func (g *gitRepo) git(args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", g.root}, args...)...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func (g *gitRepo) write(path, body string, mode os.FileMode) {
	g.t.Helper()
	full := filepath.Join(g.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), mode); err != nil {
		g.t.Fatal(err)
	}
}

func (g *gitRepo) commit(msg string) string {
	g.t.Helper()
	g.git("add", "-A")
	g.git("commit", "-qm", msg)
	return g.git("rev-parse", "HEAD")
}

func assertNoBaseline(t *testing.T, home, repo string) {
	t.Helper()
	tmp := filepath.Join(home, "tmp")
	ents, err := os.ReadDir(tmp)
	if err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "baseline-") {
				t.Fatalf("baseline temp left behind: %s", filepath.Join(tmp, e.Name()))
			}
		}
	}
	if repo == "" {
		return
	}
	out, _ := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").CombinedOutput()
	if strings.Contains(string(out), filepath.Join(home, "tmp")) {
		t.Fatalf("worktree list still has baseline:\n%s", out)
	}
}

func regressions(res Result) []string {
	var out []string
	for _, c := range res.Checks {
		out = append(out, c.Regressions...)
	}
	return out
}

func preexisting(res Result) []string {
	var out []string
	for _, c := range res.Checks {
		out = append(out, c.Preexisting...)
	}
	return out
}

const (
	answerGo  = "package pkg\n\nfunc Answer() int { return 42 }\n"
	passTest  = "package pkg\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fatal(Answer())\n\t}\n}\n"
	goTestCfg = `{
  "checks": [
    {"name": "test", "argv": ["go", "test", "./..."], "timeout": "2m", "required": true, "parse": "gotest"}
  ]
}
`
)

func TestGoodModuleVerified(t *testing.T) {
	home := setHome(t)
	g := newRepo(t)
	g.write("go.mod", "module pkg\n\ngo 1.22\n", 0o644)
	g.write("answer.go", answerGo, 0o644)
	g.write("answer_test.go", passTest, 0o644)
	base := g.commit("base")
	res, err := Run(context.Background(), Options{Repo: g.root, Worktree: g.root, BaseSHA: base})
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Status != StatusVerified {
		t.Fatalf("status %s %+v", res.Status, res.Checks)
	}
	names := map[string]CheckStatus{}
	for _, c := range res.Checks {
		names[c.Name] = c.Status
		if len(c.Regressions) > 0 {
			t.Fatalf("regression %+v", c)
		}
	}
	if names["vet"] != CheckPassed || names["test"] != CheckPassed {
		t.Fatalf("checks %+v", res.Checks)
	}
	assertNoBaseline(t, home, g.root)
}

func TestNewFailureIsRegression(t *testing.T) {
	home := setHome(t)
	g := newRepo(t)
	g.write("go.mod", "module pkg\n\ngo 1.22\n", 0o644)
	g.write("answer.go", answerGo, 0o644)
	g.write("answer_test.go", passTest, 0o644)
	g.write(".dispatch/verify.json", goTestCfg, 0o644)
	base := g.commit("base")
	g.write("answer.go", "package pkg\n\nfunc Answer() int { return 0 }\n", 0o644)
	g.commit("break")
	res, err := Run(context.Background(), Options{Repo: g.root, Worktree: g.root, BaseSHA: base})
	if err != nil {
		t.Fatalf("%v tail=%s", err, tails(res))
	}
	if res.Status != StatusFailed || !slices.Contains(regressions(res), "pkg.TestX") {
		t.Fatalf("status %s regs %v tails %s", res.Status, regressions(res), tails(res))
	}
	assertNoBaseline(t, home, g.root)
}

func TestPreexistingFailureStaysVerified(t *testing.T) {
	home := setHome(t)
	g := newRepo(t)
	g.write("go.mod", "module pkg\n\ngo 1.22\n", 0o644)
	g.write("answer.go", answerGo, 0o644)
	g.write("answer_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {\n\tt.Fatal(\"old\")\n}\n", 0o644)
	g.write(".dispatch/verify.json", goTestCfg, 0o644)
	base := g.commit("base")
	g.write("note.txt", "unrelated\n", 0o644)
	g.commit("note")
	res, err := Run(context.Background(), Options{Repo: g.root, Worktree: g.root, BaseSHA: base})
	if err != nil {
		t.Fatalf("%v %s", err, tails(res))
	}
	if res.Status != StatusVerified || !slices.Contains(preexisting(res), "pkg.TestX") || len(regressions(res)) != 0 {
		t.Fatalf("status %s pre %v reg %v", res.Status, preexisting(res), regressions(res))
	}
	assertNoBaseline(t, home, g.root)
}

func TestNothingToVerify(t *testing.T) {
	home := setHome(t)
	g := newRepo(t)
	g.write("README", "hi\n", 0o644)
	base := g.commit("base")
	res, err := Run(context.Background(), Options{Repo: g.root, Worktree: g.root, BaseSHA: base})
	if err != nil || res.Status != StatusUnverified {
		t.Fatalf("%v %+v", err, res)
	}
	assertNoBaseline(t, home, g.root)
}

func TestBaselineCacheReused(t *testing.T) {
	home := setHome(t)
	g := newRepo(t)
	g.write("fail.sh", "#!/bin/sh\nexit 1\n", 0o755)
	g.write(".dispatch/verify.json", `{"checks":[{"name":"fail","argv":["./fail.sh"],"timeout":"5s","parse":"none"}]}`, 0o644)
	base := g.commit("base")
	opt := Options{Repo: g.root, Worktree: g.root, BaseSHA: base}
	res, err := Run(context.Background(), opt)
	if err != nil || res.Status != StatusVerified || res.BaselineCached || !slices.Contains(preexisting(res), "fail:exit1") {
		t.Fatalf("first %+v %v pre %v", res.Status, err, preexisting(res))
	}
	assertNoBaseline(t, home, g.root)

	dir := filepath.Join(home, "baselines")
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("cache entries: %v %v", ents, err)
	}
	path := filepath.Join(dir, ents[0].Name())
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	poisoned := bytes.Replace(b, []byte("fail:exit1"), []byte("fail:exit9"), 1)
	if bytes.Equal(poisoned, b) {
		t.Fatalf("cache did not contain the failure id:\n%s", b)
	}
	if err := os.WriteFile(path, poisoned, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = Run(context.Background(), opt)
	if err != nil || !res.BaselineCached || res.Status != StatusFailed || !slices.Contains(regressions(res), "fail:exit1") {
		t.Fatalf("second cached=%v status=%s err=%v regs=%v", res.BaselineCached, res.Status, err, regressions(res))
	}
	assertNoBaseline(t, home, g.root)
}

func TestCheckTimeoutKillsTree(t *testing.T) {
	home := setHome(t)
	g := newRepo(t)
	pidfile := filepath.Join(t.TempDir(), "pids")
	t.Cleanup(func() { killPids(pidfile) })
	g.write("hang.sh", "#!/bin/sh\nif [ -f SLOW ]; then\n  sleep 60 &\n  echo $! >> \"$1\"\n  wait\nfi\nexit 0\n", 0o755)
	g.write(".dispatch/verify.json", fmt.Sprintf(`{"checks":[{"name":"hang","argv":["./hang.sh",%q],"timeout":"2s","parse":"none"}]}`, pidfile), 0o644)
	base := g.commit("base")
	g.write("SLOW", "1\n", 0o644)
	g.commit("slow")
	res, err := Run(context.Background(), Options{Repo: g.root, Worktree: g.root, BaseSHA: base})
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Status != StatusFailed || !slices.Contains(regressions(res), "hang:timeout") {
		t.Fatalf("status %s regs %v", res.Status, regressions(res))
	}
	pids := readPids(t, pidfile)
	if len(pids) == 0 {
		t.Fatal("child pid was not recorded")
	}
	for _, pid := range pids {
		waitDead(t, pid)
	}
	assertNoBaseline(t, home, g.root)
}

func TestBaselineWorktreeRemoved(t *testing.T) {
	t.Run("add failure", func(t *testing.T) {
		home := setHome(t)
		g := newRepo(t)
		g.write("README", "hi\n", 0o644)
		g.commit("base")
		if _, err := addDetached(context.Background(), g.root, strings.Repeat("ab", 20)); err == nil {
			t.Fatal("expected worktree add to fail")
		}
		assertNoBaseline(t, home, g.root)
	})
	t.Run("cancel", func(t *testing.T) {
		home := setHome(t)
		g := newRepo(t)
		pidfile := filepath.Join(t.TempDir(), "pids")
		t.Cleanup(func() { killPids(pidfile) })
		g.write("slow.sh", "#!/bin/sh\nsleep 60 &\necho $! > \"$1\"\nwait\n", 0o755)
		g.write(".dispatch/verify.json", fmt.Sprintf(`{"checks":[{"name":"slow","argv":["./slow.sh",%q],"timeout":"5s","parse":"none"}]}`, pidfile), 0o644)
		base := g.commit("base")
		g.write("slow.sh", "#!/bin/sh\nexit 1\n", 0o755)
		g.commit("fast-fail")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			deadline := time.Now().Add(4 * time.Second)
			for time.Now().Before(deadline) {
				b, err := os.ReadFile(pidfile)
				if err == nil && len(bytes.TrimSpace(b)) > 0 {
					time.Sleep(30 * time.Millisecond)
					cancel()
					return
				}
				time.Sleep(15 * time.Millisecond)
			}
			cancel()
		}()
		res, err := Run(ctx, Options{Repo: g.root, Worktree: g.root, BaseSHA: base})
		if err == nil || res.Status != StatusError {
			t.Fatalf("err=%v status=%s regs=%v", err, res.Status, regressions(res))
		}
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err %v", err)
		}
		for _, pid := range readPids(t, pidfile) {
			waitDead(t, pid)
		}
		if _, err := os.Stat(pidfile); err != nil {
			t.Fatal("baseline script never started")
		}
		assertNoBaseline(t, home, g.root)
	})
}

func TestCheapChecks(t *testing.T) {
	home := setHome(t)
	g := newRepo(t)
	g.write("foo_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestKeep(t *testing.T) {}\nfunc TestDrop(t *testing.T) {}\n", 0o644)
	g.write("ok.go", "package pkg\n\nfunc F() int { return 1 }\n", 0o644)
	base := g.commit("base")
	g.write("foo_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestKeep(t *testing.T) {}\n", 0o644)
	g.write("bad.go", "package pkg\n\nfunc F() { \n}\n", 0o644)
	g.write("conflict.txt", "<<<<<<< HEAD\n", 0o644)
	g.write("go.sum", "h1:placeholder\n", 0o644)
	g.write(".github/workflows/ci.yml", "name: ci\n", 0o644)
	g.write(".dispatch/verify.json", `{"checks":[{"name":"ok","argv":["/bin/sh","-c","exit 0"],"timeout":"5s"}]}`, 0o644)
	g.commit("bad")
	res, err := Run(context.Background(), Options{Repo: g.root, Worktree: g.root, BaseSHA: base})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("status %s", res.Status)
	}
	var sawDiff, sawConflict bool
	for _, c := range res.Checks {
		if c.Name == checkDiff && c.Status == CheckFailed && len(c.Regressions) > 0 {
			sawDiff = true
		}
		if c.Name == checkConflicts && slices.ContainsFunc(c.Regressions, func(s string) bool { return strings.Contains(s, "<<<<<<<") }) {
			sawConflict = true
		}
	}
	if !sawDiff || !sawConflict {
		t.Fatalf("checks %+v", res.Checks)
	}
	joined := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"TestDrop", "go.sum", ".github/workflows/ci.yml"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings %q missing %s", joined, want)
		}
	}
	assertNoBaseline(t, home, g.root)
}

func TestMovedTestIsNotAWarning(t *testing.T) {
	diff := `diff --git a/a_test.go b/a_test.go
--- a/a_test.go
+++ b/a_test.go
@@ -1 +0,0 @@
-func TestDrop(t *testing.T) {}
diff --git a/b_test.go b/b_test.go
--- /dev/null
+++ b/b_test.go
@@ -0,0 +1 @@
+func TestDrop(t *testing.T) {}
+func (s *Suite) TestKeep(t *testing.T) {}
`
	conflicts, removed := scanPatch(diff)
	if len(removed) != 0 || len(conflicts) != 0 {
		t.Fatalf("conflicts %v removed %v", conflicts, removed)
	}
	conflicts, removed = scanPatch(diff + "diff --git a/c_test.go b/c_test.go\n--- a/c_test.go\n+++ b/c_test.go\n@@ -1 +0,0 @@\n-func TestGone(t *testing.T) {}\n")
	if len(removed) != 1 || !strings.Contains(removed[0], "TestGone") {
		t.Fatalf("removed %v", removed)
	}
}

func TestParseGoTest(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "package rollup is not a separate failure",
			in: `{"Action":"run","Package":"pkg","Test":"TestX"}
{"Action":"output","Package":"pkg","Test":"TestX","Output":"--- FAIL: TestX\n"}
{"Action":"fail","Package":"pkg","Test":"TestX"}
{"Action":"fail","Package":"pkg"}`,
			want: []string{"pkg.TestX"},
		},
		{
			name: "build failure is the package",
			in: `{"Action":"output","Package":"pkg","Output":"FAIL\tpkg [build failed]\n"}
{"Action":"fail","Package":"pkg"}`,
			want: []string{"pkg"},
		},
		{
			name: "subtest and parent",
			in: `{"Action":"fail","Package":"pkg","Test":"TestX/one"}
{"Action":"fail","Package":"pkg","Test":"TestX"}
{"Action":"fail","Package":"pkg"}`,
			want: []string{"pkg.TestX", "pkg.TestX/one"},
		},
		{
			name: "pass ignored",
			in:   `{"Action":"pass","Package":"pkg","Test":"TestX"}` + "\n" + `{"Action":"pass","Package":"pkg"}`,
		},
		{
			name: "non-json ignored",
			in:   "not json\n{\"Action\":\"fail\",\"Package\":\"p\",\"Test\":\"TestY\"}\n",
			want: []string{"p.TestY"},
		},
		{
			name: "dotted module path and build failure",
			in: `{"Action":"fail","Package":"example.com/pkg","Test":"TestX"}
{"Action":"fail","Package":"example.com/broken"}`,
			want: []string{"example.com/broken", "example.com/pkg.TestX"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGoTest(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestWithGoTestJSON(t *testing.T) {
	if got := withGoTestJSON([]string{"go", "test", "./..."}); !slices.Equal(got, []string{"go", "test", "-json", "./..."}) {
		t.Fatalf("%v", got)
	}
	if got := withGoTestJSON([]string{"go", "test", "-json", "./..."}); !slices.Equal(got, []string{"go", "test", "-json", "./..."}) {
		t.Fatalf("%v", got)
	}
	if got := withGoTestJSON([]string{"go", "test", "-count=1", "./..."}); !slices.Equal(got, []string{"go", "test", "-json", "-count=1", "./..."}) {
		t.Fatalf("%v", got)
	}
	if got := withGoTestJSON([]string{"/usr/local/go/bin/go", "test", "./..."}); !slices.Equal(got, []string{"/usr/local/go/bin/go", "test", "-json", "./..."}) {
		t.Fatalf("%v", got)
	}
	if got := withGoTestJSON([]string{"go", "vet", "./..."}); !slices.Equal(got, []string{"go", "vet", "./..."}) {
		t.Fatalf("%v", got)
	}
}

func TestTailIsBoundedAndRedacted(t *testing.T) {
	home := setHome(t)
	g := newRepo(t)
	g.write("talk.sh", "#!/bin/sh\nhead -c 20000 /dev/zero | tr '\\0' 'A'\necho\necho TOKEN=supersecret\necho END\nexit 0\n", 0o755)
	g.write(".dispatch/verify.json", `{"checks":[{"name":"talk","argv":["./talk.sh"],"timeout":"5s"}]}`, 0o644)
	base := g.commit("base")
	res, err := Run(context.Background(), Options{Repo: g.root, Worktree: g.root, BaseSHA: base})
	if err != nil || res.Status != StatusVerified {
		t.Fatalf("%v %+v", err, res)
	}
	if len(res.Checks) != 1 {
		t.Fatalf("%+v", res.Checks)
	}
	tail := res.Checks[0].Tail
	if len(tail) > tailLimit || !strings.Contains(tail, "END") || !strings.Contains(tail, "[REDACTED]") || strings.Contains(tail, "supersecret") {
		t.Fatalf("tail len %d: %q", len(tail), tail[max(0, len(tail)-200):])
	}
	assertNoBaseline(t, home, g.root)
}

func TestConfigDefaults(t *testing.T) {
	g := newRepo(t)
	g.write("go.mod", "module pkg\n\ngo 1.22\n", 0o644)
	cfg, err := loadConfig(g.root, g.root)
	if err != nil || cfg.skip || len(cfg.checks) != 2 || cfg.checks[0].Name != "vet" || cfg.checks[1].Parse != "gotest" || !cfg.checks[1].Required {
		t.Fatalf("%+v %v", cfg, err)
	}
	g.write(".dispatch/verify.json", `{"checks":[{"name":"t","argv":["go","test","./..."],"required":false}]}`, 0o644)
	cfg, err = loadConfig(g.root, g.root)
	if err != nil || cfg.checks[0].Required || cfg.checks[0].Parse != "none" || cfg.checks[0].Timeout != defaultTimeout {
		t.Fatalf("%+v %v", cfg.checks, err)
	}
	g.write(".dispatch/verify.json", `{"checks":[{"name":"t","argv":["echo"],"parse":"nope"}]}`, 0o644)
	if _, err := loadConfig(g.root, g.root); err == nil {
		t.Fatal("accepted bad parse")
	}
}

func TestFailureWithoutBaseIsError(t *testing.T) {
	home := setHome(t)
	g := newRepo(t)
	g.write("fail.sh", "#!/bin/sh\nexit 1\n", 0o755)
	g.write(".dispatch/verify.json", `{"checks":[{"name":"fail","argv":["./fail.sh"],"timeout":"5s"}]}`, 0o644)
	g.commit("base")
	res, err := Run(context.Background(), Options{Repo: g.root, Worktree: g.root})
	if err == nil || res.Status != StatusError {
		t.Fatalf("%v %+v", err, res)
	}
	assertNoBaseline(t, home, g.root)
}

func tails(res Result) string {
	var b strings.Builder
	for _, c := range res.Checks {
		fmt.Fprintf(&b, "%s:%s %s\n", c.Name, c.Status, c.Tail)
	}
	return b.String()
}

func readPids(t *testing.T, path string) []int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		var pid int
		if _, err := fmt.Sscan(f, &pid); err == nil && pid > 0 {
			out = append(out, pid)
		}
	}
	return out
}

func killPids(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, f := range strings.Fields(string(b)) {
		var pid int
		if _, err := fmt.Sscan(f, &pid); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive", pid)
}
