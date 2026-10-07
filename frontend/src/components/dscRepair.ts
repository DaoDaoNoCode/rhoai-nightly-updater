import type { DSCCompatibility, DSCRemovalBlock } from "../types";

export type DSCRepairMode = "remove-invalid" | "remove-extra-components" | "reset-defaults";

export interface RepairPreview {
  /**
   * Enabled components the repair would set to Removed (reset) or whose entry it drops (remove extra). For
   * DataScienceCluster v3 a reset lists parts as "<component>.<part>" (dashboard.standard).
   */
  removals: string[];
  /** Removals that must not happen now. The backend refuses the whole repair while any is listed. */
  blocked: DSCRemovalBlock[];
}

/**
 * What a DSC repair would remove, and what blocks it (GET /api/components
 * dscCompatibility.resetRemovals / removalBlocks). "reset-defaults" is
 * refused while any reset removal is blocked; "remove-extra-components" while
 * any enabled extra component is (the backend computes blocks only for
 * enabled components). "remove-invalid" removes no component.
 */
export function repairPreview(compat: DSCCompatibility | undefined, mode: DSCRepairMode): RepairPreview {
  if (!compat || mode === "remove-invalid") return { removals: [], blocked: [] };
  const blocks = compat.removalBlocks ?? [];
  const removals = mode === "reset-defaults" ? (compat.resetRemovals ?? []) : (compat.extraComponents ?? []);
  // DataScienceCluster v3 reports parts as "<component>.<part>" (kserve.nim);
  // dropping an extra component drops its parts too.
  const covers = (removal: string, block: string) => block === removal || block.startsWith(`${removal}.`);
  return { removals, blocked: blocks.filter((b) => removals.some((r) => covers(r, b.component))) };
}
