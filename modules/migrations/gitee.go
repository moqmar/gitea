// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package migrations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/migrations/base"
	"code.gitea.io/gitea/modules/structs"

	"gitee.com/openeuler/go-gitee"
)

var (
	_ base.Downloader        = &GiteeDownloader{}
	_ base.DownloaderFactory = &GiteeDownloaderFactory{}
)

func init() {
	RegisterDownloaderFactory(&GiteeDownloaderFactory{})
}

// GiteeDownloaderFactory defines a gitee downloader factory
type GiteeDownloaderFactory struct {
}

// New returns a Downloader related to this factory according MigrateOptions
func (f *GiteeDownloaderFactory) New(ctx context.Context, opts base.MigrateOptions) (base.Downloader, error) {
	u, err := url.Parse(opts.CloneAddr)
	if err != nil {
		return nil, err
	}

	baseURL := u.Scheme + "://" + u.Host
	repoNameSpace := strings.TrimPrefix(u.Path, "/")
	repoNameSpace = strings.TrimSuffix(repoNameSpace, ".git")

	log.Trace("Create gitee downloader. BaseURL: %s RepoName: %s", baseURL, repoNameSpace)

	return NewGiteeDownloader(ctx, baseURL, repoNameSpace, opts.AuthUsername, opts.AuthPassword, opts.AuthToken)
}

// GitServiceType returns the type of git service
func (f *GiteeDownloaderFactory) GitServiceType() structs.GitServiceType {
	return structs.GiteeService
}

// GiteeDownloader implements a Downloader interface to get repository informations
// from gitee via go-gitee
// - issueCount is incremented in GetIssues() to ensure PR and Issue numbers do not overlap,
// because Gitee has individual Issue and Pull Request numbers.
// - issueSeen, working alongside issueCount, is checked in GetComments() to see whether we
// need to fetch the Issue or PR comments, as Gitee stores them separately.
type GiteeDownloader struct {
	base.NullDownloader
	ctx             context.Context
	client          *gitee.Client
	repoID          int
	repoName        string
	issueCount      int64
	fetchPRcomments bool
	maxPerPage      int
}

// NewGiteeDownloader creates a gitee Downloader via gitee API
//   Use either a username/password, personal token entered into the username field, or anonymous/public access
//   Note: Public access only allows very basic access
func NewGiteeDownloader(ctx context.Context, baseURL, repoPath, username, password, token string) (*GiteeDownloader, error) {

	giteeClient, err := gitee.NewClient(token, gitee.WithBaseURL(baseURL))
	// Only use basic auth if token is blank and password is NOT
	// Basic auth will fail with empty strings, but empty token will allow anonymous public API usage
	if token == "" && password != "" {
		giteeClient, err = gitee.NewBasicAuthClient(username, password, gitee.WithBaseURL(baseURL))
	}

	if err != nil {
		log.Trace("Error logging into gitee: %v", err)
		return nil, err
	}

	// split namespace and subdirectory
	pathParts := strings.Split(strings.Trim(repoPath, "/"), "/")
	var resp *gitee.Response
	u, _ := url.Parse(baseURL)
	for len(pathParts) >= 2 {
		_, resp, err = giteeClient.Version.GetVersion()
		if err == nil || resp != nil && resp.StatusCode == 401 {
			err = nil // if no authentication given, this still should work
			break
		}

		u.Path = path.Join(u.Path, pathParts[0])
		baseURL = u.String()
		pathParts = pathParts[1:]
		_ = gitee.WithBaseURL(baseURL)(giteeClient)
		repoPath = strings.Join(pathParts, "/")
	}
	if err != nil {
		log.Trace("Error could not get gitee version: %v", err)
		return nil, err
	}

	log.Trace("gitee downloader: use BaseURL: '%s' and RepoPath: '%s'", baseURL, repoPath)

	// Grab and store project/repo ID here, due to issues using the URL escaped path
	gr, _, err := giteeClient.Projects.GetProject(repoPath, nil, nil, gitee.WithContext(ctx))
	if err != nil {
		log.Trace("Error retrieving project: %v", err)
		return nil, err
	}

	if gr == nil {
		log.Trace("Error getting project, project is nil")
		return nil, errors.New("Error getting project, project is nil")
	}

	return &GiteeDownloader{
		ctx:        ctx,
		client:     giteeClient,
		repoID:     gr.ID,
		repoName:   gr.Name,
		maxPerPage: 100,
	}, nil
}

