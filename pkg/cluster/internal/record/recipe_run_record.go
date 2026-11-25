package record

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// RecipeRunKey is the key of the operation ConfigMap that records the last
// environment recipe run (types.RecipeRun). It is written by a JSON merge
// patch of its own key, as dscHardResetKey, when a step starts and ends,
// never per progress event; the live progress is in the lease.
const RecipeRunKey = "recipeRun"

// MaxRecipeRunBytes bounds the recorded run, far below the ConfigMap's
// 1 MiB limit (the lease and the marker share it).
const MaxRecipeRunBytes = 32 << 10

// ErrRecipeRunReplaced: the record holds another run now (a newer run
// started, in this or another updater pod), so this run's record is not
// written over it.
var ErrRecipeRunReplaced = errors.New("the recipe run record holds another run, or another attempt of it")

// SaveRecipeRun records run. With replace the record is written whatever
// it holds (a run, or an attempt of one (Resume), that starts under the
// cluster lock replaces the last one); otherwise only while it holds this
// very attempt (the same run ID, operation ID and boot ID) or none, so a
// late write of an earlier attempt never overwrites the attempt that
// resumed it. The
// write is conditional on the ConfigMap's resourceVersion and retried on a
// conflict, like the lease, so it never overwrites a newer run another pod
// recorded in between. A run larger than MaxRecipeRunBytes has its step
// messages shortened until it fits.
func SaveRecipeRun(c *kube.Client, run *types.RecipeRun, replace bool) error {
	value, err := encodeRecipeRun(run)
	if err != nil {
		return err
	}
	err = operationWriter.do(c.Context(), func() error {
		return updateOperationRecord(c, func(rec *OperationRecord) (map[string]any, error) {
			current, err := recipeRunOf(rec)
			// A record that cannot be parsed is replaced by a run that
			// starts, and kept otherwise.
			if err != nil && !replace {
				return nil, err
			}
			if !replace && current != nil && (current.ID != run.ID || current.OperationID != run.OperationID || current.BootID != run.BootID) {
				return nil, ErrRecipeRunReplaced
			}
			return map[string]any{RecipeRunKey: value}, nil
		})
	})
	switch {
	case err == nil, errors.Is(err, ErrRecipeRunReplaced):
		return err
	}
	return fmt.Errorf("save the recipe run: %w", err)
}

// encodeRecipeRun is run as JSON of at most MaxRecipeRunBytes: step
// messages are shortened (to 1 KiB, then 256 bytes, then dropped) until it
// fits.
func encodeRecipeRun(run *types.RecipeRun) (string, error) {
	cp := *run
	cp.Steps = append([]types.RecipeRunStep(nil), run.Steps...)
	for _, limit := range []int{1 << 10, 256, 0} {
		for i := range cp.Steps {
			cp.Steps[i].Message = kube.TruncateText(cp.Steps[i].Message, limit)
		}
		cp.StopReason = kube.TruncateText(cp.StopReason, max(limit, 256))
		data, err := json.Marshal(cp)
		if err != nil {
			return "", err
		}
		if len(data) <= MaxRecipeRunBytes {
			return string(data), nil
		}
	}
	return "", fmt.Errorf("the recipe run record is larger than %d bytes", MaxRecipeRunBytes)
}

// ReadRecipeRun returns the recorded run, nil when none is recorded. A
// record that cannot be parsed is an error.
func ReadRecipeRun(c *kube.Client) (*types.RecipeRun, error) {
	rec, err := ReadOperationRecord(c)
	if err != nil {
		return nil, err
	}
	return recipeRunOf(rec)
}

// recipeRunOf is the recipe run of the operation record as read, nil when
// none is recorded.
func recipeRunOf(rec *OperationRecord) (*types.RecipeRun, error) {
	raw := rec.Value(RecipeRunKey)
	if raw == "" {
		return nil, nil
	}
	var run types.RecipeRun
	if err := json.Unmarshal([]byte(raw), &run); err != nil {
		return nil, fmt.Errorf("parse the recipe run record: %w", err)
	}
	return &run, nil
}
