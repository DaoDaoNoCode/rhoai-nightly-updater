import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  Button,
  Content,
  EmptyState,
  EmptyStateActions,
  EmptyStateBody,
  EmptyStateFooter,
  Page,
  PageSection,
  PageSidebar,
  PageSidebarBody,
  PageToggleButton,
  Masthead,
  MastheadMain,
  MastheadBrand,
  MastheadLogo,
  MastheadContent,
  MastheadToggle,
  Nav,
  NavItem,
  NavList,
  Dropdown,
  DropdownItem,
  DropdownList,
  Label,
  MenuToggle,
  Spinner,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
  ToolbarGroup,
} from "@patternfly/react-core";
import ThIcon from "@patternfly/react-icons/dist/esm/icons/th-icon";
import ExternalLinkAltIcon from "@patternfly/react-icons/dist/esm/icons/external-link-alt-icon";
import SunIcon from "@patternfly/react-icons/dist/esm/icons/sun-icon";
import MoonIcon from "@patternfly/react-icons/dist/esm/icons/moon-icon";
import {
  BrowserRouter,
  Routes,
  Route,
  useLocation,
  useNavigate,
} from "react-router-dom";
import type { StatusResponse } from "./types";
import { getStatus, getUserPermissions, trackPageView } from "./services/api";
import BarsIcon from "@patternfly/react-icons/dist/esm/icons/bars-icon";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { HelpButton } from "./components/HelpModal";
import { StatusPage } from "./pages/StatusPage";
import { ComponentsPage } from "./pages/ComponentsPage";
import { TroubleshootingPage } from "./pages/TroubleshootingPage";
import { BuildExplorerPage } from "./pages/BuildExplorerPage";
import { DashboardDevPage } from "./pages/DashboardDevPage";
import {
  RECONCILE_POLL_MS,
  RECONCILE_TIMEOUT_MS,
  RECONCILE_POLL_FAST_MS,
  RECONCILE_POLL_MEDIUM_MS,
  RECONCILE_POLL_SLOW_MS,
  RECONCILE_POLL_FAST_UNTIL_MS,
  RECONCILE_POLL_MEDIUM_UNTIL_MS,
  BACKGROUND_POLL_MS,
  NAV_ITEMS,
} from "./constants";

import SearchIcon from "@patternfly/react-icons/dist/esm/icons/search-icon";

const NotFoundPage: React.FC = () => {
  const navigate = useNavigate();
  return (
    <PageSection isFilled>
      <EmptyState headingLevel="h1" icon={SearchIcon} titleText="404 — Page not found" variant="full">
        <EmptyStateBody>
          The page you're looking for doesn't exist or has been moved.
        </EmptyStateBody>
        <EmptyStateFooter>
          <EmptyStateActions>
            <Button variant="primary" onClick={() => navigate("/")}>Go to Dashboard</Button>
          </EmptyStateActions>
          <EmptyStateActions>
            <Button variant="link" onClick={() => navigate("/diagnostics")}>Open Diagnostics</Button>
          </EmptyStateActions>
        </EmptyStateFooter>
      </EmptyState>
    </PageSection>
  );
};

export type OperationPhase = "idle" | "streaming" | "reconciling" | "complete";

/** Compute the adaptive poll interval based on how long we've been actively polling. */
function getAdaptivePollInterval(activeTimeMs: number): number {
  if (activeTimeMs < RECONCILE_POLL_FAST_UNTIL_MS) return RECONCILE_POLL_FAST_MS;
  if (activeTimeMs < RECONCILE_POLL_MEDIUM_UNTIL_MS) return RECONCILE_POLL_MEDIUM_MS;
  return RECONCILE_POLL_SLOW_MS;
}

const SESSION_KEY_RECONCILING = "rhoai-reconciling";
const SESSION_KEY_RECONCILE_START = "rhoai-reconcile-start";

/** Restore reconciliation state from sessionStorage (survives page refresh). */
function restoreReconcileState(): { reconciling: boolean; startTime: number } {
  try {
    const isReconciling = sessionStorage.getItem(SESSION_KEY_RECONCILING) === "true";
    const startTime = Number(sessionStorage.getItem(SESSION_KEY_RECONCILE_START) || "0");
    if (isReconciling && startTime > 0) {
      if (Date.now() - startTime < RECONCILE_TIMEOUT_MS) {
        return { reconciling: true, startTime };
      }
      sessionStorage.removeItem(SESSION_KEY_RECONCILING);
      sessionStorage.removeItem(SESSION_KEY_RECONCILE_START);
    }
  } catch {
    // sessionStorage may be disabled
  }
  return { reconciling: false, startTime: 0 };
}

