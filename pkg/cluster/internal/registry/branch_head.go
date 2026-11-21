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
	// BranchHeadTTL is how long main's head SHA is reused: main
	// moves many times a day, and one call per repository per TTL is cheap.
	BranchHeadTTL = 5 * time.Minute
)

// BranchHead is a branch's head commit when it was read.
type BranchHead struct {
	SHA       string
	CheckedAt time.Time
}

var (
	branchHeadCache  = kube.NewLRU[string, BranchHead](64)
	branchHeadFlight kube.FlightGroup[BranchHead]
)

// LookupBranchHead reads a branch's head commit, reused for BranchHeadTTL.
func LookupBranchHead(ctx context.Context, repo, branch string) (BranchHead, error) {
	key := repo + "|" + branch
	if v, ok := branchHeadCache.Get(key); ok {
		return v, nil
	}
	v, err, _ := branchHeadFlight.Do(ctx, key, func(ctx context.Context) (BranchHead, error) {
		if v, ok := branchHeadCache.Get(key); ok {
			return v, nil
		}
		resp, err := GitHubGet(ctx, fmt.Sprintf("/repos/%s/branches/%s", repo, branch))
		if err != nil {
			return BranchHead{}, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, GitHubBodyLimit))
		if err != nil {
			return BranchHead{}, err
		}
		if resp.StatusCode != http.StatusOK {
			return BranchHead{}, fmt.Errorf("GitHub returned %d for branch %s of %s", resp.StatusCode, branch, repo)
		}
		var b struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(body, &b); err != nil {
			return BranchHead{}, fmt.Errorf("parse branch: %w", err)
		}
		sha := strings.ToLower(b.Commit.SHA)
		if !CommitSHAPattern.MatchString(sha) {
			return BranchHead{}, fmt.Errorf("GitHub reported no usable head commit for branch %s of %s", branch, repo)
		}
		v := BranchHead{SHA: sha, CheckedAt: time.Now()}
		branchHeadCache.Add(key, v, BranchHeadTTL)
		return v, nil
	})
	return v, err
}