// SetContext set context
func (g *GiteeDownloader) SetContext(ctx context.Context) {
	g.ctx = ctx
}

// GetRepoInfo returns a repository information
func (g *GiteeDownloader) GetRepoInfo() (*base.Repository, error) {
	gr, _, err := g.client.Projects.GetProject(g.repoID, nil, nil, gitee.WithContext(g.ctx))
	if err != nil {
		return nil, err
	}

	var private bool
	switch gr.Visibility {
	case gitee.InternalVisibility:
		private = true
	case gitee.PrivateVisibility:
		private = true
	}

	var owner string
	if gr.Owner == nil {
		log.Trace("gr.Owner is nil, trying to get owner from Namespace")
		if gr.Namespace != nil && gr.Namespace.Kind == "user" {
			owner = gr.Namespace.Path
		}
	} else {
		owner = gr.Owner.Username
	}

	// convert gitee repo to stand Repo
	return &base.Repository{
		Owner:         owner,
		Name:          gr.Name,
		IsPrivate:     private,
		Description:   gr.Description,
		OriginalURL:   gr.WebURL,
		CloneURL:      gr.HTTPURLToRepo,
		DefaultBranch: gr.DefaultBranch,
	}, nil
}

// GetTopics return gitee topics
func (g *GiteeDownloader) GetTopics() ([]string, error) {
	gr, _, err := g.client.Projects.GetProject(g.repoID, nil, nil, gitee.WithContext(g.ctx))
	if err != nil {
		return nil, err
	}
	return gr.TagList, err
}

// GetMilestones returns milestones
func (g *GiteeDownloader) GetMilestones() ([]*base.Milestone, error) {
	var perPage = g.maxPerPage
	var state = "all"
	var milestones = make([]*base.Milestone, 0, perPage)
	for i := 1; ; i++ {
		ms, _, err := g.client.Milestones.ListMilestones(g.repoID, &gitee.ListMilestonesOptions{
			State: &state,
			ListOptions: gitee.ListOptions{
				Page:    i,
				PerPage: perPage,
			}}, nil, gitee.WithContext(g.ctx))
		if err != nil {
			return nil, err
		}

		for _, m := range ms {
			var desc string
			if m.Description != "" {
				desc = m.Description
			}
			var state = "open"
			var closedAt *time.Time
			if m.State != "" {
				state = m.State
				if state == "closed" {
					closedAt = m.UpdatedAt
				}
			}

			var deadline *time.Time
			if m.DueDate != nil {
				deadlineParsed, err := time.Parse("2006-01-02", m.DueDate.String())
				if err != nil {
					log.Trace("Error parsing Milestone DueDate time")
					deadline = nil
				} else {
					deadline = &deadlineParsed
				}
			}

			milestones = append(milestones, &base.Milestone{
				Title:       m.Title,
				Description: desc,
				Deadline:    deadline,
				State:       state,
				Created:     *m.CreatedAt,
				Updated:     m.UpdatedAt,
				Closed:      closedAt,
			})
		}
		if len(ms) < perPage {
			break
		}
	}
	return milestones, nil
}

func (g *GiteeDownloader) normalizeColor(val string) string {
	val = strings.TrimLeft(val, "#")
	val = strings.ToLower(val)
	if len(val) == 3 {
		c := []rune(val)
		val = fmt.Sprintf("%c%c%c%c%c%c", c[0], c[0], c[1], c[1], c[2], c[2])
	}
	if len(val) != 6 {
		return ""
	}
	return val
}

