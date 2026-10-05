package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// nightlyStatusFresh is how long a Quay lookup of the latest build is served
// without revalidation. Older entries are still served while one background
// refresh runs, so /api/status never waits on Quay twice. (A var for tests.)
var nightlyStatusFresh = 60 * time.Second

const (
	// nightlyStatusErrorTTL keeps a failed lookup for one poll interval only.
	nightlyStatusErrorTTL = 30 * time.Second
	// nightlyStatusTimeout bounds the one synchronous lookup made when the
	// installed image has never been checked (first poll, or right after an
	// update changed the CatalogSource image).
	nightlyStatusTimeout = 6 * time.Second
	// nightlyBackgroundTimeout bounds refreshes and dashboard-commit warm-ups.
	nightlyBackgroundTimeout = 90 * time.Second
)

// nightlyBackground tracks background refreshes so tests can wait for them.
var nightlyBackground sync.WaitGroup

func goBackground(fn func()) {
	nightlyBackground.Add(1)
	go func() {
		defer nightlyBackground.Done()
		fn()
	}()
}

var (
	nightlyStatusCache  = newLRU[string, types.NightlyStatus](64)
	nightlyStatusFlight flightGroup[types.NightlyStatus]
	// dashboardWarmAttempts remembers FBC images whose catalog was recently
	// read (or failed) in the background, so each poll does not retry.
	dashboardWarmAttempts = newLRU[string, bool](128)
	dashboardWarmFlight   flightGroup[bool]
)

// nightlyBuildFromImage describes the catalog image configured on the
// nightly CatalogSource.
func nightlyBuildFromImage(image string) *types.NightlyBuild {
	b := &types.NightlyBuild{Image: image, Tag: extractTagFromRef(image)}
	if _, digest, ok := extractRepoAndDigest(image); ok {
		b.Digest = digest
	}
	return b
}

// isNightlyStreamImage reports whether image is a quay.io/rhoai FBC fragment
// whose tag names a release stream (e.g. rhoai-3.6), so the newest build of
// the same stream can be looked up.
func isNightlyStreamImage(image string) bool {
	repoAndTag := image
	if at := strings.Index(repoAndTag, "@"); at >= 0 {
		repoAndTag = repoAndTag[:at]
	}
	if !strings.HasPrefix(repoAndTag, quayImage+":") {
		return false
	}
	_, ok := parseTag(extractTagFromRef(image))
	return ok
}

// getNightlyStatus compares the installed nightly catalog with the newest
// build of its stream. It returns nil when the Subscription does not use the
// nightly CatalogSource. Lookups are cached per installed image: a changed
// CatalogSource image (an update) is always checked against Quay right away,
// so the result never hides the effect of an update.
func getNightlyStatus(ctx context.Context, basicAuth string, sub types.SubscriptionInfo, cs types.CatalogSourceInfo) *types.NightlyStatus {
	if !cs.Exists || cs.Image == "" || sub.Source != CatalogName {
		return nil
	}
	installed := nightlyBuildFromImage(cs.Image)
	if !isNightlyStreamImage(cs.Image) {
		// A custom catalog image: show it, but there is no stream to compare with.
		status := types.NightlyStatus{Installed: installed}
		addCachedBuildDetails(&status)
		return &status
	}

	key := cs.Image
	if cached, stored, ok := nightlyStatusCache.GetWithAge(key); ok {
		if time.Since(stored) >= nightlyStatusFresh {
			goBackground(func() { refreshNightlyStatus(basicAuth, key, installed) })
		}
		addCachedBuildDetails(&cached)
		return &cached
	}

	lookupCtx, cancel := context.WithTimeout(ctx, nightlyStatusTimeout)
	defer cancel()
	status, err, _ := nightlyStatusFlight.Do(lookupCtx, key, func(ctx context.Context) (types.NightlyStatus, error) {
		return lookupNightlyStatus(ctx, basicAuth, key, installed), nil
	})
	if err != nil {
		// Our own wait ended; a lookup may still be running for another request.
		status = types.NightlyStatus{Installed: installed, Error: "Still checking Quay for newer builds"}
	}
	addCachedBuildDetails(&status)
	return &status
}

func refreshNightlyStatus(basicAuth, key string, installed *types.NightlyBuild) {
	ctx, cancel := context.WithTimeout(context.Background(), nightlyBackgroundTimeout)
	defer cancel()
	_, _, _ = nightlyStatusFlight.Do(ctx, key, func(ctx context.Context) (types.NightlyStatus, error) {
		if _, stored, ok := nightlyStatusCache.GetWithAge(key); ok && time.Since(stored) < nightlyStatusFresh {
			return types.NightlyStatus{}, nil // another refresh just finished
		}
		return lookupNightlyStatus(ctx, basicAuth, key, installed), nil
	})
}

