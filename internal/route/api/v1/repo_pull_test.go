package v1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gogs.io/gogs/internal/route/api/v1/types"
)

// TestPullRequests runs the pull request endpoints against a real Gogs binary
// backed by SQLite, so that git hooks, patch generation and merging all happen
// the same way as in production.
func TestPullRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("skipping end-to-end test because git is not installed")
	}

	srv := startTestServer(t)
	alice := srv.client(t, "alice", srv.adminToken)

	resp := alice.do(http.MethodPost, "/admin/users", map[string]any{
		"username": "bob",
		"email":    "bob@example.com",
		"password": "bob-password",
	})
	require.Equal(t, http.StatusCreated, resp.status, resp.body)
	bob := srv.client(t, "bob", srv.createToken(t, "bob", "bob-password"))

	resp = alice.do(http.MethodPost, "/user/repos", map[string]any{
		"name":      "demo",
		"auto_init": true,
		"readme":    "Default",
	})
	require.Equal(t, http.StatusCreated, resp.status, resp.body)

	work := srv.clone(t, "alice", "alice-password", "alice/demo")
	srv.git(t, work, "checkout", "-q", "-b", "same")
	srv.git(t, work, "push", "-q", "origin", "same")
	srv.commitFile(t, work, "feature", "feature.txt", "hello from feature\n")
	srv.commitFile(t, work, "second", "second.txt", "hello from second\n")
	srv.commitFile(t, work, "conflict", "README.md", "changed on conflict\n")
	srv.commitFile(t, work, "master", "README.md", "changed on master\n")
	revParse := func(rev string) string {
		return strings.TrimSpace(srv.git(t, work, "rev-parse", rev))
	}

	t.Run("list is empty before any pull request", func(t *testing.T) {
		var prs []*pullRequest
		alice.getJSON("/repos/alice/demo/pulls", &prs)
		assert.Empty(t, prs)
	})

	t.Run("create rejects invalid fields", func(t *testing.T) {
		tests := []struct {
			name    string
			body    map[string]any
			field   string
			message string
		}{
			{name: "missing base", body: map[string]any{"head": "feature", "base": "nope"}, field: "base", message: `Base branch "nope" does not exist`},
			{name: "missing head", body: map[string]any{"head": "nope", "base": "master"}, field: "head", message: `Head branch "nope" does not exist`},
			{name: "same branch", body: map[string]any{"head": "master", "base": "master"}, field: "head", message: "must be different"},
			{name: "no commits", body: map[string]any{"head": "same", "base": "master"}, field: "head", message: "No commits between master and same"},
			{name: "head user without fork", body: map[string]any{"head": "bob:feature", "base": "master"}, field: "head", message: "bob does not have a fork"},
			{name: "draft", body: map[string]any{"head": "feature", "base": "master", "draft": true}, field: "draft", message: "not supported"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				test.body["title"] = "Invalid"
				resp := alice.do(http.MethodPost, "/repos/alice/demo/pulls", test.body)
				resp.assertValidationFailed(test.field, test.message)
			})
		}
	})

	t.Run("create requires write access to the head repository", func(t *testing.T) {
		resp := bob.do(http.MethodPost, "/repos/alice/demo/pulls", map[string]any{
			"title": "From bob",
			"head":  "feature",
			"base":  "master",
		})
		assert.Equal(t, http.StatusForbidden, resp.status, resp.body)
	})

	var first pullRequest
	t.Run("create", func(t *testing.T) {
		resp := alice.do(http.MethodPost, "/repos/alice/demo/pulls", map[string]any{
			"title": "Add feature",
			"body":  "Adds feature.txt.",
			"head":  "feature",
			"base":  "master",
		})
		require.Equal(t, http.StatusCreated, resp.status, resp.body)
		resp.decode(&first)

		apiURL := srv.url + "api/v1/repos/alice/demo"
		assert.Equal(t, apiURL+"/pulls/1", resp.header.Get("Location"))
		assert.Equal(t, apiURL+"/pulls/1", first.URL)
		assert.Equal(t, apiURL+"/issues/1", first.IssueURL)
		assert.Equal(t, srv.url+"alice/demo/pulls/1", first.HTMLURL)
		assert.Equal(t, int64(1), first.Number)
		assert.Equal(t, "Add feature", first.Title)
		assert.Equal(t, "Adds feature.txt.", first.Body)
		assert.Equal(t, types.IssueStateOpen, first.State)
		assert.Equal(t, "alice", first.User.UserName)
		assert.NotNil(t, first.Assignees)
		assert.False(t, first.Draft)
		assert.False(t, first.Merged)
		assert.Nil(t, first.ClosedAt)
		assert.Nil(t, first.MergedAt)

		require.NotNil(t, first.Head)
		assert.Equal(t, "alice:feature", first.Head.Label)
		assert.Equal(t, "feature", first.Head.Ref)
		assert.Equal(t, revParse("feature"), first.Head.SHA)
		assert.Equal(t, "alice", first.Head.User.UserName)
		assert.Equal(t, "alice/demo", first.Head.Repo.FullName)

		require.NotNil(t, first.Base)
		assert.Equal(t, "alice:master", first.Base.Label)
		assert.Equal(t, "master", first.Base.Ref)
		assert.Equal(t, revParse("origin/master"), first.Base.SHA)
		assert.Equal(t, "alice/demo", first.Base.Repo.FullName)

		require.NotNil(t, first.Commits)
		assert.Equal(t, int64(1), *first.Commits)
		assert.Equal(t, 1, *first.Additions)
		assert.Equal(t, 0, *first.Deletions)
		assert.Equal(t, 1, *first.ChangedFiles)
	})

	t.Run("create rejects a duplicate", func(t *testing.T) {
		resp := alice.do(http.MethodPost, "/repos/alice/demo/pulls", map[string]any{
			"title": "Add feature again",
			"head":  "alice:feature",
			"base":  "master",
		})
		resp.assertValidationFailed("head", "A pull request already exists for alice:feature (#1).")
	})

	t.Run("get", func(t *testing.T) {
		var pr pullRequest
		bob.getJSON("/repos/alice/demo/pulls/1", &pr)
		assert.Equal(t, first.ID, pr.ID)
		assert.Equal(t, "Add feature", pr.Title)
		assert.NotNil(t, pr.Commits)

		assert.Equal(t, http.StatusNotFound, bob.do(http.MethodGet, "/repos/alice/demo/pulls/99", nil).status)
	})

	t.Run("get does not return plain issues", func(t *testing.T) {
		resp := alice.do(http.MethodPost, "/repos/alice/demo/issues", map[string]any{"title": "Just an issue"})
		require.Equal(t, http.StatusCreated, resp.status, resp.body)
		var issue types.Issue
		resp.decode(&issue)

		assert.Equal(t, http.StatusNotFound, alice.do(http.MethodGet, fmt.Sprintf("/repos/alice/demo/pulls/%d", issue.Index), nil).status)
	})

	// Make the creation times of the two pull requests differ for sorting.
	time.Sleep(1100 * time.Millisecond)
	var second pullRequest
	resp = alice.do(http.MethodPost, "/repos/alice/demo/pulls", map[string]any{
		"title": "Add second",
		"head":  "second",
		"base":  "master",
	})
	require.Equal(t, http.StatusCreated, resp.status, resp.body)
	resp.decode(&second)

	t.Run("list", func(t *testing.T) {
		numbers := func(path string) []int64 {
			var prs []*pullRequest
			bob.getJSON(path, &prs)
			numbers := make([]int64, len(prs))
			for i, pr := range prs {
				numbers[i] = pr.Number
				assert.Nil(t, pr.Commits, "counts are only returned for a single pull request")
			}
			return numbers
		}

		assert.Equal(t, []int64{second.Number, 1}, numbers("/repos/alice/demo/pulls"))
		assert.Equal(t, []int64{1, second.Number}, numbers("/repos/alice/demo/pulls?direction=asc"))
		assert.Equal(t, []int64{1, second.Number}, numbers("/repos/alice/demo/pulls?sort=long-running"))
		assert.Empty(t, numbers("/repos/alice/demo/pulls?state=closed"))
		assert.Equal(t, []int64{second.Number}, numbers("/repos/alice/demo/pulls?head=alice:second"))
		assert.Len(t, numbers("/repos/alice/demo/pulls?head=alice"), 2)
		assert.Empty(t, numbers("/repos/alice/demo/pulls?head=bob:second"))
		assert.Empty(t, numbers("/repos/alice/demo/pulls?head=nobody:second"))
		assert.Len(t, numbers("/repos/alice/demo/pulls?base=master"), 2)
		assert.Empty(t, numbers("/repos/alice/demo/pulls?base=same"))

		resp := bob.do(http.MethodGet, "/repos/alice/demo/pulls?state=open&per_page=1", nil)
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		var page []*pullRequest
		resp.decode(&page)
		assert.Len(t, page, 1)
		link := resp.header.Get("Link")
		assert.Contains(t, link, "page=2")
		assert.Contains(t, link, "per_page=1")
		assert.Contains(t, link, "state=open")
		assert.Contains(t, link, `rel="next"`)

		bob.do(http.MethodGet, "/repos/alice/demo/pulls?state=merged", nil).assertValidationFailed("state", "")
		bob.do(http.MethodGet, "/repos/alice/demo/pulls?sort=size", nil).assertValidationFailed("sort", "")
		bob.do(http.MethodGet, "/repos/alice/demo/pulls?direction=up", nil).assertValidationFailed("direction", "")
	})

	t.Run("edit", func(t *testing.T) {
		resp := alice.do(http.MethodPatch, "/repos/alice/demo/pulls/1", map[string]any{
			"title": "Add feature.txt",
			"body":  "Edited body.",
		})
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		var pr pullRequest
		resp.decode(&pr)
		assert.Equal(t, "Add feature.txt", pr.Title)
		assert.Equal(t, "Edited body.", pr.Body)
		assert.Equal(t, types.IssueStateOpen, pr.State)
	})

	t.Run("edit requires being the author or a writer", func(t *testing.T) {
		resp := bob.do(http.MethodPatch, "/repos/alice/demo/pulls/1", map[string]any{"title": "Hijacked"})
		assert.Equal(t, http.StatusForbidden, resp.status, resp.body)
	})

	t.Run("edit rejects invalid fields without saving anything", func(t *testing.T) {
		alice.do(http.MethodPatch, "/repos/alice/demo/pulls/1", map[string]any{
			"title": "Should not be saved",
			"state": "merged",
		}).assertValidationFailed("state", "")
		alice.do(http.MethodPatch, "/repos/alice/demo/pulls/1", map[string]any{"title": " "}).
			assertValidationFailed("title", "blank")
		alice.do(http.MethodPatch, "/repos/alice/demo/pulls/1", map[string]any{
			"title": "Should not be saved",
			"base":  "nope",
		}).assertValidationFailed("base", `Base branch "nope" does not exist`)
		alice.do(http.MethodPatch, "/repos/alice/demo/pulls/1", map[string]any{"base": "feature"}).
			assertValidationFailed("base", "must be different")

		var pr pullRequest
		alice.getJSON("/repos/alice/demo/pulls/1", &pr)
		assert.Equal(t, "Add feature.txt", pr.Title)
		assert.Equal(t, "master", pr.Base.Ref)
	})

	t.Run("edit changes the base branch", func(t *testing.T) {
		path := fmt.Sprintf("/repos/alice/demo/pulls/%d", second.Number)
		resp := alice.do(http.MethodPatch, path, map[string]any{"base": "same"})
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		var pr pullRequest
		resp.decode(&pr)
		assert.Equal(t, "same", pr.Base.Ref)
		assert.Equal(t, "alice:same", pr.Base.Label)
		assert.Equal(t, revParse("same"), pr.Base.SHA)

		var onSame []*pullRequest
		alice.getJSON("/repos/alice/demo/pulls?base=same", &onSame)
		require.Len(t, onSame, 1)
		assert.Equal(t, second.Number, onSame[0].Number)

		resp = alice.do(http.MethodPatch, path, map[string]any{"base": "master"})
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		resp.decode(&pr)
		assert.Equal(t, "master", pr.Base.Ref)
	})

	t.Run("diff and patch media types", func(t *testing.T) {
		resp := bob.doWithAccept(http.MethodGet, "/repos/alice/demo/pulls/1", "application/vnd.github.diff")
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		assert.Equal(t, "application/vnd.github.diff; charset=utf-8", resp.header.Get("Content-Type"))
		assert.True(t, strings.HasPrefix(resp.body, "diff --git a/feature.txt b/feature.txt\n"), resp.body)
		assert.Contains(t, resp.body, "+hello from feature")
		assert.NotContains(t, resp.body, "README.md")

		resp = bob.doWithAccept(http.MethodGet, "/repos/alice/demo/pulls/1", "application/vnd.github.v3.patch")
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		assert.Equal(t, "application/vnd.github.patch; charset=utf-8", resp.header.Get("Content-Type"))
		assert.Contains(t, resp.body, "Subject: [PATCH] Update feature.txt on feature")
		assert.Contains(t, resp.body, "+hello from feature")

		resp = bob.doWithAccept(http.MethodGet, "/repos/alice/demo/pulls/1", "application/vnd.github+json")
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		var pr pullRequest
		resp.decode(&pr)
		assert.Equal(t, int64(1), pr.Number)
	})

	t.Run("merge status before merging", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, bob.do(http.MethodGet, "/repos/alice/demo/pulls/1/merge", nil).status)
	})

	alice.waitForMergeable(t, 1, true)

	t.Run("merge requires write access", func(t *testing.T) {
		resp := bob.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{})
		assert.Equal(t, http.StatusForbidden, resp.status, resp.body)
	})

	t.Run("merge rejects unsupported merge methods", func(t *testing.T) {
		resp := alice.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{"merge_method": "squash"})
		assert.Equal(t, http.StatusMethodNotAllowed, resp.status, resp.body)
		assert.Contains(t, resp.body, "Squash merges are not allowed on this repository.")

		resp = alice.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{"merge_method": "rebase"})
		assert.Equal(t, http.StatusMethodNotAllowed, resp.status, resp.body)
		assert.Contains(t, resp.body, "Rebase merges are not allowed on this repository.")

		alice.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{"merge_method": "octopus"}).
			assertValidationFailed("merge_method", "")
	})

	t.Run("merge rejects a stale head commit", func(t *testing.T) {
		resp := alice.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{"sha": strings.Repeat("0", 40)})
		assert.Equal(t, http.StatusConflict, resp.status, resp.body)
		assert.Contains(t, resp.body, "Head branch was modified. Review and try the merge again.")
	})

	t.Run("merge", func(t *testing.T) {
		resp := alice.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{
			"sha":            revParse("feature"),
			"merge_method":   "merge",
			"commit_title":   "Ship feature.txt",
			"commit_message": "Merged through the API.",
		})
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		var result struct {
			SHA     string `json:"sha"`
			Merged  bool   `json:"merged"`
			Message string `json:"message"`
		}
		resp.decode(&result)
		assert.True(t, result.Merged)
		assert.Equal(t, "Pull Request successfully merged", result.Message)

		srv.git(t, work, "fetch", "-q", "origin")
		assert.Equal(t, revParse("origin/master"), result.SHA)
		assert.Equal(t, "Ship feature.txt\n\nMerged through the API.", strings.TrimSpace(srv.git(t, work, "log", "-1", "--format=%B", "origin/master")))
		assert.Equal(t, "hello from feature\n", srv.git(t, work, "show", "origin/master:feature.txt"))

		var pr pullRequest
		alice.getJSON("/repos/alice/demo/pulls/1", &pr)
		assert.True(t, pr.Merged)
		assert.Equal(t, types.IssueStateClosed, pr.State)
		require.NotNil(t, pr.MergedBy)
		assert.Equal(t, "alice", pr.MergedBy.UserName)
		assert.NotNil(t, pr.MergedAt)
		assert.Equal(t, pr.MergedAt, pr.ClosedAt)
		assert.Nil(t, pr.Mergeable)
		assert.Equal(t, "unknown", pr.MergeableState)
	})

	t.Run("merge status after merging", func(t *testing.T) {
		assert.Equal(t, http.StatusNoContent, bob.do(http.MethodGet, "/repos/alice/demo/pulls/1/merge", nil).status)
	})

	t.Run("merge rejects an already merged pull request", func(t *testing.T) {
		resp := alice.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{})
		assert.Equal(t, http.StatusMethodNotAllowed, resp.status, resp.body)
		assert.Contains(t, resp.body, "Pull Request is not mergeable")
	})

	t.Run("edit cannot reopen a merged pull request", func(t *testing.T) {
		alice.do(http.MethodPatch, "/repos/alice/demo/pulls/1", map[string]any{"state": "open"}).
			assertValidationFailed("state", "cannot be reopened")
	})

	t.Run("list by state after merging", func(t *testing.T) {
		var closed, all []*pullRequest
		alice.getJSON("/repos/alice/demo/pulls?state=closed", &closed)
		alice.getJSON("/repos/alice/demo/pulls?state=all", &all)
		require.Len(t, closed, 1)
		assert.True(t, closed[0].Merged)
		assert.Len(t, all, 2)
	})

	t.Run("close and reopen", func(t *testing.T) {
		path := fmt.Sprintf("/repos/alice/demo/pulls/%d", second.Number)
		resp := alice.do(http.MethodPatch, path, map[string]any{"state": "closed"})
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		var pr pullRequest
		resp.decode(&pr)
		assert.Equal(t, types.IssueStateClosed, pr.State)
		assert.False(t, pr.Merged)
		assert.NotNil(t, pr.ClosedAt)

		resp = alice.do(http.MethodPut, path+"/merge", map[string]any{})
		assert.Equal(t, http.StatusMethodNotAllowed, resp.status, resp.body)

		alice.do(http.MethodPatch, path, map[string]any{"base": "same"}).
			assertValidationFailed("base", "closed pull request")

		resp = alice.do(http.MethodPatch, path, map[string]any{"state": "open"})
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		resp.decode(&pr)
		assert.Equal(t, types.IssueStateOpen, pr.State)
		assert.Nil(t, pr.ClosedAt)
	})

	t.Run("merge rejects a conflicting pull request", func(t *testing.T) {
		resp := alice.do(http.MethodPost, "/repos/alice/demo/pulls", map[string]any{
			"title": "Change README",
			"head":  "conflict",
			"base":  "master",
		})
		require.Equal(t, http.StatusCreated, resp.status, resp.body)
		var pr pullRequest
		resp.decode(&pr)

		alice.waitForMergeable(t, pr.Number, false)
		alice.getJSON(fmt.Sprintf("/repos/alice/demo/pulls/%d", pr.Number), &pr)
		assert.Equal(t, "dirty", pr.MergeableState)

		resp = alice.do(http.MethodPut, fmt.Sprintf("/repos/alice/demo/pulls/%d/merge", pr.Number), map[string]any{})
		assert.Equal(t, http.StatusMethodNotAllowed, resp.status, resp.body)
		assert.Contains(t, resp.body, "conflicts")
	})
}