// GetLabels returns labels
func (g *GiteeDownloader) GetLabels() ([]*base.Label, error) {
	var perPage = g.maxPerPage
	var labels = make([]*base.Label, 0, perPage)
	for i := 1; ; i++ {
		ls, _, err := g.client.Labels.ListLabels(g.repoID, &gitee.ListLabelsOptions{ListOptions: gitee.ListOptions{
			Page:    i,
			PerPage: perPage,
		}}, nil, gitee.WithContext(g.ctx))
		if err != nil {
			return nil, err
		}
		for _, label := range ls {
			baseLabel := &base.Label{
				Name:        label.Name,
				Color:       g.normalizeColor(label.Color),
				Description: label.Description,
			}
			labels = append(labels, baseLabel)
		}
		if len(ls) < perPage {
			break
		}
	}
	return labels, nil
}

func (g *GiteeDownloader) convertGiteeRelease(rel *gitee.Release) *base.Release {
	var zero int
	r := &base.Release{
		TagName:         rel.TagName,
		TargetCommitish: rel.Commit.ID,
		Name:            rel.Name,
		Body:            rel.Description,
		Created:         *rel.CreatedAt,
		PublisherID:     int64(rel.Author.ID),
		PublisherName:   rel.Author.Username,
	}

	for k, asset := range rel.Assets.Links {
		r.Assets = append(r.Assets, &base.ReleaseAsset{
			ID:            int64(asset.ID),
			Name:          asset.Name,
			ContentType:   &rel.Assets.Sources[k].Format,
			Size:          &zero,
			DownloadCount: &zero,
			DownloadFunc: func() (io.ReadCloser, error) {
				link, _, err := g.client.ReleaseLinks.GetReleaseLink(g.repoID, rel.TagName, asset.ID, gitee.WithContext(g.ctx))
				if err != nil {
					return nil, err
				}

				req, err := http.NewRequest("GET", link.URL, nil)
				if err != nil {
					return nil, err
				}
				req = req.WithContext(g.ctx)

				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return nil, err
				}

				// resp.Body is closed by the uploader
				return resp.Body, nil
			},
		})
	}
	return r
}

// GetReleases returns releases
func (g *GiteeDownloader) GetReleases() ([]*base.Release, error) {
	var perPage = g.maxPerPage
	var releases = make([]*base.Release, 0, perPage)
	for i := 1; ; i++ {
		ls, _, err := g.client.Releases.ListReleases(g.repoID, &gitee.ListReleasesOptions{
			Page:    i,
			PerPage: perPage,
		}, nil, gitee.WithContext(g.ctx))
		if err != nil {
			return nil, err
		}

		for _, release := range ls {
			releases = append(releases, g.convertGiteeRelease(release))
		}
		if len(ls) < perPage {
			break
		}
	}
	return releases, nil
}

// GetIssues returns issues according start and limit
//   Note: issue label description and colors are not supported by the go-gitee library at this time
func (g *GiteeDownloader) GetIssues(page, perPage int) ([]*base.Issue, bool, error) {
	state := "all"
	sort := "asc"

	if perPage > g.maxPerPage {
		perPage = g.maxPerPage
	}

	opt := &gitee.ListProjectIssuesOptions{
		State: &state,
		Sort:  &sort,
		ListOptions: gitee.ListOptions{
			PerPage: perPage,
			Page:    page,
		},
	}

	var allIssues = make([]*base.Issue, 0, perPage)

	issues, _, err := g.client.Issues.ListProjectIssues(g.repoID, opt, nil, gitee.WithContext(g.ctx))
	if err != nil {
		return nil, false, fmt.Errorf("error while listing issues: %v", err)
	}
	for _, issue := range issues {

		var labels = make([]*base.Label, 0, len(issue.Labels))
		for _, l := range issue.Labels {
			labels = append(labels, &base.Label{
				Name: l,
			})
		}

		var milestone string
		if issue.Milestone != nil {
			milestone = issue.Milestone.Title
		}

		var reactions []*base.Reaction
		var awardPage = 1
		for {
			awards, _, err := g.client.AwardEmoji.ListIssueAwardEmoji(g.repoID, issue.IID, &gitee.ListAwardEmojiOptions{Page: awardPage, PerPage: perPage}, gitee.WithContext(g.ctx))
			if err != nil {
				return nil, false, fmt.Errorf("error while listing issue awards: %v", err)
			}
			if len(awards) < perPage {
				break
			}
			for i := range awards {
				reactions = append(reactions, g.awardToReaction(awards[i]))
			}
			awardPage++
		}

		allIssues = append(allIssues, &base.Issue{
			Title:      issue.Title,
			Number:     int64(issue.IID),
			PosterID:   int64(issue.Author.ID),
			PosterName: issue.Author.Username,
			Content:    issue.Description,
			Milestone:  milestone,
			State:      issue.State,
			Created:    *issue.CreatedAt,
			Labels:     labels,
			Reactions:  reactions,
			Closed:     issue.ClosedAt,
			IsLocked:   issue.DiscussionLocked,
			Updated:    *issue.UpdatedAt,
		})

		// increment issueCount, to be used in GetPullRequests()
		g.issueCount++
	}

	return allIssues, len(issues) < perPage, nil
}

