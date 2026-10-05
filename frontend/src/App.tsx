import React, { Suspense, lazy, useEffect, useState } from "react";
import {
  Alert,
  AlertActionCloseButton,
  Bullseye,
  Button,
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
  SkipToContent,
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
import BarsIcon from "@patternfly/react-icons/dist/esm/icons/bars-icon";
import SearchIcon from "@patternfly/react-icons/dist/esm/icons/search-icon";
import {
  BrowserRouter,
  Link,
  Routes,
  Route,
  useLocation,
  useNavigate,
} from "react-router-dom";
import { getUserPermissions, trackPageView } from "./services/api";
import { isPermissionError } from "./errors";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { HelpButton } from "./components/HelpModal";
import { NAV_ITEMS } from "./constants";
import { AppStateProvider, useClusterStatus, useOperation } from "./state/AppState";
import { LiveAnnouncerProvider } from "./state/LiveAnnouncer";

// Route-level code splitting: each page is its own chunk.
const StatusPage = lazy(() => import("./pages/StatusPage").then((m) => ({ default: m.StatusPage })));
const ComponentsPage = lazy(() => import("./pages/ComponentsPage").then((m) => ({ default: m.ComponentsPage })));
const BuildExplorerPage = lazy(() => import("./pages/BuildExplorerPage").then((m) => ({ default: m.BuildExplorerPage })));
const DashboardDevPage = lazy(() => import("./pages/DashboardDevPage").then((m) => ({ default: m.DashboardDevPage })));
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

const PageLoading: React.FC = () => (
  <PageSection isFilled>
    <Bullseye>
      <Spinner size="xl" aria-label="Loading page" />
    </Bullseye>
  </PageSection>
);

/**
 * Adapts react-router's Link to NavItem's `component` prop: NavItem passes
 * the `to` value as `href`, plus its class names, aria-current and click handler.
 */
const RouterNavLink = React.forwardRef<HTMLAnchorElement, React.AnchorHTMLAttributes<HTMLAnchorElement>>(
  ({ href, ...props }, ref) => <Link ref={ref} to={href ?? "/"} {...props} />,
);
RouterNavLink.displayName = "RouterNavLink";

/** Page-view names for adoption metrics (aggregate only, no user identity). */
const PAGE_VIEW_NAMES: Record<string, string> = {
  "/": "dashboard",
  "/components": "components",
  "/builds": "build_explorer",
  "/dashboard-dev": "dashboard_dev",
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

const AppLayout: React.FC = () => {
  const { status } = useClusterStatus();
  const { state: operationState, dismissTimeout } = useOperation();
  const reconciling = operationState.reconcile.active;
  const [setupOpen, setSetupOpen] = useState(false);
  const [userMenuOpen, setUserMenuOpen] = useState(false);
  const [appLauncherOpen, setAppLauncherOpen] = useState(false);
  const [canMutate, setCanMutate] = useState(true); // default true until checked
  const [isDark, setIsDark] = useState(initialDarkMode);

  const location = useLocation();

  // Count each page view once, here only (pages don't track their own views).
  useEffect(() => {
    const page = PAGE_VIEW_NAMES[location.pathname];
    if (page) trackPageView(page);
  }, [location.pathname]);

  // Fetch user permissions once on mount
  useEffect(() => {
    getUserPermissions()
      .then((p) => setCanMutate(p.canMutate))
      .catch((err) => {
        // 401/403 fail closed: the user likely lacks permissions. Other
        // errors fail open for UX; the backend still enforces every mutation.
        if (isPermissionError(err)) setCanMutate(false);
      });
  }, []);

  // Dark mode: toggle CSS class on <html> and persist preference
  useEffect(() => {
    const htmlEl = document.documentElement;
    htmlEl.classList.toggle("pf-v6-theme-dark", isDark);
    try { localStorage.setItem("pf-theme", isDark ? "dark" : "light"); } catch { /* localStorage may be disabled */ }
  }, [isDark]);

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
        <MastheadBrand>
          <MastheadLogo component={(props: React.HTMLAttributes<HTMLAnchorElement>) => <Link {...props} to="/" aria-label="RHOAI Nightly Updater home" style={{ color: "inherit", textDecoration: "none" }} />}>
            <img src="data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAyNCAyNCIgZmlsbD0iI0VFMDAwMCI+PHBhdGggZD0iTTEyIDJMMyA3djEwbDkgNSA5LTVWN2wtOS01em0wIDIuMThMMTggNy4yN3Y3LjQ2TDEyIDE5LjgyIDYgMTQuNzNWNy4yN0wxMiA0LjE4eiIvPjwvc3ZnPg==" alt="" height="38" />
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
            <ToolbarGroup align={{ default: "alignEnd" }} gap={{ default: "gapNone", md: "gapMd" }}>
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
              {status ? (
                <>
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
                    <ToolbarItem visibility={{ default: "hidden", md: "visible" }}>
                      <Label isCompact color="orange" icon={<Spinner size="sm" aria-label="Reconciling" />}>
                        Reconciling...
                      </Label>
                    </ToolbarItem>
                  )}
                  <ToolbarItem visibility={{ default: "hidden", lg: "visible" }}>
                    <Label isCompact color="blue">
                      OCP {status.cluster.version}
                    </Label>
                  </ToolbarItem>
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
                          <span style={{ display: "inline-block", maxWidth: "8rem", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", verticalAlign: "bottom" }}>
                            {status.cluster.user}
                          </span>
                        </MenuToggle>
                      )}
                    >
                      <DropdownList>
                        <DropdownItem key="cluster" isDisabled description={`OCP ${status.cluster.version}`}>
                          {status.cluster.user}
                        </DropdownItem>
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
                <ToolbarItem>
                  <Spinner size="sm" aria-label="Loading cluster info" />
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
      skipToContent={<SkipToContent href={`#${MAIN_CONTENT_ID}`}>Skip to content</SkipToContent>}
      mainContainerId={MAIN_CONTENT_ID}
    >
      {operationState.reconcile.timedOut && (
        <Alert
          variant="warning"
          title="Reconciliation monitoring timed out"
          isInline
          component="p"
          actionClose={<AlertActionCloseButton onClose={dismissTimeout} />}
          style={{ margin: "var(--pf-t--global--spacer--md)" }}
        >
          Automatic status polling has stopped after 10 minutes of active monitoring. The operator may still be reconciling.
          Please check the cluster status manually or refresh the page.
        </Alert>
      )}
      <Suspense fallback={<PageLoading />}>
        <Routes>
          <Route
            path="/"
            element={<StatusPage setupOpen={setupOpen} setSetupOpen={setSetupOpen} canMutate={canMutate} />}
          />
          <Route path="/components" element={<ComponentsPage />} />
          <Route path="/builds" element={<BuildExplorerPage />} />
          <Route path="/dashboard-dev" element={<DashboardDevPage canMutate={canMutate} />} />
          <Route path="/diagnostics" element={<TroubleshootingPage />} />
          <Route path="*" element={<NotFoundPage />} />
        </Routes>
      </Suspense>
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
            <AppLayout />
          </AppStateProvider>
        </LiveAnnouncerProvider>
      </ErrorBoundary>
    </BrowserRouter>
  );
};
