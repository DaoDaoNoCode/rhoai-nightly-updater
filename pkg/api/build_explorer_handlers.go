package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// HandleLatestNightly fetches the latest nightly image tag from Quay.
var HandleLatestNightly = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("request", "op", "latest-nightly")

	result, err := cluster.FetchLatestNightly(r.Context(), c)
	if err != nil {
		slog.Error("latest-nightly failed", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, fmt.Sprintf("failed to fetch latest nightly: %v", err), code, errorCode)
		return
	}
	writeJSON(w, result, "latest-nightly")
})

// HandleNightlyTags returns the last N nightly version tags from Quay.
var HandleNightlyTags = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("request", "op", "nightly-tags")

	result, err := cluster.FetchNightlyTags(r.Context(), c, 5)
	if err != nil {
		slog.Error("nightly-tags failed", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to fetch nightly tags", code, errorCode)
		return
	}
	writeJSON(w, result, "nightly-tags")
})

// HandleBuildExplorerTags returns all nightly FBC tags from Quay.
var HandleBuildExplorerTags = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	slog.Info("request", "op", "build-explorer-tags")

	fetch := cluster.FetchNightlyTagsWithBuildDates
	if r.URL.Query().Get("includeDates") == "false" {
		fetch = cluster.FetchNightlyTags
	}
	result, err := fetch(r.Context(), c, 0)
	if err != nil {
		slog.Error("build-explorer-tags failed", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to fetch nightly tags", code, errorCode)
		return
	}

	writeJSON(w, result, "build-explorer-tags")
})

// HandleBuildExplorerContent returns the FBC content (relatedImages) for a specific tag.
var HandleBuildExplorerContent = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	image := r.URL.Query().Get("image")
	if image == "" {
		writeError(w, "missing 'image' query parameter", http.StatusBadRequest, "validation")
		return
	}
	if !imageRegex.MatchString(image) {
		writeError(w, "invalid image reference", http.StatusBadRequest, "validation")
		return
	}
	if !isAllowedImage(image) {
		writeError(w, fmt.Sprintf("image registry not allowed: %s (must be quay.io/rhoai/ or registry.redhat.io/rhoai/)", image), http.StatusBadRequest, "validation")
		return
	}

	labels := r.URL.Query().Get("labels") == "true"

	logImage := image
	if len(logImage) > 80 {
		logImage = logImage[:80] + "..."
	}
	slog.Info("request", "op", "build-explorer-content", "image", logImage, "labels", labels)

	result, err := cluster.ExtractFBCContent(r.Context(), c, image)
	if err != nil {
		slog.Error("build-explorer-content failed", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to extract FBC content", code, errorCode)
		return
	}

	if labels && len(result.RelatedImages) > 0 {
		imagesCopy := make([]types.RelatedImage, len(result.RelatedImages))
		copy(imagesCopy, result.RelatedImages)
		enriched := cluster.ResolveRelatedImageLabels(r.Context(), c, imagesCopy)
		enrichedResult := *result
		enrichedResult.RelatedImages = enriched
		writeJSON(w, &enrichedResult, "build-explorer-content")
		return
	}

	writeJSON(w, result, "build-explorer-content")
})

// prSearchMaxImages bounds the builds one PR search request names.
const prSearchMaxImages = 40

// commitSearchRegex is the commit a search may name: 7 to 40 hex characters.
var commitSearchRegex = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// HandleBuildExplorerContains reports which nightly builds contain a merged
// pull request or a commit:
// GET /api/build-explorer/contains?pr=<N>|commit=<sha>[&repo=<owner/name>][&image=<FBC ref>...].
// Exactly one of pr and commit; repo defaults to opendatahub-io/odh-dashboard
// and must belong to one of the owners cluster.ValidSearchRepo allows.
// Without image, the installed build and the newest build of every tag are
// checked. A GitHub rate limit gives partial results (rateLimited), never an error.
var HandleBuildExplorerContains = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo := q.Get("repo")
	if repo == "" {
		repo = cluster.DefaultPRSearchRepo
	}
	if !cluster.ValidSearchRepo(repo) {
		writeError(w, cluster.SearchRepoMessage, http.StatusBadRequest, "validation")
		return
	}
	prParam, commit := q.Get("pr"), q.Get("commit")
	if (prParam == "") == (commit == "") {
		writeError(w, "give exactly one of 'pr' and 'commit'", http.StatusBadRequest, "validation")
		return
	}
	var pr int
	if prParam != "" {
		var err error
		pr, err = strconv.Atoi(prParam)
		if err != nil || pr <= 0 || pr > 99999999 {
			writeError(w, "'pr' must be a pull request number", http.StatusBadRequest, "validation")
			return
		}
	} else if !commitSearchRegex.MatchString(commit) {
		writeError(w, "'commit' must be a commit SHA of 7 to 40 hex characters", http.StatusBadRequest, "validation")
		return
	}
	images := q["image"]
	if len(images) > prSearchMaxImages {
		writeError(w, fmt.Sprintf("at most %d images per request", prSearchMaxImages), http.StatusBadRequest, "validation")
		return
	}
	for _, image := range images {
		// Digest-pinned builds only: a tag moves, so its answer would change.
		if !customFBCImageRegex.MatchString(image) || !strings.Contains(image, "@sha256:") {
			writeError(w, "each 'image' must be a digest-pinned quay.io/rhoai/rhoai-fbc-fragment reference", http.StatusBadRequest, "validation")
			return
		}
	}
	slog.Info("request", "op", "build-explorer-contains", "repo", repo, "pr", pr, "commit", commit, "images", len(images))

	var result *types.PRContainsResponse
	var err error
	if commit != "" {
		result, err = cluster.FindBuildsContainingCommit(r.Context(), c, repo, commit, images)
	} else {
		result, err = cluster.FindBuildsContainingPR(r.Context(), c, repo, pr, images)
	}
	if err != nil {
		if searchErr, ok := errors.AsType[*cluster.PRSearchError](err); ok {
			writeError(w, searchErr.Msg, searchErr.Status, searchErr.Code)
			return
		}
		slog.Error("build-explorer-contains failed", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "failed to search the builds", code, errorCode)
		return
	}
	writeJSON(w, result, "build-explorer-contains")
})