// GetComments returns comments according issueNumber
// TODO: figure out how to transfer comment reactions
func (g *GiteeDownloader) GetComments(issueNumber int64) ([]*base.Comment, error) {
	var allComments = make([]*base.Comment, 0, g.maxPerPage)

	var page = 1
	var realIssueNumber int64

	for {
		var comments []*gitee.Discussion
		var resp *gitee.Response
		var err error
		// fetchPRcomments decides whether to fetch Issue or PR comments
		if !g.fetchPRcomments {
			realIssueNumber = issueNumber
			comments, resp, err = g.client.Discussions.ListIssueDiscussions(g.repoID, int(realIssueNumber), &gitee.ListIssueDiscussionsOptions{
				Page:    page,
				PerPage: g.maxPerPage,
			}, nil, gitee.WithContext(g.ctx))
		} else {
			// If this is a PR, we need to figure out the Gitee/original PR ID to be passed below
			realIssueNumber = issueNumber - g.issueCount
			comments, resp, err = g.client.Discussions.ListMergeRequestDiscussions(g.repoID, int(realIssueNumber), &gitee.ListMergeRequestDiscussionsOptions{
				Page:    page,
				PerPage: g.maxPerPage,
			}, nil, gitee.WithContext(g.ctx))
		}

		if err != nil {
			return nil, fmt.Errorf("error while listing comments: %v %v", g.repoID, err)
		}
		for _, comment := range comments {
			// Flatten comment threads
			if !comment.IndividualNote {
				for _, note := range comment.Notes {
					allComments = append(allComments, &base.Comment{
						IssueIndex:  realIssueNumber,
						PosterID:    int64(note.Author.ID),
						PosterName:  note.Author.Username,
						PosterEmail: note.Author.Email,
						Content:     note.Body,
						Created:     *note.CreatedAt,
					})
				}
			} else {
				c := comment.Notes[0]
				allComments = append(allComments, &base.Comment{
					IssueIndex:  realIssueNumber,
					PosterID:    int64(c.Author.ID),
					PosterName:  c.Author.Username,
					PosterEmail: c.Author.Email,
					Content:     c.Body,
					Created:     *c.CreatedAt,
				})
			}

		}
		if resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return allComments, nil
}

// GetPullRequests returns pull requests according page and perPage
func (g *GiteeDownloader) GetPullRequests(page, perPage int) ([]*base.PullRequest, bool, error) {
	if perPage > g.maxPerPage {
		perPage = g.maxPerPage
	}

	opt := &gitee.ListProjectMergeRequestsOptions{
		ListOptions: gitee.ListOptions{
			PerPage: perPage,
			Page:    page,
		},
	}

	// Set fetchPRcomments to true here, so PR comments are fetched instead of Issue comments
	g.fetchPRcomments = true

	var allPRs = make([]*base.PullRequest, 0, perPage)

	prs, _, err := g.client.MergeRequests.ListProjectMergeRequests(g.repoID, opt, nil, gitee.WithContext(g.ctx))
	if err != nil {
		return nil, false, fmt.Errorf("error while listing merge requests: %v", err)
	}
	for _, pr := range prs {

		var labels = make([]*base.Label, 0, len(pr.Labels))
		for _, l := range pr.Labels {
			labels = append(labels, &base.Label{
				Name: l,
			})
		}

		var merged bool
		if pr.State == "merged" {
			merged = true
			pr.State = "closed"
		}

		var mergeTime = pr.MergedAt
		if merged && pr.MergedAt == nil {
			mergeTime = pr.UpdatedAt
		}

		var closeTime = pr.ClosedAt
		if merged && pr.ClosedAt == nil {
			closeTime = pr.UpdatedAt
		}

		var locked bool
		if pr.State == "locked" {
			locked = true
		}

		var milestone string
		if pr.Milestone != nil {
			milestone = pr.Milestone.Title
		}

		var reactions []*base.Reaction
		var awardPage = 1
		for {
			awards, _, err := g.client.AwardEmoji.ListMergeRequestAwardEmoji(g.repoID, pr.IID, &gitee.ListAwardEmojiOptions{Page: awardPage, PerPage: perPage}, gitee.WithContext(g.ctx))
			if err != nil {
				return nil, false, fmt.Errorf("error while listing merge requests awards: %v", err)
			}
			if len(awards) < perPage {
				break
			}
			for i := range awards {
				reactions = append(reactions, g.awardToReaction(awards[i]))
			}
			awardPage++
		}

		// Add the PR ID to the Issue Count because PR and Issues share ID space in Gitea
		newPRNumber := g.issueCount + int64(pr.IID)

		allPRs = append(allPRs, &base.PullRequest{
			Title:          pr.Title,
			Number:         newPRNumber,
			OriginalNumber: int64(pr.IID),
			PosterName:     pr.Author.Username,
			PosterID:       int64(pr.Author.ID),
			Content:        pr.Description,
			Milestone:      milestone,
			State:          pr.State,
			Created:        *pr.CreatedAt,
			Closed:         closeTime,
			Labels:         labels,
			Merged:         merged,
			MergeCommitSHA: pr.MergeCommitSHA,
			MergedTime:     mergeTime,
			IsLocked:       locked,
			Reactions:      reactions,
			Head: base.PullRequestBranch{
				Ref:       pr.SourceBranch,
				SHA:       pr.SHA,
				RepoName:  g.repoName,
				OwnerName: pr.Author.Username,
				CloneURL:  pr.WebURL,
			},
			Base: base.PullRequestBranch{
				Ref:       pr.TargetBranch,
				SHA:       pr.DiffRefs.BaseSha,
				RepoName:  g.repoName,
				OwnerName: pr.Author.Username,
			},
			PatchURL: pr.WebURL + ".patch",
		})
	}

	return allPRs, len(prs) < perPage, nil
}

// GetReviews returns pull requests review
func (g *GiteeDownloader) GetReviews(pullRequestNumber int64) ([]*base.Review, error) {
	state, resp, err := g.client.MergeRequestApprovals.GetApprovalState(g.repoID, int(pullRequestNumber), gitee.WithContext(g.ctx))
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			log.Error(fmt.Sprintf("GiteeDownloader: while migrating a error occurred: '%s'", err.Error()))
			return []*base.Review{}, nil
		}
		return nil, err
	}

	// GitLab's Approvals are equivalent to Gitea's approve reviews
	approvers := make(map[int]string)
	for i := range state.Rules {
		for u := range state.Rules[i].ApprovedBy {
			approvers[state.Rules[i].ApprovedBy[u].ID] = state.Rules[i].ApprovedBy[u].Username
		}
	}

	var reviews = make([]*base.Review, 0, len(approvers))
	for id, name := range approvers {
		reviews = append(reviews, &base.Review{
			ReviewerID:   int64(id),
			ReviewerName: name,
			// GitLab API doesn't return a creation date
			CreatedAt: time.Now(),
			// All we get are approvals
			State: base.ReviewStateApproved,
		})
	}

	return reviews, nil
}

func (g *GiteeDownloader) awardToReaction(award *gitee.AwardEmoji) *base.Reaction {
	return &base.Reaction{
		UserID:   int64(award.User.ID),
		UserName: award.User.Username,
		Content:  award.Name,
	}
}
