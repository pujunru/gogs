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

	t.Run("list is empty before any pull request", func(t *testing.T) {
		var prs []*types.PullRequest
		alice.getJSON("/repos/alice/demo/pulls", &prs)
		assert.Empty(t, prs)
	})

	t.Run("create rejects invalid branches", func(t *testing.T) {
		tests := []struct {
			name    string
			head    string
			base    string
			message string
		}{
			{name: "missing base", head: "feature", base: "nope", message: "base branch does not exist"},
			{name: "missing head", head: "nope", base: "master", message: "head branch does not exist"},
			{name: "same branch", head: "master", base: "master", message: "must be different"},
			{name: "no commits", head: "same", base: "master", message: "no commits between"},
			{name: "head user without fork", head: "bob:feature", base: "master", message: "does not have a fork"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				resp := alice.do(http.MethodPost, "/repos/alice/demo/pulls", map[string]any{
					"title": "Invalid",
					"head":  test.head,
					"base":  test.base,
				})
				assert.Equal(t, http.StatusUnprocessableEntity, resp.status, resp.body)
				assert.Contains(t, resp.body, test.message)
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

	var created types.PullRequest
	t.Run("create", func(t *testing.T) {
		resp := alice.do(http.MethodPost, "/repos/alice/demo/pulls", map[string]any{
			"title": "Add feature",
			"body":  "Adds feature.txt.",
			"head":  "feature",
			"base":  "master",
		})
		require.Equal(t, http.StatusCreated, resp.status, resp.body)
		resp.decode(&created)

		assert.Equal(t, int64(1), created.Index)
		assert.Equal(t, "Add feature", created.Title)
		assert.Equal(t, "Adds feature.txt.", created.Body)
		assert.Equal(t, types.IssueStateOpen, created.State)
		assert.Equal(t, "feature", created.HeadBranch)
		assert.Equal(t, "master", created.BaseBranch)
		assert.Equal(t, "alice/demo", created.HeadRepo.FullName)
		assert.Equal(t, "alice/demo", created.BaseRepo.FullName)
		assert.Equal(t, "alice", created.Poster.UserName)
		assert.Equal(t, srv.url+"alice/demo/pulls/1", created.HTMLURL)
		assert.False(t, created.HasMerged)
	})

	t.Run("create rejects a duplicate", func(t *testing.T) {
		resp := alice.do(http.MethodPost, "/repos/alice/demo/pulls", map[string]any{
			"title": "Add feature again",
			"head":  "alice:feature",
			"base":  "master",
		})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.status, resp.body)
		assert.Contains(t, resp.body, "already exists for these branches: #1")
	})

	t.Run("get", func(t *testing.T) {
		var pr types.PullRequest
		bob.getJSON("/repos/alice/demo/pulls/1", &pr)
		assert.Equal(t, created.ID, pr.ID)
		assert.Equal(t, "Add feature", pr.Title)

		assert.Equal(t, http.StatusNotFound, bob.do(http.MethodGet, "/repos/alice/demo/pulls/99", nil).status)
	})

	t.Run("get does not return plain issues", func(t *testing.T) {
		resp := alice.do(http.MethodPost, "/repos/alice/demo/issues", map[string]any{"title": "Just an issue"})
		require.Equal(t, http.StatusCreated, resp.status, resp.body)
		var issue types.Issue
		resp.decode(&issue)

		assert.Equal(t, http.StatusNotFound, alice.do(http.MethodGet, fmt.Sprintf("/repos/alice/demo/pulls/%d", issue.Index), nil).status)
	})

	t.Run("list filters by state", func(t *testing.T) {
		var open, closed []*types.PullRequest
		bob.getJSON("/repos/alice/demo/pulls", &open)
		bob.getJSON("/repos/alice/demo/pulls?state=closed", &closed)
		require.Len(t, open, 1)
		assert.Equal(t, created.ID, open[0].ID)
		assert.Empty(t, closed)
	})

	t.Run("edit", func(t *testing.T) {
		resp := alice.do(http.MethodPatch, "/repos/alice/demo/pulls/1", map[string]any{
			"title": "Add feature.txt",
			"body":  "Edited body.",
		})
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		var pr types.PullRequest
		resp.decode(&pr)
		assert.Equal(t, "Add feature.txt", pr.Title)
		assert.Equal(t, "Edited body.", pr.Body)
		assert.Equal(t, types.IssueStateOpen, pr.State)
	})

	t.Run("edit requires being the poster or a writer", func(t *testing.T) {
		resp := bob.do(http.MethodPatch, "/repos/alice/demo/pulls/1", map[string]any{"title": "Hijacked"})
		assert.Equal(t, http.StatusForbidden, resp.status, resp.body)
	})

	t.Run("edit rejects an unknown state", func(t *testing.T) {
		resp := alice.do(http.MethodPatch, "/repos/alice/demo/pulls/1", map[string]any{
			"title": "Should not be saved",
			"state": "merged",
		})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.status, resp.body)

		var pr types.PullRequest
		alice.getJSON("/repos/alice/demo/pulls/1", &pr)
		assert.Equal(t, "Add feature.txt", pr.Title)
	})

	t.Run("diff", func(t *testing.T) {
		resp := bob.do(http.MethodGet, "/repos/alice/demo/pulls/1/diff", nil)
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		assert.Equal(t, "text/plain; charset=utf-8", resp.header.Get("Content-Type"))
		assert.Contains(t, resp.body, "diff --git a/feature.txt b/feature.txt")
		assert.Contains(t, resp.body, "+hello from feature")
		assert.NotContains(t, resp.body, "README.md")
	})

	t.Run("merge status before merging", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, bob.do(http.MethodGet, "/repos/alice/demo/pulls/1/merge", nil).status)
	})

	alice.waitForMergeable(t, 1, true)

	t.Run("merge requires write access", func(t *testing.T) {
		resp := bob.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{})
		assert.Equal(t, http.StatusForbidden, resp.status, resp.body)
	})

	t.Run("merge rejects an unknown style", func(t *testing.T) {
		resp := alice.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{"merge_style": "squash"})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.status, resp.body)
	})

	t.Run("merge rejects rebasing when the repository does not allow it", func(t *testing.T) {
		resp := alice.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{"merge_style": "rebase_before_merging"})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.status, resp.body)
	})

	t.Run("merge rejects a stale head commit", func(t *testing.T) {
		resp := alice.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{"sha": strings.Repeat("0", 40)})
		assert.Equal(t, http.StatusConflict, resp.status, resp.body)
	})

	t.Run("merge", func(t *testing.T) {
		headCommitID := strings.TrimSpace(srv.git(t, work, "rev-parse", "feature"))
		resp := alice.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{
			"sha":                headCommitID,
			"commit_description": "Merged through the API.",
		})
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		var pr types.PullRequest
		resp.decode(&pr)
		assert.True(t, pr.HasMerged)
		assert.Equal(t, types.IssueStateClosed, pr.State)
		require.NotNil(t, pr.MergedBy)
		assert.Equal(t, "alice", pr.MergedBy.UserName)
		require.NotNil(t, pr.MergedCommitID)
		assert.Equal(t, headCommitID, *pr.MergedCommitID)
		assert.NotNil(t, pr.Merged)

		raw := alice.do(http.MethodGet, "/repos/alice/demo/raw/master/feature.txt", nil)
		require.Equal(t, http.StatusOK, raw.status, raw.body)
		assert.Equal(t, "hello from feature\n", raw.body)

		srv.git(t, work, "fetch", "-q", "origin")
		message := srv.git(t, work, "log", "-1", "--format=%B", "origin/master")
		assert.Contains(t, message, "Merge branch 'feature' of alice/demo into master")
		assert.Contains(t, message, "Merged through the API.")
	})

	t.Run("merge status after merging", func(t *testing.T) {
		assert.Equal(t, http.StatusNoContent, bob.do(http.MethodGet, "/repos/alice/demo/pulls/1/merge", nil).status)
	})

	t.Run("merge rejects an already merged pull request", func(t *testing.T) {
		resp := alice.do(http.MethodPut, "/repos/alice/demo/pulls/1/merge", map[string]any{})
		assert.Equal(t, http.StatusMethodNotAllowed, resp.status, resp.body)
	})

	t.Run("edit cannot reopen a merged pull request", func(t *testing.T) {
		resp := alice.do(http.MethodPatch, "/repos/alice/demo/pulls/1", map[string]any{"state": "open"})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.status, resp.body)
	})

	t.Run("list shows merged pull requests as closed", func(t *testing.T) {
		var open, closed []*types.PullRequest
		alice.getJSON("/repos/alice/demo/pulls", &open)
		alice.getJSON("/repos/alice/demo/pulls?state=closed", &closed)
		assert.Empty(t, open)
		require.Len(t, closed, 1)
		assert.True(t, closed[0].HasMerged)
	})

	t.Run("close and reopen", func(t *testing.T) {
		resp := alice.do(http.MethodPost, "/repos/alice/demo/pulls", map[string]any{
			"title": "Add second",
			"head":  "second",
			"base":  "master",
		})
		require.Equal(t, http.StatusCreated, resp.status, resp.body)
		var pr types.PullRequest
		resp.decode(&pr)
		path := fmt.Sprintf("/repos/alice/demo/pulls/%d", pr.Index)

		resp = alice.do(http.MethodPatch, path, map[string]any{"state": "closed"})
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		resp.decode(&pr)
		assert.Equal(t, types.IssueStateClosed, pr.State)
		assert.False(t, pr.HasMerged)

		resp = alice.do(http.MethodPut, path+"/merge", map[string]any{})
		assert.Equal(t, http.StatusMethodNotAllowed, resp.status, resp.body)

		resp = alice.do(http.MethodPatch, path, map[string]any{"state": "open"})
		require.Equal(t, http.StatusOK, resp.status, resp.body)
		resp.decode(&pr)
		assert.Equal(t, types.IssueStateOpen, pr.State)
	})

	t.Run("merge rejects a conflicting pull request", func(t *testing.T) {
		resp := alice.do(http.MethodPost, "/repos/alice/demo/pulls", map[string]any{
			"title": "Change README",
			"head":  "conflict",
			"base":  "master",
		})
		require.Equal(t, http.StatusCreated, resp.status, resp.body)
		var pr types.PullRequest
		resp.decode(&pr)

		alice.waitForMergeable(t, pr.Index, false)
		resp = alice.do(http.MethodPut, fmt.Sprintf("/repos/alice/demo/pulls/%d/merge", pr.Index), map[string]any{})
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

func (c *apiClient) do(method, path string, body any) *apiResponse {
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
		var pr types.PullRequest
		if resp.status != http.StatusOK || json.Unmarshal([]byte(resp.body), &pr) != nil {
			return false
		}
		return pr.Mergeable != nil && *pr.Mergeable == want
	}, 30*time.Second, 200*time.Millisecond, "pull request #%d never became mergeable=%v", index, want)
}
