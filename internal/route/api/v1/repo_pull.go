package v1

import (
	"bytes"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/gogs/git-module"

	"gogs.io/gogs/internal/context"
	"gogs.io/gogs/internal/database"
	"gogs.io/gogs/internal/gitx"
	"gogs.io/gogs/internal/route/api/v1/types"
)

// The pull request endpoints follow the shape of GitHub's REST API for pull
// requests (https://docs.github.com/en/rest/pulls/pulls), so that clients
// written for GitHub work with few or no changes.

// pullRequestBranch is the "head" or "base" of a pull request.
type pullRequestBranch struct {
	Label string            `json:"label"`
	Ref   string            `json:"ref"`
	SHA   string            `json:"sha"`
	User  *types.User       `json:"user"`
	Repo  *types.Repository `json:"repo"`
}

type pullRequest struct {
	URL            string                `json:"url"`
	ID             int64                 `json:"id"`
	HTMLURL        string                `json:"html_url"`
	IssueURL       string                `json:"issue_url"`
	Number         int64                 `json:"number"`
	State          types.IssueStateType  `json:"state"`
	Title          string                `json:"title"`
	Body           string                `json:"body"`
	User           *types.User           `json:"user"`
	Labels         []*types.IssueLabel   `json:"labels"`
	Milestone      *types.IssueMilestone `json:"milestone"`
	Assignee       *types.User           `json:"assignee"`
	Assignees      []*types.User         `json:"assignees"`
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`
	ClosedAt       *time.Time            `json:"closed_at"`
	MergedAt       *time.Time            `json:"merged_at"`
	Head           *pullRequestBranch    `json:"head"`
	Base           *pullRequestBranch    `json:"base"`
	Draft          bool                  `json:"draft"`
	Merged         bool                  `json:"merged"`
	Mergeable      *bool                 `json:"mergeable"`
	MergeableState string                `json:"mergeable_state"`
	MergedBy       *types.User           `json:"merged_by"`
	Comments       int                   `json:"comments"`
	// The counts below are only returned for a single pull request, like GitHub.
	Commits      *int64 `json:"commits,omitempty"`
	Additions    *int   `json:"additions,omitempty"`
	Deletions    *int   `json:"deletions,omitempty"`
	ChangedFiles *int   `json:"changed_files,omitempty"`
}

// pullRequestError renders an error in the shape GitHub uses.
func pullRequestError(c *context.APIContext, status int, message string) {
	c.JSON(status, map[string]any{
		"message":           message,
		"documentation_url": context.DocURL,
	})
}

// pullRequestValidationFailed renders a 422 response in the shape GitHub uses
// for invalid fields.
func pullRequestValidationFailed(c *context.APIContext, field, code, message string) {
	c.JSON(http.StatusUnprocessableEntity, map[string]any{
		"message": "Validation Failed",
		"errors": []map[string]string{{
			"resource": "PullRequest",
			"field":    field,
			"code":     code,
			"message":  message,
		}},
		"documentation_url": context.DocURL,
	})
}

func mustAllowPulls(c *context.APIContext) {
	if !c.Repo.Repository.AllowsPulls() {
		c.NotFound()
		return
	}
}

// pullRequestHeadRef is the ref in the base repository that always points to the
// latest head commit of a pull request, including one from a fork.
func pullRequestHeadRef(pr *database.PullRequest) string {
	return fmt.Sprintf("refs/pull/%d/head", pr.Index)
}

// getPullRequestByIndex returns the pull request issue with the given index,
// with everything toPullRequest needs loaded. It returns nil when a response has
// been written.
func getPullRequestByIndex(c *context.APIContext, index int64) *database.Issue {
	issue, err := database.GetIssueByIndex(c.Repo.Repository.ID, index)
	if err != nil {
		c.NotFoundOrError(err, "get issue by index")
		return nil
	}
	if !issue.IsPull || issue.PullRequest == nil {
		c.NotFound()
		return nil
	}

	issue.PullRequest.Issue = issue
	if err = issue.PullRequest.LoadAttributes(); err != nil {
		c.Error(err, "load pull request attributes")
		return nil
	}
	return issue
}

// toPullRequest converts a pull request issue to its API form. The issue
// attributes and the BaseRepo, HeadRepo and Merger of its PullRequest must be
// loaded. The commit and line counts are only computed when withCounts is true.
func toPullRequest(c *context.APIContext, baseGitRepo *git.Repository, issue *database.Issue, withCounts bool) (*pullRequest, error) {
	pr := issue.PullRequest
	apiIssue := toIssue(issue)
	repoURL := c.BaseURL + "/repos/" + pr.BaseRepo.FullName()

	apiPR := &pullRequest{
		URL:       fmt.Sprintf("%s/pulls/%d", repoURL, issue.Index),
		ID:        pr.ID,
		HTMLURL:   issue.HTMLURL(),
		IssueURL:  fmt.Sprintf("%s/issues/%d", repoURL, issue.Index),
		Number:    issue.Index,
		State:     apiIssue.State,
		Title:     apiIssue.Title,
		Body:      apiIssue.Body,
		User:      apiIssue.Poster,
		Labels:    apiIssue.Labels,
		Milestone: apiIssue.Milestone,
		Assignee:  apiIssue.Assignee,
		Assignees: []*types.User{},
		CreatedAt: issue.Created,
		UpdatedAt: issue.Updated,
		Merged:    pr.HasMerged,
		Comments:  issue.NumComments,
	}
	if apiIssue.Assignee != nil {
		apiPR.Assignees = append(apiPR.Assignees, apiIssue.Assignee)
	}

	if pr.HasMerged {
		apiPR.MergedAt = &pr.Merged
		apiPR.ClosedAt = &pr.Merged
		apiPR.MergedBy = toUser(pr.Merger)
	} else if issue.IsClosed {
		// Gogs does not record when an issue was closed, and closing it is
		// usually the last update.
		apiPR.ClosedAt = &issue.Updated
	}

	apiPR.MergeableState = "unknown"
	if !issue.IsClosed && !pr.IsChecking() {
		mergeable := pr.CanAutoMerge()
		apiPR.Mergeable = &mergeable
		apiPR.MergeableState = "dirty"
		if mergeable {
			apiPR.MergeableState = "clean"
		}
	}

	// The head ref only goes missing if pushing to the base repository failed,
	// fall back to the head branch then and skip the counts.
	headSHA, headRefErr := baseGitRepo.RevParse(pullRequestHeadRef(pr))
	if headRefErr != nil && pr.HeadRepo != nil {
		if headGitRepo, err := git.Open(pr.HeadRepo.RepoPath()); err == nil {
			headSHA, _ = headGitRepo.BranchCommitID(pr.HeadBranch)
		}
	}
	apiPR.Head = &pullRequestBranch{
		Label: pr.HeadUserName + ":" + pr.HeadBranch,
		Ref:   pr.HeadBranch,
		SHA:   headSHA,
	}
	// The head repository is nil when it has been deleted.
	if pr.HeadRepo != nil {
		apiPR.Head.User = toUser(pr.HeadRepo.MustOwner())
		apiPR.Head.Repo = toRepository(pr.HeadRepo, nil)
	}

	// Fall back to the merge base when the base branch has been deleted.
	baseSHA, err := baseGitRepo.BranchCommitID(pr.BaseBranch)
	if err != nil {
		baseSHA = pr.MergeBase
	}
	apiPR.Base = &pullRequestBranch{
		Label: pr.BaseRepo.MustOwner().Name + ":" + pr.BaseBranch,
		Ref:   pr.BaseBranch,
		SHA:   baseSHA,
		User:  toUser(pr.BaseRepo.MustOwner()),
		Repo:  toRepository(pr.BaseRepo, nil),
	}

	if !withCounts || headRefErr != nil {
		return apiPR, nil
	}

	commits, err := baseGitRepo.RevListCount([]string{pr.MergeBase + ".." + headSHA})
	if err != nil {
		return nil, errors.Wrap(err, "count commits")
	}
	numstat, err := git.NewCommand("diff", "--numstat", "-M", "--end-of-options", pr.MergeBase, headSHA).
		RunInDirWithTimeout(0, baseGitRepo.Path())
	if err != nil {
		return nil, errors.Wrap(err, "get diff stats")
	}
	var additions, deletions, changedFiles int
	for _, line := range strings.Split(strings.TrimSpace(string(numstat)), "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		changedFiles++
		// Binary files have "-" instead of line counts.
		added, _ := strconv.Atoi(fields[0])
		deleted, _ := strconv.Atoi(fields[1])
		additions += added
		deletions += deleted
	}
	apiPR.Commits = &commits
	apiPR.Additions = &additions
	apiPR.Deletions = &deletions
	apiPR.ChangedFiles = &changedFiles
	return apiPR, nil
}

// respondPullRequest renders a single pull request with its counts.
func respondPullRequest(c *context.APIContext, status int, issue *database.Issue) {
	baseGitRepo, err := git.Open(c.Repo.Repository.RepoPath())
	if err != nil {
		c.Error(err, "open base repository")
		return
	}
	apiPR, err := toPullRequest(c, baseGitRepo, issue, true)
	if err != nil {
		c.Error(err, "convert pull request")
		return
	}
	c.JSON(status, apiPR)
}

func listPullRequests(c *context.APIContext) {
	baseRepo := c.Repo.Repository
	opts := database.PullRequestsOptions{
		BaseRepoID: baseRepo.ID,
		State:      c.QueryTrim("state"),
		BaseBranch: c.QueryTrim("base"),
		SortType:   c.QueryTrim("sort"),
		Page:       c.QueryInt("page"),
		PageSize:   c.QueryInt("per_page"),
	}

	switch opts.State {
	case "":
		opts.State = "open"
	case "open", "closed", "all":
	default:
		pullRequestValidationFailed(c, "state", "invalid", `state must be one of "open", "closed" or "all"`)
		return
	}

	switch opts.SortType {
	case "":
		opts.SortType = "created"
	case "created", "updated", "popularity", "long-running":
	default:
		pullRequestValidationFailed(c, "sort", "invalid", `sort must be one of "created", "updated", "popularity" or "long-running"`)
		return
	}
	switch c.QueryTrim("direction") {
	case "":
		opts.Ascending = opts.SortType != "created"
	case "asc":
		opts.Ascending = true
	case "desc":
	default:
		pullRequestValidationFailed(c, "direction", "invalid", `direction must be "asc" or "desc"`)
		return
	}

	if opts.PageSize <= 0 {
		opts.PageSize = 30
	} else if opts.PageSize > 100 {
		opts.PageSize = 100
	}

	if head := c.QueryTrim("head"); head != "" {
		username, branch, _ := strings.Cut(head, ":")
		headRepo, err := findHeadRepository(c, username)
		if err != nil {
			c.Error(err, "find head repository")
			return
		}
		// No pull request can match a head that does not exist.
		if headRepo == nil {
			c.JSONSuccess([]*pullRequest{})
			return
		}
		opts.HeadRepoID = headRepo.ID
		opts.HeadBranch = branch
	}

	prs, count, err := database.PullRequests(opts)
	if err != nil {
		c.Error(err, "list pull requests")
		return
	}

	baseGitRepo, err := git.Open(baseRepo.RepoPath())
	if err != nil {
		c.Error(err, "open base repository")
		return
	}
	apiPRs := make([]*pullRequest, 0, len(prs))
	for _, pr := range prs {
		issue := pr.Issue
		issue.PullRequest = pr
		if err = issue.LoadAttributes(); err != nil {
			c.Error(err, "load issue attributes")
			return
		}
		apiPR, err := toPullRequest(c, baseGitRepo, issue, false)
		if err != nil {
			c.Error(err, "convert pull request")
			return
		}
		apiPRs = append(apiPRs, apiPR)
	}

	c.SetLinkHeader(int(count), opts.PageSize)
	c.JSONSuccess(&apiPRs)
}

// findHeadRepository returns the repository that the user's branches would come
// from for a pull request to the context repository: the repository itself for
// its owner, or the user's fork of it. It returns nil if there is none.
func findHeadRepository(c *context.APIContext, username string) (*database.Repository, error) {
	baseRepo := c.Repo.Repository
	headUser, err := database.Handle.Users().GetByUsername(c.Req.Context(), username)
	if err != nil {
		if database.IsErrUserNotExist(err) {
			return nil, nil
		}
		return nil, errors.Wrap(err, "get user by name")
	}
	if headUser.ID == baseRepo.OwnerID {
		return baseRepo, nil
	}

	headRepo, hasFork, err := database.HasForkedRepo(headUser.ID, baseRepo.ID)
	if err != nil {
		return nil, errors.Wrap(err, "get forked repository")
	} else if !hasFork {
		return nil, nil
	}
	headRepo.Owner = headUser
	return headRepo, nil
}

func getPullRequest(c *context.APIContext) {
	issue := getPullRequestByIndex(c, c.ParamsInt64(":index"))
	if c.Written() {
		return
	}

	switch pullRequestMediaFormat(c.Req.Header.Get("Accept")) {
	case "diff":
		renderPullRequestDiff(c, issue.PullRequest, "diff")
	case "patch":
		renderPullRequestDiff(c, issue.PullRequest, "patch")
	default:
		respondPullRequest(c, http.StatusOK, issue)
	}
}

// pullRequestMediaFormat returns "diff" or "patch" when the Accept header asks
// for GitHub's diff or patch media type of a pull request, e.g.,
// "application/vnd.github.diff" or "application/vnd.github.v3.patch".
func pullRequestMediaFormat(accept string) string {
	for _, part := range strings.Split(accept, ",") {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || !strings.HasPrefix(mediaType, "application/vnd.github") {
			continue
		}
		switch {
		case strings.HasSuffix(mediaType, ".diff"):
			return "diff"
		case strings.HasSuffix(mediaType, ".patch"):
			return "patch"
		}
	}
	return ""
}

func renderPullRequestDiff(c *context.APIContext, pr *database.PullRequest, format string) {
	var cmd *git.Command
	headRef := pullRequestHeadRef(pr)
	if format == "patch" {
		cmd = git.NewCommand("format-patch", "--stdout", "--no-signature", "--full-index", "--end-of-options", pr.MergeBase+".."+headRef)
	} else {
		cmd = git.NewCommand("diff", "-M", "--full-index", "--end-of-options", pr.MergeBase, headRef)
	}

	var stdout, stderr bytes.Buffer
	if err := cmd.RunInDirPipelineWithTimeout(0, &stdout, &stderr, c.Repo.Repository.RepoPath()); err != nil {
		c.Errorf(err, "get pull request %s: %s", format, stderr.String())
		return
	}

	c.Header().Set("Content-Type", "application/vnd.github."+format+"; charset=utf-8")
	c.Resp.WriteHeader(http.StatusOK)
	_, _ = c.Resp.Write(stdout.Bytes())
}

type createPullRequestRequest struct {
	Title string `json:"title" binding:"Required"`
	// Head is the branch to merge from, either "<branch>" in the base repository
	// or "<username>:<branch>" in that user's fork of the base repository.
	Head  string `json:"head" binding:"Required"`
	Base  string `json:"base" binding:"Required"`
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
}

func createPullRequest(c *context.APIContext, form createPullRequestRequest) {
	if form.Draft {
		pullRequestValidationFailed(c, "draft", "invalid", "Draft pull requests are not supported")
		return
	}

	baseRepo := c.Repo.Repository
	baseGitRepo, err := git.Open(baseRepo.RepoPath())
	if err != nil {
		c.Error(err, "open base repository")
		return
	}
	if !baseGitRepo.HasBranch(form.Base) {
		pullRequestValidationFailed(c, "base", "invalid", fmt.Sprintf("Base branch %q does not exist", form.Base))
		return
	}

	headRepo := baseRepo
	headUsername := baseRepo.MustOwner().Name
	headBranch := form.Head
	if username, branch, ok := strings.Cut(form.Head, ":"); ok {
		headBranch = branch
		headRepo, err = findHeadRepository(c, username)
		if err != nil {
			c.Error(err, "find head repository")
			return
		} else if headRepo == nil {
			pullRequestValidationFailed(c, "head", "invalid", fmt.Sprintf("%s does not have a fork of this repository", username))
			return
		}
		headUsername = headRepo.MustOwner().Name
	}

	if !c.User.IsAdmin &&
		!database.Handle.Permissions().Authorize(
			c.Req.Context(),
			c.User.ID,
			headRepo.ID,
			database.AccessModeWrite,
			database.AccessModeOptions{
				OwnerID: headRepo.OwnerID,
				Private: headRepo.IsPrivate,
			},
		) {
		c.Status(http.StatusForbidden)
		return
	}

	headGitRepo := baseGitRepo
	if headRepo.ID != baseRepo.ID {
		headGitRepo, err = git.Open(headRepo.RepoPath())
		if err != nil {
			c.Error(err, "open head repository")
			return
		}
	}
	if !headGitRepo.HasBranch(headBranch) {
		pullRequestValidationFailed(c, "head", "invalid", fmt.Sprintf("Head branch %q does not exist", form.Head))
		return
	}
	if headRepo.ID == baseRepo.ID && headBranch == form.Base {
		pullRequestValidationFailed(c, "head", "invalid", "Head and base must be different branches")
		return
	}

	existing, err := database.GetUnmergedPullRequest(headRepo.ID, baseRepo.ID, headBranch, form.Base)
	if err == nil {
		pullRequestValidationFailed(c, "head", "custom", fmt.Sprintf("A pull request already exists for %s:%s (#%d).", headUsername, headBranch, existing.Index))
		return
	} else if !database.IsErrPullRequestNotExist(err) {
		c.Error(err, "get unmerged pull request")
		return
	}

	meta, err := gitx.Module.PullRequestMeta(headGitRepo.Path(), baseGitRepo.Path(), headBranch, form.Base)
	if err != nil {
		if gitx.IsErrNoMergeBase(err) {
			pullRequestValidationFailed(c, "head", "invalid", fmt.Sprintf("%s and %s have no common history", form.Base, form.Head))
		} else {
			c.Error(err, "get pull request meta")
		}
		return
	}
	if len(meta.Commits) == 0 {
		pullRequestValidationFailed(c, "head", "custom", fmt.Sprintf("No commits between %s and %s", form.Base, form.Head))
		return
	}

	patch, err := headGitRepo.DiffBinary(meta.MergeBase, headBranch)
	if err != nil {
		c.Error(err, "get patch")
		return
	}

	pullIssue := &database.Issue{
		RepoID:   baseRepo.ID,
		Index:    baseRepo.NextIssueIndex(),
		Title:    form.Title,
		PosterID: c.User.ID,
		Poster:   c.User,
		IsPull:   true,
		Content:  form.Body,
	}
	pullRequest := &database.PullRequest{
		HeadRepoID:   headRepo.ID,
		BaseRepoID:   baseRepo.ID,
		HeadUserName: headUsername,
		HeadBranch:   headBranch,
		BaseBranch:   form.Base,
		HeadRepo:     headRepo,
		BaseRepo:     baseRepo,
		MergeBase:    meta.MergeBase,
		Type:         database.PullRequestTypeGogs,
	}
	if err = database.NewPullRequest(baseRepo, pullIssue, nil, nil, pullRequest, patch); err != nil {
		c.Error(err, "new pull request")
		return
	} else if err = pullRequest.PushToBaseRepo(); err != nil {
		c.Error(err, "push to base repository")
		return
	}

	issue := getPullRequestByIndex(c, pullIssue.Index)
	if c.Written() {
		return
	}
	c.Header().Set("Location", fmt.Sprintf("%s/repos/%s/pulls/%d", c.BaseURL, baseRepo.FullName(), issue.Index))
	respondPullRequest(c, http.StatusCreated, issue)
}

type editPullRequestRequest struct {
	Title *string `json:"title"`
	Body  *string `json:"body"`
	State *string `json:"state"`
	Base  *string `json:"base"`
}

func editPullRequest(c *context.APIContext, form editPullRequestRequest) {
	issue := getPullRequestByIndex(c, c.ParamsInt64(":index"))
	if c.Written() {
		return
	}
	pr := issue.PullRequest

	if !issue.IsPoster(c.User.ID) && !c.Repo.IsWriter() {
		c.Status(http.StatusForbidden)
		return
	}

	// Validate every field before saving anything, so a rejected request does not
	// leave a partial edit behind.
	if form.Title != nil && strings.TrimSpace(*form.Title) == "" {
		pullRequestValidationFailed(c, "title", "missing_field", "Title cannot be blank")
		return
	}

	var reopen, closing bool
	if form.State != nil {
		switch types.IssueStateType(*form.State) {
		case types.IssueStateOpen:
			reopen = issue.IsClosed
		case types.IssueStateClosed:
			closing = !issue.IsClosed
		default:
			pullRequestValidationFailed(c, "state", "invalid", `state must be "open" or "closed"`)
			return
		}
	}

	changeBase := form.Base != nil && *form.Base != pr.BaseBranch
	newBase := pr.BaseBranch
	if changeBase {
		newBase = *form.Base
	}

	if reopen || changeBase {
		switch {
		case pr.HasMerged && reopen:
			pullRequestValidationFailed(c, "state", "invalid", "A merged pull request cannot be reopened")
			return
		case pr.HasMerged:
			pullRequestValidationFailed(c, "base", "invalid", "Cannot change the base branch of a merged pull request")
			return
		case changeBase && issue.IsClosed && !reopen:
			pullRequestValidationFailed(c, "base", "invalid", "Cannot change the base branch of a closed pull request")
			return
		case pr.HeadRepo == nil:
			pullRequestValidationFailed(c, "head", "invalid", "The head repository has been deleted")
			return
		}

		existing, err := database.GetUnmergedPullRequest(pr.HeadRepoID, pr.BaseRepoID, pr.HeadBranch, newBase)
		if err == nil && existing.ID != pr.ID {
			pullRequestValidationFailed(c, "base", "custom", fmt.Sprintf("A pull request already exists for %s:%s (#%d).", pr.HeadUserName, pr.HeadBranch, existing.Index))
			return
		} else if err != nil && !database.IsErrPullRequestNotExist(err) {
			c.Error(err, "get unmerged pull request")
			return
		}
	}

	if changeBase {
		baseGitRepo, err := git.Open(c.Repo.Repository.RepoPath())
		if err != nil {
			c.Error(err, "open base repository")
			return
		}
		if !baseGitRepo.HasBranch(newBase) {
			pullRequestValidationFailed(c, "base", "invalid", fmt.Sprintf("Base branch %q does not exist", newBase))
			return
		}
		if pr.HeadRepoID == pr.BaseRepoID && pr.HeadBranch == newBase {
			pullRequestValidationFailed(c, "base", "invalid", "Head and base must be different branches")
			return
		}

		meta, err := gitx.Module.PullRequestMeta(pr.HeadRepo.RepoPath(), baseGitRepo.Path(), pr.HeadBranch, newBase)
		if err != nil {
			if gitx.IsErrNoMergeBase(err) {
				pullRequestValidationFailed(c, "base", "invalid", fmt.Sprintf("%s and %s have no common history", newBase, pr.HeadBranch))
			} else {
				c.Error(err, "get pull request meta")
			}
			return
		}
		if len(meta.Commits) == 0 {
			pullRequestValidationFailed(c, "base", "custom", fmt.Sprintf("There are no new commits between base branch '%s' and head branch '%s'", newBase, pr.HeadBranch))
			return
		}
	}

	if form.Title != nil || form.Body != nil {
		if form.Title != nil {
			issue.Title = *form.Title
		}
		if form.Body != nil {
			issue.Content = *form.Body
		}
		if err := database.UpdateIssue(issue); err != nil {
			c.Error(err, "update issue")
			return
		}
	}

	if changeBase || reopen {
		pr.BaseBranch = newBase
		// Regenerating the patch also saves the new base branch and merge base.
		if err := pr.UpdatePatch(); err != nil {
			c.Error(err, "update patch")
			return
		}
		pr.AddToTaskQueue()
	}
	if reopen || closing {
		if err := issue.ChangeStatus(c.User, c.Repo.Repository, closing); err != nil {
			c.Error(err, "change status")
			return
		}
	}

	issue = getPullRequestByIndex(c, issue.Index)
	if c.Written() {
		return
	}
	respondPullRequest(c, http.StatusOK, issue)
}

func isPullRequestMerged(c *context.APIContext) {
	issue := getPullRequestByIndex(c, c.ParamsInt64(":index"))
	if c.Written() {
		return
	}
	if issue.PullRequest.HasMerged {
		c.NoContent()
		return
	}
	c.NotFound()
}

type mergePullRequestRequest struct {
	CommitTitle   string `json:"commit_title"`
	CommitMessage string `json:"commit_message"`
	// SHA, when set, must match the head commit for the merge to happen.
	SHA string `json:"sha"`
	// MergeMethod is "merge" (the default), "squash" or "rebase".
	MergeMethod string `json:"merge_method"`
}

func mergePullRequest(c *context.APIContext, form mergePullRequestRequest) {
	issue := getPullRequestByIndex(c, c.ParamsInt64(":index"))
	if c.Written() {
		return
	}
	pr := issue.PullRequest

	var mergeStyle database.MergeStyle
	switch form.MergeMethod {
	case "", "merge":
		mergeStyle = database.MergeStyleRegular
	case "rebase":
		if !c.Repo.Repository.PullsAllowRebase {
			pullRequestError(c, http.StatusMethodNotAllowed, "Rebase merges are not allowed on this repository.")
			return
		}
		mergeStyle = database.MergeStyleRebase
	case "squash":
		pullRequestError(c, http.StatusMethodNotAllowed, "Squash merges are not allowed on this repository.")
		return
	default:
		pullRequestValidationFailed(c, "merge_method", "invalid", `merge_method must be one of "merge", "squash" or "rebase"`)
		return
	}

	switch {
	case pr.HasMerged:
		pullRequestError(c, http.StatusMethodNotAllowed, "Pull Request is not mergeable: it has already been merged.")
		return
	case issue.IsClosed:
		pullRequestError(c, http.StatusMethodNotAllowed, "Pull Request is not mergeable: it is closed.")
		return
	case pr.HeadRepo == nil:
		pullRequestError(c, http.StatusMethodNotAllowed, "Pull Request is not mergeable: the head repository has been deleted.")
		return
	case pr.IsChecking():
		pullRequestError(c, http.StatusMethodNotAllowed, "Pull Request is not mergeable: it is still being checked for conflicts, try again later.")
		return
	case !pr.CanAutoMerge():
		pullRequestError(c, http.StatusMethodNotAllowed, "Pull Request is not mergeable: it has conflicts.")
		return
	}

	baseGitRepo, err := git.Open(c.Repo.Repository.RepoPath())
	if err != nil {
		c.Error(err, "open base repository")
		return
	}

	if form.SHA != "" {
		headGitRepo, err := git.Open(pr.HeadRepo.RepoPath())
		if err != nil {
			c.Error(err, "open head repository")
			return
		}
		headSHA, err := headGitRepo.BranchCommitID(pr.HeadBranch)
		if err != nil {
			c.Error(err, "get head branch commit ID")
			return
		}
		if headSHA != form.SHA {
			pullRequestError(c, http.StatusConflict, "Head branch was modified. Review and try the merge again.")
			return
		}
	}

	issue.Repo = c.Repo.Repository
	if err = pr.Merge(c.User, baseGitRepo, mergeStyle, form.CommitTitle, form.CommitMessage); err != nil {
		c.Error(err, "merge pull request")
		return
	}

	mergedSHA, err := baseGitRepo.BranchCommitID(pr.BaseBranch)
	if err != nil {
		c.Error(err, "get base branch commit ID")
		return
	}
	c.JSONSuccess(map[string]any{
		"sha":     mergedSHA,
		"merged":  true,
		"message": "Pull Request successfully merged",
	})
}