// lookupNightlyStatus reads the current digest of the installed stream's tag
// and the build dates of both images, then caches the comparison.
func lookupNightlyStatus(ctx context.Context, basicAuth, key string, installed *types.NightlyBuild) types.NightlyStatus {
	status := types.NightlyStatus{Installed: copyBuild(installed)}
	// Always read the tag's digest from Quay here (maxAge 0): this is the
	// value updateAvailable is computed from.
	digest, err := cachedTagDigest(ctx, basicAuth, installed.Tag, 0)
	if err != nil {
		status.Error = fmt.Sprintf("Could not read the latest %s build from Quay: %v", installed.Tag, err)
		nightlyStatusCache.Add(key, status, nightlyStatusErrorTTL)
		return status
	}
	status.Latest = &types.NightlyBuild{
		Image:  fmt.Sprintf("%s:%s@%s", quayImage, installed.Tag, digest),
		Tag:    installed.Tag,
		Digest: digest,
	}
	status.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	if installed.Digest != "" {
		update := installed.Digest != digest
		status.UpdateAvailable = &update
	}

	refs := []string{status.Latest.Image}
	if installed.Digest != "" {
		refs = append(refs, installed.Image)
	}
	labels := resolveImageLabels(ctx, basicAuth, refs, false)
	if l := labels[status.Installed.Image]; l != nil {
		status.Installed.BuildDate = l.BuildDate
	}
	if l := labels[status.Latest.Image]; l != nil {
		status.Latest.BuildDate = l.BuildDate
	}
	nightlyStatusCache.Add(key, status, 0)

	// The dashboard commit needs the FBC catalog (a few MB) and one more
	// label read, so it is filled in the background for later polls.
	for _, ref := range refs {
		goBackground(func() { warmDashboardCommit(basicAuth, ref) })
	}
	return status
}

func copyBuild(b *types.NightlyBuild) *types.NightlyBuild {
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

// addCachedBuildDetails fills build dates and dashboard commits that are
// already cached. It makes no network call.
func addCachedBuildDetails(status *types.NightlyStatus) {
	status.Installed = copyBuild(status.Installed)
	status.Latest = copyBuild(status.Latest)
	for _, b := range []*types.NightlyBuild{status.Installed, status.Latest} {
		if b == nil || b.Digest == "" {
			continue
		}
		if b.BuildDate == "" {
			if l, ok := cachedImageLabels(b.Image); ok {
				b.BuildDate = l.BuildDate
			}
		}
		if b.DashboardCommit == "" {
			b.DashboardCommit, b.DashboardGitURL = cachedDashboardCommit(b.Image)
		}
	}
}

// dashboardRelatedImage returns the odh-dashboard image of a parsed catalog.
func dashboardRelatedImage(content *types.FBCContentResponse) string {
	for _, ri := range content.RelatedImages {
		if ri.Name == "odh_dashboard_image" {
			return ri.Image
		}
	}
	for _, ri := range content.RelatedImages {
		if strings.Contains(ri.Image, "/odh-dashboard-rhel") {
			return ri.Image
		}
	}
	return ""
}

func cachedDashboardCommit(fbcImage string) (string, string) {
	content, ok := cachedFBCContent(fbcImage)
	if !ok {
		return "", ""
	}
	dashboard := dashboardRelatedImage(content)
	if dashboard == "" {
		return "", ""
	}
	labels, ok := cachedImageLabels(dashboard)
	if !ok {
		return "", ""
	}
	return labels.GitCommit, labels.GitURL
}

// warmDashboardCommit reads the catalog of a digest-pinned FBC image and the
// labels of its dashboard image into the caches. At most one attempt per
// image runs per 10 minutes, whatever the outcome.
func warmDashboardCommit(basicAuth, fbcImage string) {
	if _, _, ok := extractRepoAndDigest(fbcImage); !ok {
		return
	}
	if commit, _ := cachedDashboardCommit(fbcImage); commit != "" {
		return
	}
	if _, attempted := dashboardWarmAttempts.Get(fbcImage); attempted {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), nightlyBackgroundTimeout)
	defer cancel()
	_, _, _ = dashboardWarmFlight.Do(ctx, fbcImage, func(ctx context.Context) (bool, error) {
		if _, attempted := dashboardWarmAttempts.Get(fbcImage); attempted {
			return true, nil
		}
		defer dashboardWarmAttempts.Add(fbcImage, true, 10*time.Minute)
		content, err := extractFBCContentWithAuth(ctx, basicAuth, fbcImage)
		if err != nil {
			slog.Debug("dashboard commit: reading FBC catalog failed", "image", truncateForLog(fbcImage), "error", err)
			return false, nil
		}
		if dashboard := dashboardRelatedImage(content); dashboard != "" {
			if _, err := imageLabelsWithAuth(ctx, basicAuth, dashboard, false); err != nil {
				slog.Debug("dashboard commit: reading labels failed", "image", truncateForLog(dashboard), "error", err)
			}
		}
		return true, nil
	})
}