type testServer struct {
	dir        string
	bin        string
	config     string
	url        string
	env        []string
	adminToken string
}

func startTestServer(t *testing.T) *testServer {
	t.Helper()

	dir := t.TempDir()
	srv := &testServer{
		dir:    dir,
		bin:    filepath.Join(dir, "gogs"),
		config: filepath.Join(dir, "app.ini"),
		// Isolate git and Gogs from the global git configuration of whoever runs
		// the test, Gogs writes to it on startup.
		env: append(os.Environ(),
			"HOME="+dir,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
		),
	}

	build := exec.Command("go", "build", "-o", srv.bin, "./cmd/gogs")
	build.Dir = filepath.Join("..", "..", "..", "..")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "build gogs: %s", output)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	srv.url = fmt.Sprintf("http://127.0.0.1:%d/", port)

	currentUser, err := user.Current()
	require.NoError(t, err)
	config := fmt.Sprintf(`
RUN_USER = %[1]s
RUN_MODE = prod

[server]
DOMAIN = 127.0.0.1
HTTP_ADDR = 127.0.0.1
HTTP_PORT = %[2]d
EXTERNAL_URL = %[3]s
APP_DATA_PATH = %[4]s/data
DISABLE_SSH = true
OFFLINE_MODE = true

[repository]
ROOT = %[4]s/repositories

[database]
TYPE = sqlite3
PATH = %[4]s/data/gogs.db

[security]
SECRET_KEY = pull-request-api-test

[log]
MODE = file
LEVEL = Trace
ROOT_PATH = %[4]s/log
`, currentUser.Username, port, srv.url, dir)
	require.NoError(t, os.WriteFile(srv.config, []byte(config), 0o600))

	var serverOutput bytes.Buffer
	web := exec.Command(srv.bin, "web", "--config", srv.config)
	web.Dir = dir
	web.Env = srv.env
	web.Stdout = &serverOutput
	web.Stderr = &serverOutput
	require.NoError(t, web.Start())
	t.Cleanup(func() {
		_ = web.Process.Kill()
		_ = web.Wait()
		if t.Failed() {
			t.Logf("gogs output:\n%s", serverOutput.String())
			logs, _ := filepath.Glob(filepath.Join(dir, "log", "*.log"))
			for _, path := range logs {
				content, _ := os.ReadFile(path)
				t.Logf("%s:\n%s", filepath.Base(path), content)
			}
		}
	})

	require.Eventually(t, func() bool {
		resp, err := http.Get(srv.url + "healthcheck")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, time.Minute, 200*time.Millisecond, "gogs did not start:\n%s", &serverOutput)

	srv.run(t, dir, srv.bin, "admin", "create-user",
		"--name", "alice",
		"--password", "alice-password",
		"--email", "alice@example.com",
		"--admin",
		"--config", srv.config,
	)
	srv.adminToken = srv.createToken(t, "alice", "alice-password")
	return srv
}

