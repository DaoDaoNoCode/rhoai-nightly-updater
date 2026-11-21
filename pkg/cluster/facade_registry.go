package cluster

import (
	"context"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The image registry and GitHub reads (internal/registry), under the names
// pkg/api and this package use.

// The PR and commit search's default repository and its scope.
const (
	DefaultPRSearchRepo = registry.DefaultPRSearchRepo
	SearchRepoMessage   = registry.SearchRepoMessage
)

// PRSearchError is registry.PRSearchError.
type PRSearchError = registry.PRSearchError

// ValidSearchRepo is registry.ValidSearchRepo.
func ValidSearchRepo(repo string) bool { return registry.ValidSearchRepo(repo) }

// FindBuildsContainingPR is registry.FindBuildsContainingPR.
func FindBuildsContainingPR(ctx context.Context, c *Client, repo string, number int, images []string) (*types.PRContainsResponse, error) {
	return registry.FindBuildsContainingPR(ctx, c, repo, number, images)
}

// FindBuildsContainingCommit is registry.FindBuildsContainingCommit.
func FindBuildsContainingCommit(ctx context.Context, c *Client, repo, sha string, images []string) (*types.PRContainsResponse, error) {
	return registry.FindBuildsContainingCommit(ctx, c, repo, sha, images)
}

// ExtractFBCContent is registry.ExtractFBCContent.
func ExtractFBCContent(ctx context.Context, c *Client, imageRef string) (*types.FBCContentResponse, error) {
	return registry.ExtractFBCContent(ctx, c, imageRef)
}

// ResolveRelatedImageLabels is registry.ResolveRelatedImageLabels.
func ResolveRelatedImageLabels(ctx context.Context, c *Client, images []types.RelatedImage) []types.RelatedImage {
	return registry.ResolveRelatedImageLabels(ctx, c, images)
}

// FetchNightlyTags is registry.FetchNightlyTags.
func FetchNightlyTags(ctx context.Context, c *Client, limit int) (*types.NightlyTagsResponse, error) {
	return registry.FetchNightlyTags(ctx, c, limit)
}

// FetchLatestNightly is registry.FetchLatestNightly.
func FetchLatestNightly(ctx context.Context, c *Client) (*types.LatestNightlyResponse, error) {
	return registry.FetchLatestNightly(ctx, c)
}

// FetchNightlyTagsWithBuildDates is registry.FetchNightlyTagsWithBuildDates.
func FetchNightlyTagsWithBuildDates(ctx context.Context, c *Client, limit int) (*types.NightlyTagsResponse, error) {
	return registry.FetchNightlyTagsWithBuildDates(ctx, c, limit)
}