function persistReconcileState(isReconciling: boolean, startTime: number): void {
  try {
    if (isReconciling) {
      sessionStorage.setItem(SESSION_KEY_RECONCILING, "true");
      sessionStorage.setItem(SESSION_KEY_RECONCILE_START, String(startTime));
    } else {
      sessionStorage.removeItem(SESSION_KEY_RECONCILING);
      sessionStorage.removeItem(SESSION_KEY_RECONCILE_START);
    }
  } catch {
    // sessionStorage may be disabled
  }
}

const AppLayout: React.FC = () => {
  const restoredState = restoreReconcileState();
  const [status, setStatus] = useState<StatusResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [reconciling, setReconciling] = useState(restoredState.reconciling);
  const [reconcileStartTime, setReconcileStartTime] = useState(restoredState.startTime);
  const [setupOpen, setSetupOpen] = useState(false);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);
  const [userMenuOpen, setUserMenuOpen] = useState(false);
  const [appLauncherOpen, setAppLauncherOpen] = useState(false);
  const [canMutate, setCanMutate] = useState(true); // default true until checked
  const [isDark, setIsDark] = useState(() => {
    try { return localStorage.getItem('pf-theme') === 'dark'; } catch { return false; }
  });
  const [reconcileTimedOut, setReconcileTimedOut] = useState(false);
  const [operationPhase, setOperationPhase] = useState<OperationPhase>("idle");

  const reconcilingRef = useRef(restoredState.reconciling);
  const reconcileStartRef = useRef<number>(restoredState.startTime);
  const activePollingTimeRef = useRef<number>(0);
  const lastPollTimestampRef = useRef<number>(0);
  const operationPhaseRef = useRef<OperationPhase>("idle");
  const reconcilePhaseStartRef = useRef<number>(0);

  const location = useLocation();
  const navigate = useNavigate();

  // Track page views (aggregate only, no user identity)
  useEffect(() => {
    const pageMap: Record<string, string> = {
      "/": "dashboard",
      "/components": "components",
      "/builds": "build_explorer",
      "/dashboard-dev": "dashboard_dev",
      "/diagnostics": "diagnostics",
    };
    const page = pageMap[location.pathname];
    if (page) trackPageView(page);
  }, [location.pathname]);

  // Fetch user permissions once on mount
  useEffect(() => {
    getUserPermissions()
      .then((p) => setCanMutate(p.canMutate))
      .catch((err) => {
        // For auth errors (401/403), fail closed — the user likely lacks permissions
        const msg = err instanceof Error ? err.message.toLowerCase() : "";
        if (msg.includes("401") || msg.includes("403") || msg.includes("unauthorized") || msg.includes("forbidden")) {
          setCanMutate(false);
        }
        // For network/other errors, default to true (fail-open for UX;
        // backend mutation endpoints still enforce auth)
      });
  }, []);

  // Dark mode: toggle CSS class on <html> and persist preference
  useEffect(() => {
    const htmlEl = document.documentElement;
    if (isDark) {
      htmlEl.classList.add('pf-v6-theme-dark');
    } else {
      htmlEl.classList.remove('pf-v6-theme-dark');
    }
    try { localStorage.setItem('pf-theme', isDark ? 'dark' : 'light'); } catch {}
  }, [isDark]);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const s = await getStatus();
      setStatus(s);
      setLastRefreshed(new Date());

      if (reconcilingRef.current) {
        const phase = s.csv.phase;

        // Track active polling time (time spent while tab is visible)
        const now = Date.now();
        if (lastPollTimestampRef.current > 0) {
          const delta = now - lastPollTimestampRef.current;
          // Only count intervals that look like normal poll gaps (< 2x poll interval)
          // to avoid counting time when the tab was hidden
          if (delta < RECONCILE_POLL_MS * 2.5) {
            activePollingTimeRef.current += delta;
          }
        }
        lastPollTimestampRef.current = now;

        if (phase === "Succeeded" || phase === "Failed") {
          // Fire browser notification when reconciliation finishes
          if (Notification.permission === "granted") {
            new Notification("RHOAI Nightly Updater", {
              body:
                phase === "Succeeded"
                  ? "Operator update completed successfully"
                  : "Operator update failed",
            });
          } else if (Notification.permission !== "denied") {
            Notification.requestPermission();
          }
          reconcilingRef.current = false;
          setReconciling(false);
          persistReconcileState(false, 0);
          setOperationPhase("complete");
          operationPhaseRef.current = "complete";
        } else if (activePollingTimeRef.current >= RECONCILE_TIMEOUT_MS) {
          // Timeout reached without success or failure — inform the user
          reconcilingRef.current = false;
          setReconciling(false);
          persistReconcileState(false, 0);
          setReconcileTimedOut(true);
          setOperationPhase("idle");
          operationPhaseRef.current = "idle";
        }
      }
    } catch (e) {
      const msg = e instanceof Error ? e.message : "Failed to fetch status";
      setError(msg);
    } finally {
      setLoading(false);
    }
  }, []);

  const startReconciling = useCallback(() => {
    const now = Date.now();
    reconcilingRef.current = true;
    reconcileStartRef.current = now;
    activePollingTimeRef.current = 0;
    lastPollTimestampRef.current = 0;
    setReconcileStartTime(now);
    setReconciling(true);
    setReconcileTimedOut(false);
    persistReconcileState(true, now);
  }, []);

  const handleMutationComplete = useCallback(() => {
    startReconciling();
    refresh();
  }, [startReconciling, refresh]);

  // Stream lifecycle handlers — used by UpdatePanel via StatusPage
  const handleStreamStart = useCallback(() => {
    setOperationPhase("streaming");
    operationPhaseRef.current = "streaming";
  }, []);

  const handleStreamEnd = useCallback(
    (success: boolean) => {
      if (success) {
        setOperationPhase("reconciling");
        operationPhaseRef.current = "reconciling";
        reconcilePhaseStartRef.current = Date.now();
        startReconciling();
        refresh();
      } else {
        setOperationPhase("idle");
        operationPhaseRef.current = "idle";
      }
    },
    [startReconciling, refresh],
  );

  const handleReconcileComplete = useCallback(() => {
    setOperationPhase("complete");
    operationPhaseRef.current = "complete";
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // Reconcile polling with adaptive interval (fast -> medium -> slow based on active polling time)
  // When streaming, SSE provides live data so we skip polling entirely.
  useEffect(() => {
    if (!reconciling) return;

    let timerId: ReturnType<typeof setTimeout>;

    const scheduleNext = () => {
      const interval = getAdaptivePollInterval(activePollingTimeRef.current);
      timerId = setTimeout(() => {
        if (document.hidden) {
          scheduleNext();
          return;
        }
        refresh();
        if (reconcilingRef.current) {
          scheduleNext();
        }
      }, interval);
    };

    scheduleNext();

    return () => clearTimeout(timerId);
  }, [reconciling, refresh]);

  // Background polling (every 60s for general freshness)
  // Paused during streaming (SSE provides live data) and during reconcile (has its own poll)
  useEffect(() => {
    const id = setInterval(() => {
      if (document.hidden) return;
      if (reconcilingRef.current) return;
      if (operationPhaseRef.current === "streaming") return;
      refresh();
    }, BACKGROUND_POLL_MS);

    return () => clearInterval(id);
  }, [refresh]);

  const header = (
    <Masthead>
      <MastheadMain>
        <MastheadToggle>
          <PageToggleButton
            variant="plain"
            aria-label="Global navigation"
          >
            <BarsIcon />
          </PageToggleButton>
        </MastheadToggle>
        <MastheadBrand data-codemods>
          <MastheadLogo>
            <img src="data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAyNCAyNCIgZmlsbD0iI0VFMDAwMCI+PHBhdGggZD0iTTEyIDJMMyA3djEwbDkgNSA5LTVWN2wtOS01em0wIDIuMThMMTggNy4yN3Y3LjQ2TDEyIDE5LjgyIDYgMTQuNzNWNy4yN0wxMiA0LjE4eiIvPjwvc3ZnPg==" alt="RHOAI" height="38" />
            <span style={{ display: "inline-flex", flexDirection: "column", lineHeight: 1.2, marginLeft: "8px" }}>
              <strong style={{ fontSize: "var(--pf-t--global--font--size--body--default)" }}>RHOAI</strong>
              <span style={{ fontSize: "var(--pf-t--global--font--size--body--default)", fontWeight: "var(--pf-t--global--font--weight--body--default)" }}>Nightly Updater</span>
            </span>
          </MastheadLogo>
        </MastheadBrand>
      </MastheadMain>
      <MastheadContent>
        <Toolbar isStatic>
          <ToolbarContent>
            <ToolbarGroup align={{ default: "alignEnd" }}>
              <ToolbarItem>
                <Button
                  variant="plain"
                  aria-label="Toggle dark mode"
                  onClick={() => setIsDark((prev) => !prev)}
                >
                  {isDark ? <SunIcon /> : <MoonIcon />}
                </Button>
              </ToolbarItem>
              <ToolbarItem>
                <HelpButton />
              </ToolbarItem>
              {status ? (
                <>
                  <ToolbarItem>
                    <Dropdown
                      isOpen={appLauncherOpen}
                      onSelect={() => setAppLauncherOpen(false)}
                      onOpenChange={setAppLauncherOpen}
                      popperProps={{ position: "right" }}
                      toggle={(toggleRef) => (
                        <MenuToggle
                          ref={toggleRef}
                          onClick={() => setAppLauncherOpen(!appLauncherOpen)}
                          isExpanded={appLauncherOpen}
                          variant="plain"
                          aria-label="Applications"
                        >
                          <ThIcon />
                        </MenuToggle>
                      )}
                    >
                      <DropdownList>
                        {status.consoleURL && (
                          <DropdownItem
                            key="console"
                            onClick={() => window.open(status.consoleURL, "_blank")}
                            icon={<ExternalLinkAltIcon />}
                          >
                            OpenShift Console
                          </DropdownItem>
                        )}
                        {status.consoleURL && (
                          <DropdownItem
                            key="rhoai-dashboard"
                            onClick={() => {
                              const dashboardURL = status.consoleURL?.replace("console-openshift-console", "data-science-gateway");
                              window.open(dashboardURL, "_blank");
                            }}
                            icon={<ExternalLinkAltIcon />}
                          >
                            RHOAI Dashboard
                          </DropdownItem>
                        )}
                      </DropdownList>
                    </Dropdown>
                  </ToolbarItem>
                  {reconciling && (
                    <ToolbarItem>
                      <Label isCompact color="orange" icon={<Spinner size="sm" aria-label="Reconciling" />}>
                        Reconciling...
                      </Label>
                    </ToolbarItem>
                  )}
                  <ToolbarItem>
                    <Label isCompact color="blue">
                      OCP {status.cluster.version}
                    </Label>
                  </ToolbarItem>
                  <ToolbarItem>
                    <Dropdown
                      isOpen={userMenuOpen}
                      onSelect={() => setUserMenuOpen(false)}
                      onOpenChange={setUserMenuOpen}
                      toggle={(toggleRef) => (
                        <MenuToggle
                          ref={toggleRef}
                          onClick={() => setUserMenuOpen(!userMenuOpen)}
                          isExpanded={userMenuOpen}
                          variant="plainText"
                        >
                          {status.cluster.user}
                        </MenuToggle>
                      )}
                    >
                      <DropdownList>
                        <DropdownItem
                          key="logout"
                          onClick={() => { window.location.href = "/oauth/sign_in"; }}
                        >
                          Log out
                        </DropdownItem>
                      </DropdownList>
                    </Dropdown>
                  </ToolbarItem>
                </>
              ) : (
                <>
                  <ToolbarItem>
                    <Spinner size="sm" aria-label="Loading cluster info" />
                  </ToolbarItem>
                  <ToolbarItem>
                    <Label isCompact color="blue">Loading...</Label>
                  </ToolbarItem>
                </>
              )}
            </ToolbarGroup>
          </ToolbarContent>
        </Toolbar>
      </MastheadContent>
    </Masthead>
  );

  const sidebar = (
    <PageSidebar>
      <PageSidebarBody>
        <Nav>
          <NavList>
            {NAV_ITEMS.map((item) => (
              <NavItem
                key={item.path}
                isActive={location.pathname === item.path}
                onClick={() => navigate(item.path)}
              >
                {item.label}
              </NavItem>
            ))}
          </NavList>
        </Nav>
      </PageSidebarBody>
    </PageSidebar>
  );

  return (
    <Page masthead={header} sidebar={sidebar} isManagedSidebar>
      {reconcileTimedOut && (
        <Alert
          variant="warning"
          title="Reconciliation monitoring timed out"
          isInline
          actionClose={<AlertActionCloseButton onClose={() => setReconcileTimedOut(false)} />}
          style={{ margin: "var(--pf-t--global--spacer--md)" }}
        >
          Automatic status polling has stopped after 10 minutes of active monitoring. The operator may still be reconciling.
          Please check the cluster status manually or refresh the page.
        </Alert>
      )}
      <Routes>
        <Route
          path="/"
          element={
            <StatusPage
              status={status}
              loading={loading}
              error={error}
              reconciling={reconciling}
              reconcileStartTime={reconcileStartTime}
              reconcileTimedOut={reconcileTimedOut}
              lastRefreshed={lastRefreshed}
              setupOpen={setupOpen}
              setSetupOpen={setSetupOpen}
              refresh={refresh}
              handleMutationComplete={handleMutationComplete}
              canMutate={canMutate}
              operationPhase={operationPhase}
              onStreamStart={handleStreamStart}
              onStreamEnd={handleStreamEnd}
              onReconcileComplete={handleReconcileComplete}
            />
          }
        />
        <Route path="/components" element={<ComponentsPage />} />
        <Route path="/builds" element={<BuildExplorerPage />} />
        <Route path="/dashboard-dev" element={<DashboardDevPage canMutate={canMutate} />} />
        <Route path="/diagnostics" element={<TroubleshootingPage />} />
        <Route path="*" element={<NotFoundPage />} />
      </Routes>
    </Page>
  );
};

export const App: React.FC = () => {
  // If deployed under a sub-path, add basename to BrowserRouter (e.g. basename="/my-app").
  return (
    <BrowserRouter>
      <ErrorBoundary>
        <AppLayout />
      </ErrorBoundary>
    </BrowserRouter>
  );
};
