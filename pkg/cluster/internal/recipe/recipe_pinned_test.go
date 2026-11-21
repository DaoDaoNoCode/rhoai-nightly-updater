package recipe

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A build pinned in the run record (which the ServiceAccount can write) is
// installed only when it is a digest-pinned build of the RHOAI catalog
// repository that the recipe's own build choice allows.
func TestPinnedBuildProblem(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	repo := "quay.io/rhoai/rhoai-fbc-fragment"
	for _, tc := range []struct {
		name   string
		build  types.RecipeBuild
		pinned types.RecipePinnedBuild
		ok     bool
	}{
		{"latest, a nightly line", types.RecipeBuild{Latest: true}, types.RecipePinnedBuild{Tag: "rhoai-3.6", Image: repo + ":rhoai-3.6@" + d, Digest: d}, true},
		{"tag, its line", types.RecipeBuild{Tag: "rhoai-3.6"}, types.RecipePinnedBuild{Tag: "rhoai-3.6", Image: repo + ":rhoai-3.6@" + d, Digest: d}, true},
		{"image, itself", types.RecipeBuild{Image: repo + "@" + d}, types.RecipePinnedBuild{Image: repo + "@" + d, Digest: d}, true},
		{"another repository", types.RecipeBuild{Latest: true}, types.RecipePinnedBuild{Tag: "rhoai-3.6", Image: "quay.io/attacker/fbc:rhoai-3.6@" + d, Digest: d}, false},
		{"another registry", types.RecipeBuild{Latest: true}, types.RecipePinnedBuild{Tag: "rhoai-3.6", Image: "evil.example.com/rhoai/rhoai-fbc-fragment:rhoai-3.6@" + d, Digest: d}, false},
		{"a tag, not a digest", types.RecipeBuild{Latest: true}, types.RecipePinnedBuild{Tag: "rhoai-3.6", Image: repo + ":rhoai-3.6", Digest: d}, false},
		{"digest mismatch", types.RecipeBuild{Latest: true}, types.RecipePinnedBuild{Tag: "rhoai-3.6", Image: repo + ":rhoai-3.6@" + d, Digest: other}, false},
		{"another line than the recipe's", types.RecipeBuild{Tag: "rhoai-3.6"}, types.RecipePinnedBuild{Tag: "rhoai-3.4", Image: repo + ":rhoai-3.4@" + d, Digest: d}, false},
		{"recorded tag differs from the image's", types.RecipeBuild{Tag: "rhoai-3.6"}, types.RecipePinnedBuild{Tag: "rhoai-3.6", Image: repo + ":rhoai-3.4@" + d, Digest: d}, false},
		{"another image than the recipe's", types.RecipeBuild{Image: repo + "@" + d}, types.RecipePinnedBuild{Image: repo + "@" + other, Digest: other}, false},
		{"latest, not a nightly line", types.RecipeBuild{Latest: true}, types.RecipePinnedBuild{Tag: "custom", Image: repo + ":custom@" + d, Digest: d}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.pinned
			got := PinnedBuildProblem(tc.build, &p)
			if (got == "") != tc.ok {
				t.Fatalf("problem %q, want ok=%v", got, tc.ok)
			}
		})
	}
}
