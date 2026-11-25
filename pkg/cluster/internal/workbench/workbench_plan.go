package workbench

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// ConnectionSecret names the dashboard S3 Connection object a project's
// pipeline server exposes to the tool.
const ConnectionSecret = storage.ConnectionSecretName

// Annotation keys on ImageStreams match what the dashboard consumes (odh-dashboard
// rhoai-3.6 frontend/src/types.ts ImageStreamAnnotation, ImageStreamSpecTagAnnotation).
const (
	imageNameAnnotation    = "opendatahub.io/notebook-image-name"
	imageOrderAnnotation   = "opendatahub.io/notebook-image-order"
	imageTierAnnotation    = "opendatahub.io/notebook-tier"
	imageHiddenAnnotation  = "opendatahub.io/notebook-image-hidden"
	imageDeprecated        = "opendatahub.io/deprecated"
	tagOutdatedAnnotation  = "opendatahub.io/image-tag-outdated"
	tagRecommended         = "opendatahub.io/workbench-image-recommended"
	tagDefaultImage        = "opendatahub.io/default-image"
	tagBuildCommit         = "opendatahub.io/notebook-build-commit"
	notebookImageLabel     = "opendatahub.io/notebook-image"
	byonCreatedByLabel     = "app.kubernetes.io/created-by"
	profileVisibility      = "opendatahub.io/dashboard-feature-visibility"
	profileDisabled        = "opendatahub.io/disabled"
	profileDisplayName     = "opendatahub.io/display-name"
	storageClassConfig     = "opendatahub.io/sc-config"
	defaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"
	// KueueProjectLabel is present when Kueue should admit that project's workloads
	// (packages/k8s-core k8sTypes.ts KnownLabels.KUEUE_MANAGED).
	KueueProjectLabel = envkit.KueueProjectLabel
	// DefaultStorageSize is what the dashboard uses when notebookController.pvcSize
	// is absent (storage/useDefaultPvcSize.ts).
	DefaultStorageSize = "20Gi"
)

// ImageStream holds the ImageStream fields image selection consults.
type ImageStream struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		Tags []struct {
			Name        string            `json:"name"`
			Annotations map[string]string `json:"annotations"`
		} `json:"tags"`
	} `json:"spec"`
	Status struct {
		DockerImageRepository string `json:"dockerImageRepository"`
		Tags                  []struct {
			Tag   string `json:"tag"`
			Items []any  `json:"items"`
		} `json:"tags"`
	} `json:"status"`
}

func (s ImageStream) byon() bool { return s.Metadata.Labels[byonCreatedByLabel] == "byon" }

// deprecated is the dashboard's isImageStreamDeprecated (odh-dashboard
// rhoai-3.7-ea.1 utilities/imageStreamUtils.ts L98-99): the stream's
// opendatahub.io/deprecated is "true". The dashboard labels such a stream
// Deprecated (ImageStreamSelector.tsx, NotebookImageDisplayName.tsx
// L99-101, L155-166): it gets no new versions.
func (s ImageStream) deprecated() bool { return s.Metadata.Annotations[imageDeprecated] == "true" }

// selectable applies the same rules as the dashboard image selector (ImageSelectorField.tsx
// L89-92, spawnerUtils.ts isInvalidBYONImageStream, isHiddenOOTBImageStream).
func (s ImageStream) selectable() bool {
	if s.byon() {
		if len(s.Spec.Tags) == 0 {
			return false
		}
		for _, st := range s.Status.Tags {
			if st.Tag == s.Spec.Tags[0].Name {
				return st.Items != nil
			}
		}
		return false
	}
	return s.Metadata.Annotations[imageHiddenAnnotation] != "true"
}

// usableTags implements getExistingVersionsForImageStream: every spec tag that is
// not outdated and appears in status, ordered via compareTagVersions (recommended
// tags first, otherwise newest by version).
func (s ImageStream) usableTags() []int {
	var out []int
	for i, t := range s.Spec.Tags {
		if t.Annotations[tagOutdatedAnnotation] != "" {
			continue
		}
		if slices.ContainsFunc(s.Status.Tags, func(st struct {
			Tag   string `json:"tag"`
			Items []any  `json:"items"`
		}) bool {
			return st.Tag == t.Name
		}) {
			out = append(out, i)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		ta, tb := s.Spec.Tags[out[a]], s.Spec.Tags[out[b]]
		ra, rb := ta.Annotations[tagRecommended] == "true", tb.Annotations[tagRecommended] == "true"
		if ra != rb {
			return ra
		}
		va, oka := parseVersion(ta.Name)
		vb, okb := parseVersion(tb.Name)
		if oka && okb {
			return compareVersions(va, vb) > 0
		}
		return ta.Name > tb.Name
	})
	return out
}

