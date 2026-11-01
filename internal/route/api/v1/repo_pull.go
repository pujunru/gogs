package v1

import (
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/gogs/git-module"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/context"
	"gogs.io/gogs/internal/database"
	"gogs.io/gogs/internal/gitx"
	"gogs.io/gogs/internal/route/api/v1/types"
)

func mustAllowPulls(c *context.APIContext) {
	if !c.Repo.Repository.AllowsPulls() {
		c.NotFound()
		return
	}
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

func listPullRequests(c *context.APIContext) {
	opts := &database.IssuesOptions{
		RepoID:   c.Repo.Repository.ID,
		Page:     c.QueryInt("page"),
		IsClosed: types.IssueStateType(c.Query("state")) == types.IssueStateClosed,
		IsPull:   true,
	}

	issues, err := database.Issues(opts)
	if err != nil {
		c.Error(err, "list pull requests")
		return
	}
	count, err := database.IssuesCount(opts)
	if err != nil {
		c.Error(err, "count pull requests")
		return
	}

	apiPRs := make([]*types.PullRequest, 0, len(issues))
	for _, issue := range issues {
		if err = issue.LoadAttributes(); err != nil {
			c.Error(err, "load issue attributes")
			return
		}
		// Pull request rows are inserted after their issues, so skip any that are
		// not fully created yet.
		if issue.PullRequest == nil {
			continue
		}
		issue.PullRequest.Issue = issue
		if err = issue.PullRequest.LoadAttributes(); err != nil {
			c.Error(err, "load pull request attributes")
			return
		}
		apiPRs = append(apiPRs, toPullRequest(issue))
	}

	c.SetLinkHeader(int(count), conf.UI.IssuePagingNum)
	c.JSONSuccess(&apiPRs)
}

func getPullRequest(c *context.APIContext) {
	issue := getPullRequestByIndex(c, c.ParamsInt64(":index"))
	if c.Written() {
		return
	}
	c.JSONSuccess(toPullRequest(issue))
}

type createPullRequestRequest struct {
	Title string `json:"title" binding:"Required"`
	Body  string `json:"body"`
	// Head is the branch to merge from, either "<branch>" in the base repository
	// or "<username>:<branch>" in that user's fork of the base repository.
	Head      string  `json:"head" binding:"Required"`
	Base      string  `json:"base" binding:"Required"`
	Assignee  string  `json:"assignee"`
	Milestone int64   `json:"milestone"`
	Labels    []int64 `json:"labels"`
}

func createPullRequest(c *context.APIContext, form createPullRequestRequest) {
	baseRepo := c.Repo.Repository
	baseGitRepo, err := git.Open(baseRepo.RepoPath())
	if err != nil {
		c.Error(err, "open base repository")
		return
	}
	if !baseGitRepo.HasBranch(form.Base) {
		c.ErrorStatus(http.StatusUnprocessableEntity, errors.Newf("base branch does not exist: %s", form.Base))
		return
	}

	headRepo := baseRepo
	headUsername := baseRepo.MustOwner().Name
	headBranch := form.Head
	if username, branch, ok := strings.Cut(form.Head, ":"); ok {
		headBranch = branch
		headUser, err := database.Handle.Users().GetByUsername(c.Req.Context(), username)
		if err != nil {
			if database.IsErrUserNotExist(err) {
				c.ErrorStatus(http.StatusUnprocessableEntity, errors.Newf("head user does not exist: %s", username))
			} else {
				c.Error(err, "get head user by name")
			}
			return
		}
		headUsername = headUser.Name

		if headUser.ID != baseRepo.OwnerID {
			var hasFork bool
			headRepo, hasFork, err = database.HasForkedRepo(headUser.ID, baseRepo.ID)
			if err != nil {
				c.Error(err, "get forked repository")
				return
			} else if !hasFork {
				c.ErrorStatus(http.StatusUnprocessableEntity, errors.Newf("%s does not have a fork of this repository", headUser.Name))
				return
			}
		}
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
		c.ErrorStatus(http.StatusUnprocessableEntity, errors.Newf("head branch does not exist: %s", form.Head))
		return
	}
	if headRepo.ID == baseRepo.ID && headBranch == form.Base {
		c.ErrorStatus(http.StatusUnprocessableEntity, errors.New("head and base must be different branches"))
		return
	}

	existing, err := database.GetUnmergedPullRequest(headRepo.ID, baseRepo.ID, headBranch, form.Base)
	if err == nil {
		c.ErrorStatus(http.StatusUnprocessableEntity, errors.Newf("an open pull request already exists for these branches: #%d", existing.Index))
		return
	} else if !database.IsErrPullRequestNotExist(err) {
		c.Error(err, "get unmerged pull request")
		return
	}

	meta, err := gitx.Module.PullRequestMeta(headGitRepo.Path(), baseGitRepo.Path(), headBranch, form.Base)
	if err != nil {
		if gitx.IsErrNoMergeBase(err) {
			c.ErrorStatus(http.StatusUnprocessableEntity, errors.New("head and base have no common history"))
		} else {
			c.Error(err, "get pull request meta")
		}
		return
	}
	if len(meta.Commits) == 0 {
		c.ErrorStatus(http.StatusUnprocessableEntity, errors.Newf("no commits between %s and %s", form.Base, form.Head))
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
	if c.Repo.IsWriter() {
		if form.Assignee != "" {
			assignee, err := database.Handle.Users().GetByUsername(c.Req.Context(), form.Assignee)
			if err != nil {
				if database.IsErrUserNotExist(err) {
					c.ErrorStatus(http.StatusUnprocessableEntity, errors.Newf("assignee does not exist: [name: %s]", form.Assignee))
				} else {
					c.Error(err, "get user by name")
				}
				return
			}
			pullIssue.AssigneeID = assignee.ID
		}
		if form.Milestone > 0 {
			if _, err = database.GetMilestoneByRepoID(baseRepo.ID, form.Milestone); err != nil {
				if database.IsErrMilestoneNotExist(err) {
					c.ErrorStatus(http.StatusUnprocessableEntity, errors.Newf("milestone does not exist: [id: %d]", form.Milestone))
				} else {
					c.Error(err, "get milestone by repository ID")
				}
				return
			}
			pullIssue.MilestoneID = form.Milestone
		}
	} else {
		form.Labels = nil
	}

	patch, err := headGitRepo.DiffBinary(meta.MergeBase, headBranch)
	if err != nil {
		c.Error(err, "get patch")
		return
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
	if err = database.NewPullRequest(baseRepo, pullIssue, form.Labels, nil, pullRequest, patch); err != nil {
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
	c.JSON(http.StatusCreated, toPullRequest(issue))
}

func editPullRequest(c *context.APIContext, form editIssueRequest) {
	issue := getPullRequestByIndex(c, c.ParamsInt64(":index"))
	if c.Written() {
		return
	}
	pr := issue.PullRequest

	if !issue.IsPoster(c.User.ID) && !c.Repo.IsWriter() {
		c.Status(http.StatusForbidden)
		return
	}

	// Validate the state change before saving anything, so a rejected request
	// does not leave a partial edit behind.
	var reopen, closing bool
	if form.State != nil {
		switch types.IssueStateType(*form.State) {
		case types.IssueStateOpen:
			reopen = issue.IsClosed
		case types.IssueStateClosed:
			closing = !issue.IsClosed
		default:
			c.ErrorStatus(http.StatusUnprocessableEntity, errors.Newf("state must be %q or %q", types.IssueStateOpen, types.IssueStateClosed))
			return
		}
	}
	if reopen {
		if pr.HasMerged {
			c.ErrorStatus(http.StatusUnprocessableEntity, errors.New("a merged pull request cannot be reopened"))
			return
		}
		if pr.HeadRepo == nil {
			c.ErrorStatus(http.StatusUnprocessableEntity, errors.New("the head repository has been deleted"))
			return
		}
		existing, err := database.GetUnmergedPullRequest(pr.HeadRepoID, pr.BaseRepoID, pr.HeadBranch, pr.BaseBranch)
		if err == nil {
			c.ErrorStatus(http.StatusUnprocessableEntity, errors.Newf("an open pull request already exists for these branches: #%d", existing.Index))
			return
		} else if !database.IsErrPullRequestNotExist(err) {
			c.Error(err, "get unmerged pull request")
			return
		}
	}

	if !applyIssueEdit(c, issue, form) {
		return
	}

	if reopen {
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

	issue = getPullRequestByIndex(c, c.ParamsInt64(":index"))
	if c.Written() {
		return
	}
	c.JSONSuccess(toPullRequest(issue))
}

func getPullRequestDiff(c *context.APIContext) {
	issue := getPullRequestByIndex(c, c.ParamsInt64(":index"))
	if c.Written() {
		return
	}

	patchPath, err := c.Repo.Repository.PatchPath(issue.Index)
	if err != nil {
		c.Error(err, "get patch path")
		return
	}
	// The patch is regenerated whenever the head branch is pushed, so it always
	// holds the latest changes between the merge base and the head branch.
	patch, err := os.Open(patchPath)
	if err != nil {
		if os.IsNotExist(err) {
			c.NotFound()
		} else {
			c.Error(err, "open patch")
		}
		return
	}
	defer func() { _ = patch.Close() }()

	c.Header().Set("Content-Type", "text/plain; charset=utf-8")
	c.Resp.WriteHeader(http.StatusOK)
	_, _ = io.Copy(c.Resp, patch)
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
	// MergeStyle is either "create_merge_commit" (the default) or
	// "rebase_before_merging".
	MergeStyle        string `json:"merge_style"`
	CommitDescription string `json:"commit_description"`
	// SHA, when set, must match the head branch commit for the merge to happen.
	SHA string `json:"sha"`
}

func mergePullRequest(c *context.APIContext, form mergePullRequestRequest) {
	issue := getPullRequestByIndex(c, c.ParamsInt64(":index"))
	if c.Written() {
		return
	}
	pr := issue.PullRequest

	switch {
	case pr.HasMerged:
		c.ErrorStatus(http.StatusMethodNotAllowed, errors.New("pull request has already been merged"))
		return
	case issue.IsClosed:
		c.ErrorStatus(http.StatusMethodNotAllowed, errors.New("pull request is closed"))
		return
	case pr.HeadRepo == nil:
		c.ErrorStatus(http.StatusMethodNotAllowed, errors.New("the head repository has been deleted"))
		return
	case pr.IsChecking():
		c.ErrorStatus(http.StatusMethodNotAllowed, errors.New("pull request is still being checked for conflicts, try again later"))
		return
	case !pr.CanAutoMerge():
		c.ErrorStatus(http.StatusMethodNotAllowed, errors.New("pull request has conflicts that must be resolved"))
		return
	}

	mergeStyle := database.MergeStyle(form.MergeStyle)
	switch mergeStyle {
	case "":
		mergeStyle = database.MergeStyleRegular
	case database.MergeStyleRegular:
	case database.MergeStyleRebase:
		if !c.Repo.Repository.PullsAllowRebase {
			c.ErrorStatus(http.StatusUnprocessableEntity, errors.New("rebase merging is not allowed in this repository"))
			return
		}
	default:
		c.ErrorStatus(http.StatusUnprocessableEntity, errors.Newf("merge_style must be %q or %q", database.MergeStyleRegular, database.MergeStyleRebase))
		return
	}

	if form.SHA != "" {
		headGitRepo, err := git.Open(pr.HeadRepo.RepoPath())
		if err != nil {
			c.Error(err, "open head repository")
			return
		}
		headCommitID, err := headGitRepo.BranchCommitID(pr.HeadBranch)
		if err != nil {
			c.Error(err, "get head branch commit ID")
			return
		}
		if headCommitID != form.SHA {
			c.ErrorStatus(http.StatusConflict, errors.Newf("head branch was modified, its commit is now %s", headCommitID))
			return
		}
	}

	baseGitRepo, err := git.Open(c.Repo.Repository.RepoPath())
	if err != nil {
		c.Error(err, "open base repository")
		return
	}
	issue.Repo = c.Repo.Repository
	if err = pr.Merge(c.User, baseGitRepo, mergeStyle, form.CommitDescription); err != nil {
		c.Error(err, "merge pull request")
		return
	}

	issue = getPullRequestByIndex(c, c.ParamsInt64(":index"))
	if c.Written() {
		return
	}
	c.JSONSuccess(toPullRequest(issue))
}