func (srv *testServer) createToken(t *testing.T, username, password string) string {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, srv.url+"api/v1/users/"+username+"/tokens", strings.NewReader(`{"name":"test"}`))
	require.NoError(t, err)
	req.SetBasicAuth(username, password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))

	var token struct {
		Sha1 string `json:"sha1"`
	}
	require.NoError(t, json.Unmarshal(body, &token))
	return token.Sha1
}

func (srv *testServer) run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = srv.env
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s %s: %s", name, strings.Join(args, " "), output)
	return string(output)
}

func (srv *testServer) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return srv.run(t, dir, "git", args...)
}

func (srv *testServer) clone(t *testing.T, username, password, repo string) string {
	t.Helper()

	work := filepath.Join(srv.dir, "work")
	remote := strings.Replace(srv.url, "http://", "http://"+username+":"+password+"@", 1) + repo + ".git"
	srv.git(t, srv.dir, "clone", "-q", remote, work)
	srv.git(t, work, "config", "user.name", username)
	srv.git(t, work, "config", "user.email", username+"@example.com")
	return work
}

// commitFile commits a file to the branch, creating the branch from master if
// needed, and pushes it.
func (srv *testServer) commitFile(t *testing.T, work, branch, name, content string) {
	t.Helper()

	srv.git(t, work, "checkout", "-q", "-B", branch, "origin/master")
	require.NoError(t, os.WriteFile(filepath.Join(work, name), []byte(content), 0o600))
	srv.git(t, work, "add", name)
	srv.git(t, work, "commit", "-q", "-m", "Update "+name+" on "+branch)
	srv.git(t, work, "push", "-q", "origin", branch)
	if branch == "master" {
		srv.git(t, work, "fetch", "-q", "origin")
	}
}

