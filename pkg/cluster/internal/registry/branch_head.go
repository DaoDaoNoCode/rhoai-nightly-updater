package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// CommitSHAPattern matches a full commit SHA (40 lower-case hex).
var CommitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

const (
	// branchHeadTTL is how long main's head SHA is reused: main
	// moves many times a day, and one call per repository per TTL is cheap.
	branchHeadTTL = 5 * time.Minute
)

// branchHead is a branch's head commit when it was read.
type branchHead struct {
	SHA       string
	CheckedAt time.Time
}

var (
	branchHeadCache  = kube.NewLRU[string, branchHead](64)
	branchHeadFlight kube.FlightGroup[branchHead]
)

// LookupBranchHead reads a branch's head commit, reused for branchHeadTTL.
func LookupBranchHead(ctx context.Context, repo, branch string) (branchHead, error) {
	key := repo + "|" + branch
	if v, ok := branchHeadCache.Get(key); ok {
		return v, nil
	}
	v, err, _ := branchHeadFlight.Do(ctx, key, func(ctx context.Context) (branchHead, error) {
		if v, ok := branchHeadCache.Get(key); ok {
			return v, nil
		}
		resp, err := GitHubGet(ctx, fmt.Sprintf("/repos/%s/branches/%s", repo, branch))
		if err != nil {
			return branchHead{}, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, GitHubBodyLimit))
		if err != nil {
			return branchHead{}, err
		}
		if resp.StatusCode != http.StatusOK {
			return branchHead{}, fmt.Errorf("GitHub returned %d for branch %s of %s", resp.StatusCode, branch, repo)
		}
		var b struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(body, &b); err != nil {
			return branchHead{}, fmt.Errorf("parse branch: %w", err)
		}
		sha := strings.ToLower(b.Commit.SHA)
		if !CommitSHAPattern.MatchString(sha) {
			return branchHead{}, fmt.Errorf("GitHub reported no usable head commit for branch %s of %s", branch, repo)
		}
		v := branchHead{SHA: sha, CheckedAt: time.Now()}
		branchHeadCache.Add(key, v, branchHeadTTL)
		return v, nil
	})
	return v, err
}
