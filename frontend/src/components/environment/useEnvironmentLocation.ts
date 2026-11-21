import { useCallback, useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ENVIRONMENT_TABS, OLD_TABS, type EnvironmentTab } from "./environmentTabs";

/**
 * The Environment page's view in the URL: the tab (?tab=), the project
 * (?project=) and the Models tab's model (?model=), so links, reloads and
 * back and forward keep it. A link to the earlier layout's tabs opens
 * Pipelines. setFocusTarget moves the focus to an element once the view
 * that has it shows.
 */
export function useEnvironmentLocation() {
  const [searchParams, setSearchParams] = useSearchParams();
  const tabParam = searchParams.get("tab") ?? "";
  const tab: EnvironmentTab = (ENVIRONMENT_TABS as readonly string[]).includes(tabParam) ? (tabParam as EnvironmentTab) : OLD_TABS.includes(tabParam) ? "pipelines" : "overview";
  const urlProject = searchParams.get("project") || null;
  // project: a name; "" an explicit clear (the URL keeps an empty project=,
  // so the default does not come back); null drops it (the default applies).
  const navigate = useCallback((next: { tab?: EnvironmentTab; project?: string | null }, replace = false) => {
    setSearchParams((params) => {
      const p = new URLSearchParams(params);
      if (next.tab !== undefined) {
        if (next.tab === "overview") p.delete("tab");
        else p.set("tab", next.tab);
      }
      if (next.project !== undefined) {
        if (next.project === null) p.delete("project");
        else p.set("project", next.project);
      }
      return p;
    }, { replace });
  }, [setSearchParams]);
  // A link to the earlier layout's tabs opens Pipelines, replacing the old address.
  useEffect(() => {
    if (OLD_TABS.includes(tabParam)) navigate({ tab: "pipelines" }, true);
  }, [tabParam, navigate]);

  // "Show issue" moves the focus to the part (or a field) once its tab shows.
  const [focusTarget, setFocusTarget] = useState<string | null>(null);
  useEffect(() => {
    if (!focusTarget) return;
    const el = document.getElementById(focusTarget);
    if (el) {
      el.focus();
      el.scrollIntoView?.({ block: "center" });
      setFocusTarget(null);
    }
  }, [focusTarget, tabParam, urlProject]);

  const chooseModel = (name: string) => setSearchParams((params) => {
    const next = new URLSearchParams(params);
    next.set("model", name);
    return next;
  });

  return { searchParams, tab, urlProject, navigate, setFocusTarget, chooseModel };
}