type apiClient struct {
	t       *testing.T
	baseURL string
	token   string
}

func (srv *testServer) client(t *testing.T, username, token string) *apiClient {
	require.NotEmpty(t, token, "token for %s", username)
	return &apiClient{t: t, baseURL: srv.url + "api/v1", token: token}
}

type apiResponse struct {
	t      *testing.T
	status int
	header http.Header
	body   string
}

func (r *apiResponse) decode(v any) {
	r.t.Helper()
	require.NoError(r.t, json.Unmarshal([]byte(r.body), v), r.body)
}

// assertValidationFailed asserts a GitHub-style 422 response for the field,
// whose message contains the given text.
func (r *apiResponse) assertValidationFailed(field, message string) {
	r.t.Helper()

	require.Equal(r.t, http.StatusUnprocessableEntity, r.status, r.body)
	var body struct {
		Message string `json:"message"`
		Errors  []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	r.decode(&body)
	assert.Equal(r.t, "Validation Failed", body.Message)
	require.Len(r.t, body.Errors, 1, r.body)
	assert.Equal(r.t, field, body.Errors[0].Field)
	assert.Contains(r.t, body.Errors[0].Message, message)
}

func (c *apiClient) do(method, path string, body any) *apiResponse {
	c.t.Helper()
	return c.request(method, path, body, "")
}

func (c *apiClient) doWithAccept(method, path, accept string) *apiResponse {
	c.t.Helper()
	return c.request(method, path, nil, accept)
}

func (c *apiClient) request(method, path string, body any, accept string) *apiResponse {
	c.t.Helper()

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		require.NoError(c.t, err)
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader)
	require.NoError(c.t, err)
	req.Header.Set("Authorization", "token "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return &apiResponse{t: c.t, status: resp.StatusCode, header: resp.Header, body: string(respBody)}
}

func (c *apiClient) getJSON(path string, v any) {
	c.t.Helper()

	resp := c.do(http.MethodGet, path, nil)
	require.Equal(c.t, http.StatusOK, resp.status, resp.body)
	resp.decode(v)
}

// waitForMergeable waits for the background conflict check that runs after
// every push to settle on the wanted result.
func (c *apiClient) waitForMergeable(t *testing.T, index int64, want bool) {
	t.Helper()

	require.Eventually(t, func() bool {
		resp := c.do(http.MethodGet, fmt.Sprintf("/repos/alice/demo/pulls/%d", index), nil)
		var pr pullRequest
		if resp.status != http.StatusOK || json.Unmarshal([]byte(resp.body), &pr) != nil {
			return false
		}
		return pr.Mergeable != nil && *pr.Mergeable == want
	}, 30*time.Second, 200*time.Millisecond, "pull request #%d never became mergeable=%v", index, want)
}