// parseVersion splits tags like "2025.2" or "v1.2.3" into numeric segments
// (compare-versions validate: at most four dot-separated parts, optional v prefix).
func parseVersion(s string) ([]int, bool) {
	s = strings.TrimPrefix(s, "v")
	core, _, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return nil, false
	}
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || p == "" {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

var tierOrder = map[string]int{"secure": 0, "community": 1, "custom": 2}

func (s ImageStream) tier() string {
	if t := strings.ToLower(s.Metadata.Annotations[imageTierAnnotation]); t != "" {
		return t
	}
	return "custom"
}

func (s ImageStream) order() int {
	n, err := strconv.Atoi(s.Metadata.Annotations[imageOrderAnnotation])
	if err != nil {
		return 100
	}
	return n
}

// lessForSelector sorts streams the way the image selector does (imageTierUtils.ts
// compareImageStreamTier, then notebook-image-order).
func lessForSelector(a, b ImageStream) bool {
	ta, tb := a.tier(), b.tier()
	ra, oka := tierOrder[ta]
	rb, okb := tierOrder[tb]
	if !oka {
		ra = len(tierOrder)
	}
	if !okb {
		rb = len(tierOrder)
	}
	if ra != rb {
		return ra < rb
	}
	if ta != tb {
		return ta < tb
	}
	return a.order() < b.order()
}

// ChooseImage decides the workbench image. The dashboard's Create workbench
// form preselects none (odh-dashboard rhoai-3.7-ea.1 SpawnerPage.tsx
// L111-114: it starts with no image; the user picks one), and it never reads
// opendatahub.io/default-image, so this tool decides, preferring what the
// dashboard treats as current. It chooses in passes, each over the selectable
// streams with a usable tag: (1) RHOAI's own (out-of-the-box) streams that
// are not deprecated, (2) custom (BYON) streams that are not deprecated,
// (3) deprecated streams (Deprecated is then set). Within a pass, a usable
// tag marked opendatahub.io/default-image wins (RHOAI marks its default
// image so, e.g. "Jupyter | Minimal | CPU" on 3.6), else the first stream in
// the selector's order (tier, then order) with the dashboard's default
// version (recommended when set, else newest). A custom stream without a
// tier sorts as "custom", before RHOAI's "Red Hat-provided" tier, which the
// selector does not rank, so the passes keep RHOAI's images first. ok is
// false if every stream lacks a usable tag.
func ChooseImage(streams []ImageStream) (ImageChoice, bool) {
	var candidates []ImageStream
	for _, s := range streams {
		if s.selectable() && len(s.usableTags()) > 0 {
			candidates = append(candidates, s)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return lessForSelector(candidates[i], candidates[j]) })
	choose := func(s ImageStream, tag int) ImageChoice {
		t := s.Spec.Tags[tag]
		return ImageChoice{
			Stream: s.Metadata.Name, Namespace: s.Metadata.Namespace, Tag: t.Name,
			Repository:  s.Status.DockerImageRepository,
			DisplayName: kube.FirstNonEmpty(s.Metadata.Annotations[imageNameAnnotation], s.Metadata.Name),
			BuildCommit: t.Annotations[tagBuildCommit],
			Deprecated:  s.deprecated(),
		}
	}
	passes := []func(ImageStream) bool{
		func(s ImageStream) bool { return !s.byon() && !s.deprecated() },
		func(s ImageStream) bool { return s.byon() && !s.deprecated() },
		ImageStream.deprecated,
	}
	for _, in := range passes {
		var pass []ImageStream
		for _, s := range candidates {
			if in(s) {
				pass = append(pass, s)
			}
		}
		for _, s := range pass {
			for _, i := range s.usableTags() {
				if s.Spec.Tags[i].Annotations[tagDefaultImage] == "true" {
					return choose(s, i), true
				}
			}
		}
		if len(pass) > 0 {
			return choose(pass[0], pass[0].usableTags()[0]), true
		}
	}
	return ImageChoice{}, false
}

// HardwareProfile is the infrastructure.opendatahub.io/v1 HardwareProfile
// subset profile selection needs.
type HardwareProfile struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		Identifiers []Identifier `json:"identifiers"`
		Scheduling  *struct {
			Type  string `json:"type"`
			Kueue *struct {
				LocalQueueName string `json:"localQueueName"`
			} `json:"kueue"`
		} `json:"scheduling"`
		DRA json.RawMessage `json:"dra"`
	} `json:"spec"`
}

