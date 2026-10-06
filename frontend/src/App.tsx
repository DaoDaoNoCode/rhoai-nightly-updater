import React, { Suspense, lazy, useEffect, useState } from "react";
import {
  Brand,
  Button,
  Divider,
  EmptyState,
  EmptyStateActions,
  EmptyStateBody,
  EmptyStateFooter,
  Flex,
  Page,
  PageSection,
  PageSidebar,
  PageSidebarBody,
  PageToggleButton,
  Masthead,
  MastheadMain,
  MastheadBrand,
  MastheadContent,
  MastheadToggle,
  Nav,
  NavItem,
  NavList,
  Dropdown,
  DropdownGroup,
  DropdownItem,
  DropdownList,
  MenuToggle,
  SkipToContent,
  Spinner,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
  ToolbarGroup,
  Truncate,
} from "@patternfly/react-core";
import ThIcon from "@patternfly/react-icons/dist/esm/icons/th-icon";
import SunIcon from "@patternfly/react-icons/dist/esm/icons/sun-icon";
import MoonIcon from "@patternfly/react-icons/dist/esm/icons/moon-icon";
import BarsIcon from "@patternfly/react-icons/dist/esm/icons/bars-icon";
import SearchIcon from "@patternfly/react-icons/dist/esm/icons/search-icon";
import {
  BrowserRouter,
  Link,
  Routes,
  Route,
  useLocation,
  useNavigate,
  Navigate,
  useSearchParams,
} from "react-router-dom";
import { trackPageView } from "./services/api";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { HelpButton } from "./components/HelpModal";
import { PageLoading, SessionExpiredState } from "./components/PageStates";
import { StatusLabel } from "./components/StatusLabel";
import { NAV_ITEMS } from "./constants";
import { LOGO_SRC } from "./logo";
import { AppStateProvider, useClusterStatus, useOperation } from "./state/AppState";
import { AppInfoProvider, useSessionExpired, useVersion } from "./state/AppInfo";
import { isReleaseVersion } from "./utils";
import { LiveAnnouncerProvider } from "./state/LiveAnnouncer";

// Route-level code splitting: each page is its own chunk.
const StatusPage = lazy(() => import("./pages/StatusPage").then((m) => ({ default: m.StatusPage })));
const ComponentsPage = lazy(() => import("./pages/ComponentsPage").then((m) => ({ default: m.ComponentsPage })));
const BuildExplorerPage = lazy(() => import("./pages/BuildExplorerPage").then((m) => ({ default: m.BuildExplorerPage })));
const DashboardDevPage = lazy(() => import("./pages/DashboardDevPage").then((m) => ({ default: m.DashboardDevPage })));
const TestResourcesPage = lazy(() => import("./pages/TestResourcesPage").then((m) => ({ default: m.TestResourcesPage })));
const TroubleshootingPage = lazy(() => import("./pages/TroubleshootingPage").then((m) => ({ default: m.TroubleshootingPage })));

const MAIN_CONTENT_ID = "main-content";

const NotFoundPage: React.FC = () => {
  const navigate = useNavigate();
  return (
    <PageSection isFilled>
      <EmptyState headingLevel="h1" icon={SearchIcon} titleText="404 — Page not found" variant="full">
        <EmptyStateBody>
          The page you&apos;re looking for doesn&apos;t exist or has been moved.
        </EmptyStateBody>
        <EmptyStateFooter>
          <EmptyStateActions>
            <Button variant="primary" onClick={() => navigate("/")}>Go to Status</Button>
          </EmptyStateActions>
          <EmptyStateActions>
            <Button variant="link" onClick={() => navigate("/diagnostics")}>Open Diagnostics</Button>
          </EmptyStateActions>
        </EmptyStateFooter>
      </EmptyState>
    </PageSection>
  );
};

/**
 * Adapts react-router's Link to NavItem's `component` prop: NavItem passes
 * the `to` value as `href`, plus its class names, aria-current and click handler.
 */
const RouterNavLink = React.forwardRef<HTMLAnchorElement, React.AnchorHTMLAttributes<HTMLAnchorElement>>(
  ({ href, ...props }, ref) => <Link ref={ref} to={href ?? "/"} {...props} />,
);
RouterNavLink.displayName = "RouterNavLink";

/** /dashboard-dev?tab=resources was the Test resources tab; it now has its own page. */
export const DashboardDevRoute: React.FC = () => {
  const [searchParams] = useSearchParams();
  return searchParams.get("tab") === "resources" ? <Navigate to="/test-resources" replace /> : <DashboardDevPage />;
};

