package main

import (
	"context"
	"strings"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

// validBranchName applies git's core rules for a branch name, so a bad name is
// refused here as validation rather than as a GitLab 400.
func validBranchName(name string) bool {
	if name == "" || len(name) > 250 || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.HasPrefix(name, "-") || strings.Contains(name, "..") || strings.Contains(name, "//") ||
		strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".") || strings.Contains(name, "@{") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	return true
}

type glBranch struct {
	Name   string `json:"name"`
	WebURL string `json:"web_url"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

func branchInfo(t gp.Target, b glBranch) gp.BranchInfo {
	u := b.WebURL
	if u == "" {
		u = webURL(hostOf(t), t.Repo.FullPath, "/-/tree/"+b.Name)
	}
	return gp.BranchInfo{Name: b.Name, SHA: b.Commit.ID, URL: u}
}

// GetBranch implements gp.BranchManager.
func (p *glProvider) GetBranch(_ context.Context, req gp.GetBranchRequest) (gp.BranchInfo, error) {
	if !validBranchName(req.Name) {
		return gp.BranchInfo{}, gp.Errorf(gp.CodeValidation, "invalid branch name")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.BranchInfo{}, err
	}
	var b glBranch
	if err := api.get(proj+"/repository/branches/"+pathPart(req.Name), &b); err != nil {
		return gp.BranchInfo{}, err
	}
	return branchInfo(req.Target, b), nil
}

// CreateBranch implements gp.BranchManager: a new branch at the head of From
// (the default branch when empty). The source is looked up first so a missing
// one is not_found; GitLab answers 400 "Branch already exists" for a taken name,
// which is classified as a conflict.
func (p *glProvider) CreateBranch(_ context.Context, req gp.CreateBranchRequest) (gp.BranchInfo, error) {
	if !validBranchName(req.Name) {
		return gp.BranchInfo{}, gp.Errorf(gp.CodeValidation, "invalid branch name")
	}
	from := strings.TrimSpace(req.From)
	if from == "" {
		from = req.Target.Repo.DefaultBranch
	}
	if from == "" {
		return gp.BranchInfo{}, gp.Errorf(gp.CodeValidation, "the source branch is required: the repository has no known default branch")
	}
	if !validBranchName(from) {
		return gp.BranchInfo{}, gp.Errorf(gp.CodeValidation, "invalid source branch name")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.BranchInfo{}, err
	}
	var src glBranch
	if err := api.get(proj+"/repository/branches/"+pathPart(from), &src); err != nil {
		return gp.BranchInfo{}, err
	}
	var made glBranch
	if err := api.send("POST", proj+"/repository/branches", map[string]any{"branch": req.Name, "ref": src.Commit.ID}, &made); err != nil {
		return gp.BranchInfo{}, err
	}
	if made.Name == "" {
		made.Name, made.Commit = req.Name, src.Commit
	}
	return branchInfo(req.Target, made), nil
}
