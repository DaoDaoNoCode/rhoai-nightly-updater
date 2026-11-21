package catalog

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// extractTargetVersion parses the target major.minor from an FBC image tag.
// e.g., "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5@sha256:..." → (3, 5, true)
func extractTargetVersion(imageRef string) (int, int, bool) {
	// Extract the tag portion: "rhoai-3.5" from the full image ref
	ref := imageRef
	if atIdx := strings.Index(ref, "@"); atIdx >= 0 {
		ref = ref[:atIdx]
	}
	colonIdx := strings.LastIndex(ref, ":")
	if colonIdx < 0 {
		return 0, 0, false
	}
	tag := ref[colonIdx+1:]

	var major, minor int
	n, _ := fmt.Sscanf(tag, "rhoai-%d.%d", &major, &minor)
	if n == 2 {
		return major, minor, true
	}
	return 0, 0, false
}

func BestChannel(channels []any, imageRef string) (string, error) {
	if len(channels) == 0 {
		slog.Warn("detectNightlyChannel: no channels found in packagemanifest")
		return "", nil
	}

	targetMajor, targetMinor, hasTarget := extractTargetVersion(imageRef)
	targetRelease, hasRelease := registry.ParseTag(registry.TagFromRef(imageRef))
	if hasTarget {
		slog.Info("detectNightlyChannel: filtering to target version", "major", targetMajor, "minor", targetMinor, "image", imageRef)
	}

	bestChannel := ""
	var bestParsed *registry.Tag
	for _, ch := range channels {
		channel, _ := ch.(map[string]any)
		name, _ := channel["name"].(string)
		csv, _ := channel["currentCSV"].(string)

		// Skip lifecycle channels that aren't for nightly dev use
		if strings.HasPrefix(name, "eus-") || strings.HasPrefix(name, "support-required-upgrade") {
			continue
		}

		parsed, ok := registry.ParseCSVVersion(csv)
		if !ok {
			slog.Debug("skipping channel with unparseable CSV", "channel", name, "csv", csv)
			continue
		}

		// If we know the target version, only consider channels with matching major.minor
		if hasTarget && (parsed.Major != targetMajor || parsed.Minor != targetMinor) {
			continue
		}

		if hasRelease && parsed.EA != targetRelease.EA {
			continue
		}

		cmp := 0
		if bestParsed != nil {
			cmp = registry.CompareTags(parsed, *bestParsed)
		}

		if bestParsed == nil || cmp > 0 || (cmp == 0 && channelPriority(name, parsed.EA) > channelPriority(bestChannel, bestParsed.EA)) {
			bestParsed = &parsed
			bestChannel = name
		}
	}

	if bestChannel == "" {
		slog.Warn("detectNightlyChannel: no matching channels found", "targetMajor", targetMajor, "targetMinor", targetMinor)
	} else {
		slog.Info("detected nightly channel", "channel", bestChannel, "csv", bestParsed.Raw)
	}

	return bestChannel, nil
}

// channelPriority returns a numeric priority for tiebreaking when two channels
// have the same version. Higher = preferred.
//
// For GA releases (ea < 0): stable-M.x > stable-M.N > beta > everything else.
// For EA releases (ea >= 0): beta > stable-M.x > stable-M.N > everything else.
func channelPriority(name string, ea int) int {
	isEA := ea >= 0
	switch {
	case name == "beta":
		if isEA {
			return 40
		}
		return 20
	case strings.HasPrefix(name, "stable-") && strings.HasSuffix(name, ".x"):
		if isEA {
			return 30
		}
		return 40
	case strings.HasPrefix(name, "stable-"):
		if isEA {
			return 25
		}
		return 35
	case strings.HasPrefix(name, "fast"):
		return 10
	default:
		return 0
	}
}

// SourceSpec returns the nightly CatalogSource for an FBC image.
func SourceSpec(image string) map[string]any {
	return map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "CatalogSource",
		"metadata": map[string]any{
			"name":      rhoai.CatalogName,
			"namespace": rhoai.CatalogNS,
		},
		"spec": map[string]any{
			"sourceType":  "grpc",
			"image":       image,
			"displayName": "RHOAI Development Catalog",
			"publisher":   "RHOAI DevOps",
			"grpcPodConfig": map[string]any{
				"securityContextConfig": "restricted",
			},
		},
	}
}