/** Page-view names for adoption metrics (aggregate only, no user identity). */
const PAGE_VIEW_NAMES: Record<string, string> = {
  "/": "dashboard",
  "/components": "components",
  "/builds": "build_explorer",
  "/dashboard-dev": "dashboard_dev",
  "/test-resources": "test_resources",
  "/diagnostics": "diagnostics",
};

function initialDarkMode(): boolean {
  try {
    const stored = localStorage.getItem("pf-theme");
    if (stored === "dark") return true;
    if (stored === "light") return false;
  } catch {
    // localStorage may be disabled
  }
  return typeof window.matchMedia === "function" && window.matchMedia("(prefers-color-scheme: dark)").matches;
}

/** The RHOAI dashboard route next to the console route (same cluster apps domain). */
export function rhoaiDashboardURL(consoleURL: string): string {
  return consoleURL.replace("console-openshift-console", "data-science-gateway");
}

const AppLayout: React.FC = () => {
  const { status, error: statusError } = useClusterStatus();
  const { state: operationState } = useOperation();
  const sessionExpired = useSessionExpired();
  const runningVersion = useVersion()?.version;
  const reconciling = operationState.reconcile.active;
  const [userMenuOpen, setUserMenuOpen] = useState(false);
  const [appLauncherOpen, setAppLauncherOpen] = useState(false);
  const [isDark, setIsDark] = useState(initialDarkMode);

  const location = useLocation();

  // Count each page view once, here only (pages don't track their own views).
  useEffect(() => {
    const page = PAGE_VIEW_NAMES[location.pathname];
    if (page) trackPageView(page);
  }, [location.pathname]);

  // Dark mode: toggle CSS class on <html> and persist preference
  useEffect(() => {
    const htmlEl = document.documentElement;
    htmlEl.classList.toggle("pf-v6-theme-dark", isDark);
    try { localStorage.setItem("pf-theme", isDark ? "dark" : "light"); } catch { /* localStorage may be disabled */ }
  }, [isDark]);

  const consoleURL = status?.consoleURL?.startsWith("https://") ? status.consoleURL : "";

  const header = (
    <Masthead display={{ default: "inline" }}>
      <MastheadMain>
        <MastheadToggle>
          <PageToggleButton variant="plain" aria-label="Global navigation">
            <BarsIcon />
          </PageToggleButton>
        </MastheadToggle>
        <MastheadBrand>
          {/* A plain link, not MastheadLogo: its fixed 11.8rem box pushes the toolbar off small screens. */}
          <Link to="/" aria-label="RHOAI Nightly Updater home">
            <Flex component="span" display={{ default: "inlineFlex" }} alignItems={{ default: "alignItemsCenter" }} gap={{ default: "gapSm" }} flexWrap={{ default: "nowrap" }}>
              <Brand src={LOGO_SRC} alt="" heights={{ default: "32px" }} />
              <span className="pf-v6-u-display-none pf-v6-u-display-inline-on-sm pf-v6-u-font-size-lg pf-v6-u-font-weight-bold pf-v6-u-text-color-regular pf-v6-u-text-nowrap">
                RHOAI Nightly Updater
              </span>
            </Flex>
          </Link>
        </MastheadBrand>
      </MastheadMain>
      <MastheadContent>
        <Toolbar isStatic>
          <ToolbarContent>
            <ToolbarGroup align={{ default: "alignEnd" }} gap={{ default: "gapNone", md: "gapMd" }} alignItems="center">
              {reconciling && (
                <ToolbarItem visibility={{ default: "hidden", md: "visible" }}>
                  <StatusLabel status="progress" icon={<Spinner size="sm" aria-hidden="true" />}>Reconciling</StatusLabel>
                </ToolbarItem>
              )}
              {isReleaseVersion(runningVersion) && (
                <ToolbarItem visibility={{ default: "hidden", md: "visible" }}>
                  <span className="pf-v6-u-font-size-sm pf-v6-u-text-color-subtle pf-v6-u-text-nowrap" title={`Updater release ${runningVersion}`}>
                    {runningVersion}
                  </span>
                </ToolbarItem>
              )}
              <ToolbarItem>
                <Button
                  variant="plain"
                  aria-label="Dark mode"
                  aria-pressed={isDark}
                  onClick={() => setIsDark((prev) => !prev)}
                  icon={isDark ? <SunIcon /> : <MoonIcon />}
                />
              </ToolbarItem>
              <ToolbarItem>
                <HelpButton />
              </ToolbarItem>
              {consoleURL && (
                <ToolbarItem visibility={{ default: "hidden", md: "visible" }}>
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
                        icon={<ThIcon />}
                      />
                    )}
                  >
                    <DropdownList>
                      <DropdownItem key="console" to={consoleURL} isExternalLink>OpenShift console</DropdownItem>
                      <DropdownItem key="rhoai-dashboard" to={rhoaiDashboardURL(consoleURL)} isExternalLink>RHOAI dashboard</DropdownItem>
                    </DropdownList>
                  </Dropdown>
                </ToolbarItem>
              )}
              {status ? (
                <ToolbarItem>
                  <Dropdown
                    isOpen={userMenuOpen}
                    onSelect={() => setUserMenuOpen(false)}
                    onOpenChange={setUserMenuOpen}
                    popperProps={{ position: "right" }}
                    toggle={(toggleRef) => (
                      <MenuToggle
                        ref={toggleRef}
                        onClick={() => setUserMenuOpen(!userMenuOpen)}
                        isExpanded={userMenuOpen}
                        variant="plainText"
                        aria-label={`User menu for ${status.cluster.user}`}
                      >
                        <Truncate content={status.cluster.user} maxCharsDisplayed={16} position="end" />
                      </MenuToggle>
                    )}
                  >
                    {consoleURL && (
                      // The app launcher is hidden on small screens; its links move here.
                      <DropdownGroup className="pf-v6-u-display-none-on-md" label="Applications" labelHeadingLevel="h2">
                        <DropdownList>
                          <DropdownItem key="console-mobile" to={consoleURL} isExternalLink>OpenShift console</DropdownItem>
                          <DropdownItem key="rhoai-dashboard-mobile" to={rhoaiDashboardURL(consoleURL)} isExternalLink>RHOAI dashboard</DropdownItem>
                        </DropdownList>
                        <Divider component="li" />
                      </DropdownGroup>
                    )}
                    <DropdownGroup label={`Signed in as ${status.cluster.user}`} labelHeadingLevel="h2">
                      <DropdownList>
                        <DropdownItem key="logout" onClick={() => { window.location.href = "/oauth/sign_in"; }}>
                          Log out
                        </DropdownItem>
                      </DropdownList>
                    </DropdownGroup>
                  </Dropdown>
                </ToolbarItem>
              ) : sessionExpired ? (
                <ToolbarItem>
                  <StatusLabel status="danger">Signed out</StatusLabel>
                </ToolbarItem>
              ) : statusError ? (
                <ToolbarItem>
                  <StatusLabel status="danger">Cluster unavailable</StatusLabel>
                </ToolbarItem>
              ) : (
                <ToolbarItem>
                  <Spinner size="md" aria-label="Loading cluster info" />
                </ToolbarItem>
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
        <Nav aria-label="Global">
          <NavList>
            {NAV_ITEMS.map((item) => (
              <NavItem
                key={item.path}
                itemId={item.path}
                to={item.path}
                component={RouterNavLink}
                isActive={location.pathname === item.path}
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
    <Page
      masthead={header}
      sidebar={sidebar}
      isManagedSidebar
      isContentFilled
      skipToContent={<SkipToContent href={`#${MAIN_CONTENT_ID}`}>Skip to content</SkipToContent>}
      mainContainerId={MAIN_CONTENT_ID}
    >
      {sessionExpired ? (
        // Nothing on any page works until the user signs in again (UX-Global-16).
        <SessionExpiredState />
      ) : (
        <Suspense fallback={<PageLoading title="Loading page" />}>
          <Routes>
            <Route path="/" element={<StatusPage />} />
            <Route path="/components" element={<ComponentsPage />} />
            <Route path="/builds" element={<BuildExplorerPage />} />
            <Route path="/dashboard-dev" element={<DashboardDevRoute />} />
            <Route path="/test-resources" element={<TestResourcesPage />} />
            <Route path="/diagnostics" element={<TroubleshootingPage />} />
            <Route path="*" element={<NotFoundPage />} />
          </Routes>
        </Suspense>
      )}
    </Page>
  );
};

export const App: React.FC = () => {
  // If deployed under a sub-path, add basename to BrowserRouter (e.g. basename="/my-app").
  return (
    <BrowserRouter>
      <ErrorBoundary>
        <LiveAnnouncerProvider>
          <AppStateProvider>
            <AppInfoProvider>
              <AppLayout />
            </AppInfoProvider>
          </AppStateProvider>
        </LiveAnnouncerProvider>
      </ErrorBoundary>
    </BrowserRouter>
  );
};