func (p HardwareProfile) dra() bool {
	return len(p.Spec.DRA) > 0 && string(p.Spec.DRA) != "null"
}

// valid mirrors isHardwareProfileValid for picking: cpu and memory identifiers must
// both exist (validationUtils.ts hasCPUandMemory).
func (p HardwareProfile) valid() bool {
	var cpu, mem bool
	for _, id := range p.Spec.Identifiers {
		if !id.valid() {
			return false
		}
		cpu = cpu || id.ResourceType == "CPU" || id.Identifier == "cpu"
		mem = mem || id.ResourceType == "Memory" || id.Identifier == "memory"
	}
	return cpu && mem
}

// Identifier is one resource of a hardware profile.
type Identifier struct {
	Identifier   string `json:"identifier"`
	DisplayName  string `json:"displayName"`
	ResourceType string `json:"resourceType"`
	DefaultCount any    `json:"defaultCount"`
	MinCount     any    `json:"minCount"`
	MaxCount     any    `json:"maxCount"`
}

// valid ports the dashboard's identifierSchema (packages/hardware-profiles
// src/pages/manage/validationUtils.ts L13-133): a display name and an
// identifier, a default and a minimum count, none negative, each a whole
// number in its unit, and minimum <= default <= maximum.
func (id Identifier) valid() bool {
	if strings.TrimSpace(id.DisplayName) == "" || strings.TrimSpace(id.Identifier) == "" || id.DefaultCount == nil || id.MinCount == nil {
		return false
	}
	memory := id.ResourceType == "Memory" || id.Identifier == "memory"
	cpu := id.ResourceType == "CPU" || id.Identifier == "cpu"
	parse := func(v any) (float64, bool) {
		s := ResourceText(v)
		if strings.HasPrefix(strings.TrimSpace(s), "-") {
			return 0, false
		}
		n, num, ok := parseCount(s, cpu, memory)
		return n, ok && num == float64(int64(num))
	}
	lo, okLo := parse(id.MinCount)
	def, okDef := parse(id.DefaultCount)
	if !okLo || !okDef || def < lo {
		return false
	}
	if id.MaxCount != nil {
		hi, okHi := parse(id.MaxCount)
		if !okHi || def > hi || lo > hi {
			return false
		}
	}
	return true
}

var memoryUnits = map[string]float64{"": 1, "Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "K": 1e3, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15}

// parseCount splits a count into its number and unit as the dashboard's
// splitValueUnit does: the value in base units (bytes, cores) and the
// number as written (which must be whole).
func parseCount(s string, cpu, memory bool) (value, number float64, ok bool) {
	s = strings.TrimSpace(s)
	end := len(s)
	for end > 0 && (s[end-1] < '0' || s[end-1] > '9') && s[end-1] != '.' {
		end--
	}
	num, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0, 0, false
	}
	unit := s[end:]
	switch {
	case memory:
		m, known := memoryUnits[unit]
		return num * m, num, known
	case cpu:
		switch unit {
		case "":
			return num, num, true
		case "m":
			return num / 1000, num, true
		}
		return 0, 0, false
	}
	return num, num, unit == ""
}

// visibleForWorkbenches matches isHardwareProfileVisibleIn(profile, ["workbench"]):
// missing annotation, invalid JSON, or features that omit every known name means
// the profile shows everywhere.
func (p HardwareProfile) visibleForWorkbenches() bool {
	raw := p.Metadata.Annotations[profileVisibility]
	if raw == "" {
		return true
	}
	var in []string
	if json.Unmarshal([]byte(raw), &in) != nil {
		return true
	}
	known := false
	for _, v := range in {
		if v == "workbench" {
			return true
		}
		known = known || v == "model-serving"
	}
	return !known
}

func (p HardwareProfile) enabled() bool {
	v, ok := p.Metadata.Annotations[profileDisabled]
	return !ok || v == "false"
}

func (p HardwareProfile) queue() string {
	if s := p.Spec.Scheduling; s != nil && s.Type == "Queue" {
		if s.Kueue != nil {
			return kube.FirstNonEmpty(s.Kueue.LocalQueueName, "")
		}
	}
	return ""
}

func (p HardwareProfile) isQueue() bool {
	return p.Spec.Scheduling != nil && p.Spec.Scheduling.Type == "Queue"
}

// KueueMode is the dashboard's Kueue rule (envkit.KueueMode), shared with
// the Models tab's profile pick.
type KueueMode = envkit.KueueMode

const (
	OnlyNonKueue = envkit.OnlyNonKueue
	OnlyKueue    = envkit.OnlyKueue
	NoProfiles   = envkit.NoProfiles
)

// ModeFor is envkit.KueueModeFor.
func ModeFor(projectKueue, kueueAvailable bool) KueueMode {
	return envkit.KueueModeFor(projectKueue, kueueAvailable)
}

// ChooseProfile repeats the dashboard new-workbench default (useHardwareProfileConfig.ts
// L307-326): walk global profiles (applications namespace) then project-scoped ones—valid,
// workbench-visible, non-DRA; take the first enabled profile filterProfilesByKueue keeps
// (kueueUtils.ts L99-128), mapping defaults to requests and limits (shared/utils.ts
// getContainerResourcesFromHardwareProfile). Under OnlyKueue, drop Queue profiles tied to
// a LocalQueue this project does not have. When nothing matches, the string explains why.
func ChooseProfile(global, project []HardwareProfile, mode KueueMode, queues []string) (ProfileChoice, string) {
	if mode == NoProfiles {
		return ProfileChoice{}, "The project uses Kueue (label kueue.openshift.io/managed=true), but Kueue is not available on this cluster, so the dashboard offers no hardware profile for a workbench there."
	}
	all := append(append([]HardwareProfile{}, global...), project...)
	var seen int
	for _, p := range all {
		if p.dra() || !p.valid() || !p.visibleForWorkbenches() || !p.enabled() {
			continue
		}
		seen++
		switch mode {
		case OnlyKueue:
			if !p.isQueue() {
				continue
			}
			if q := p.queue(); q != "" && !slices.Contains(queues, q) {
				continue
			}
		case OnlyNonKueue:
			if p.isQueue() {
				continue
			}
		}
		choice := ProfileChoice{
			Name: p.Metadata.Name, Namespace: p.Metadata.Namespace, ResourceVersion: p.Metadata.ResourceVersion,
			DisplayName: kube.FirstNonEmpty(p.Metadata.Annotations[profileDisplayName], p.Metadata.Name),
			Resources:   map[string]any{}, Queue: p.queue(),
		}
		for _, id := range p.Spec.Identifiers {
			choice.Resources[id.Identifier] = id.DefaultCount
			choice.Order = append(choice.Order, id.Identifier)
		}
		return choice, ""
	}
	switch {
	case seen == 0:
		return ProfileChoice{}, "No enabled hardware profile is offered for workbenches. A cluster-admin enables one in the dashboard (Settings, Hardware profiles)."
	case mode == OnlyKueue:
		return ProfileChoice{}, "The project uses Kueue (label kueue.openshift.io/managed=true), and no enabled workbench hardware profile sends workloads to a LocalQueue the project has. A cluster-admin adds a Queue profile for one of its LocalQueues, or the project's LocalQueue."
	default:
		return ProfileChoice{}, "Every enabled workbench hardware profile uses Kueue, and the project does not (label kueue.openshift.io/managed=true), so the dashboard offers none there."
	}
}

// ResourceText renders a defaultCount for human-readable output ("2", "4Gi").
func ResourceText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// StorageClass captures the StorageClass fields storage class selection uses.
type StorageClass struct {
	Metadata kube.ObjectMeta `json:"metadata"`
}

// ChooseStorageClass mirrors the dashboard workbench storage default
// (storage/useDefaultStorageClass.ts L104-158, without fallbackToFirst):
// start with the admin-toggled dashboard default (opendatahub.io/sc-config
// isEnabled and isDefault); if the cluster lacks a default class, use
// notebookController.storageClassName when that StorageClass exists; otherwise
// the first cluster default; return "" when nothing fits and let the API server decide.
func ChooseStorageClass(classes []StorageClass, configured string) string {
	var defaults []string
	exists := false
	for _, c := range classes {
		var cfg struct {
			IsEnabled bool `json:"isEnabled"`
			IsDefault bool `json:"isDefault"`
		}
		if json.Unmarshal([]byte(c.Metadata.Annotations[storageClassConfig]), &cfg) == nil && cfg.IsEnabled && cfg.IsDefault {
			return c.Metadata.Name
		}
		if c.Metadata.Annotations[defaultClassAnnotation] == "true" {
			defaults = append(defaults, c.Metadata.Name)
		}
		exists = exists || (configured != "" && c.Metadata.Name == configured)
	}
	if len(defaults) == 0 && exists {
		return configured
	}
	if len(defaults) > 0 {
		return defaults[0]
	}
	return ""
}
