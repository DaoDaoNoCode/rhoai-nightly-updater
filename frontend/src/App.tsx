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
import EllipsisVIcon from "@patternfly/react-icons/dist/esm/icons/ellipsis-v-icon";
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
import { ErrorBoundary, PageErrorBoundary } from "./components/ErrorBoundary";
import { HelpButton } from "./components/HelpModal";
import { AdminOnlyPageState, AdminRequiredState, PageLoading, PermissionsUnknownState, ProfileOffState, SessionExpiredState } from "./components/PageStates";
import { StatusLabel } from "./components/StatusLabel";
import { ADMIN_ONLY_PAGES, NAV_ITEMS } from "./constants";
import { LOGO_SRC } from "./logo";
import { RoleAppStateProvider, useClusterStatus, useOperation } from "./state/AppState";
import { AppInfoProvider, useGitOpsProfile, useVersion } from "./state/AppInfo";
import { PermissionsProvider, usePermissions, useRole, useSessionExpired, useSignedInUser, type ViewRole } from "./state/Permissions";
import { GITOPS_PROFILE_PAGES } from "./profile";
import { isReleaseVersion } from "./utils";
import { LiveAnnouncerProvider } from "./state/LiveAnnouncer";
import { ProgressViewsProvider } from "./components/ProgressViews";
import { ThemeProvider } from "./theme/ThemeContext";
import { ThemeMenuGroups, ThemeSwitcher } from "./theme/ThemeSwitcher";

// Route-level code splitting: each page is its own chunk.
const StatusPage = lazy(() => import("./pages/StatusPage").then((m) => ({ default: m.StatusPage })));
const ComponentsPage = lazy(() => import("./pages/ComponentsPage").then((m) => ({ default: m.ComponentsPage })));
const BuildExplorerPage = lazy(() => import("./pages/BuildExplorerPage").then((m) => ({ default: m.BuildExplorerPage })));
const DashboardDevPage = lazy(() => import("./pages/DashboardDevPage").then((m) => ({ default: m.DashboardDevPage })));
const EnvironmentPage = lazy(() => import("./pages/EnvironmentPage").then((m) => ({ default: m.EnvironmentPage })));
const TroubleshootingPage = lazy(() => import("./pages/TroubleshootingPage").then((m) => ({ default: m.TroubleshootingPage })));
// What a user who is not a cluster-admin sees instead (docs/research/per-user-views.md).
const UserStatusPage = lazy(() => import("./pages/UserStatusPage").then((m) => ({ default: m.UserStatusPage })));
const UserEnvironmentPage = lazy(() => import("./pages/UserEnvironmentPage").then((m) => ({ default: m.UserEnvironmentPage })));

const MAIN_CONTENT_ID = "main-content";

const NotFoundPage: React.FC = () => {
  const navigate = useNavigate();
  const admin = useRole() === "admin";
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
          {admin && (
            <EmptyStateActions>
              <Button variant="link" onClick={() => navigate("/diagnostics")}>Open Diagnostics</Button>
            </EmptyStateActions>
          )}
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

/** /environment, the page's name, opens the Environment page (its address is /test-resources), with the same tab and project. */
export const EnvironmentRoute: React.FC = () => {
  const location = useLocation();
  return <Navigate to={{ pathname: "/test-resources", search: location.search, hash: location.hash }} replace />;
};

/** /dashboard-dev?tab=resources was the Test resources tab; it is now the Environment page. */
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

/**
 * The page for the address. Memoized on what decides which page shows (the
 * role and the profile): a re-render of the shell (a poll answer, the
 * status's refresh time) does not render the page again; the pages read the
 * app state they show themselves, and Routes follows the address.
 */
const RoutedPage = React.memo<{ role: ViewRole; gitOpsProfile: boolean }>(({ role, gitOpsProfile }) => {
  const offInProfile = (feature: string, element: React.ReactElement) => (gitOpsProfile ? <ProfileOffState feature={feature} /> : element);
  /** The page for the role: a loading state until it is known, never a cluster-admin page first. */
  const byRole = (admin: React.ReactElement, user: React.ReactElement) => (role === "admin" ? admin : role === "user" ? user : <PageLoading title="Loading page" />);
  const adminOnly = (element: React.ReactElement) => byRole(element, <AdminOnlyPageState />);
  return (
    <Suspense fallback={<PageLoading title="Loading page" />}>
      <Routes>
        <Route path="/" element={byRole(<StatusPage />, <UserStatusPage />)} />
        <Route path="/components" element={adminOnly(<ComponentsPage />)} />
        <Route path="/builds" element={offInProfile("Build Explorer", <BuildExplorerPage />)} />
        <Route path="/dashboard-dev" element={offInProfile("Dashboard Dev", adminOnly(<DashboardDevRoute />))} />
        <Route path="/test-resources" element={offInProfile("The Environment page", byRole(<EnvironmentPage />, <UserEnvironmentPage />))} />
        <Route path="/environment" element={<EnvironmentRoute />} />
        <Route path="/diagnostics" element={adminOnly(<TroubleshootingPage />)} />
        <Route path="*" element={<NotFoundPage />} />
      </Routes>
    </Suspense>
  );
});
RoutedPage.displayName = "RoutedPage";

const AppLayout: React.FC = () => {
  const { status, error: statusError } = useClusterStatus();
  const { state: operationState } = useOperation();
  const sessionExpired = useSessionExpired();
  const runningVersion = useVersion()?.version;
  const reconciling = operationState.reconcile.active;
  // The gitops profile has only some pages, and only for cluster-admins.
  const gitOpsProfile = useGitOpsProfile();
  const { status: permissionStatus, retry: retryPermissions } = usePermissions();
  // admin: every page, as before 5.0.0; user: the summary, Build Explorer and
  // their own projects; checking: nothing role-specific until the answer.
  const role = useRole();
  const signedInUser = useSignedInUser();
  const adminRequired = gitOpsProfile && role !== "admin" && permissionStatus === "denied";
  const permissionsUnknown = gitOpsProfile && role !== "admin" && permissionStatus === "unknown";
  const navItems = NAV_ITEMS.filter((item) => (!gitOpsProfile || GITOPS_PROFILE_PAGES.has(item.path)) && (role === "admin" || !ADMIN_ONLY_PAGES.has(item.path)));
  // The user menu names the signed-in user: from the status for a cluster-admin (as before), else from the permission check.
  const userName = role === "admin" ? status?.cluster.user : signedInUser ?? undefined;
  const hasUserMenu = role === "admin" ? !!status : !!signedInUser;
  const [userMenuOpen, setUserMenuOpen] = useState(false);
  const [appLauncherOpen, setAppLauncherOpen] = useState(false);
  const [overflowMenuOpen, setOverflowMenuOpen] = useState(false);

  const location = useLocation();

  // Count each page view once, here only (pages don't track their own views).
  useEffect(() => {
    const page = PAGE_VIEW_NAMES[location.pathname];
    if (page) trackPageView(page);
  }, [location.pathname]);

  const consoleURL = status?.consoleURL?.startsWith("https://") ? status.consoleURL : "";
  // The RHOAI dashboard's address as the backend discovered it (the Dashboard CR's
  // status.url, else its Route); unknown means no link, never a guessed host.
  const dashboardURL = status?.dashboardURL?.startsWith("https://") ? status.dashboardURL : "";

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
              {/* Whether the popover fits depends on the button's distance
                  from the viewport's right edge; on a live cluster it was
                  measured 12.7 px off its trigger at 768 px (md) and
                  centred at 1024 px, so it shows from lg. Below lg the same choices
                  move into the overflow menu (the user menu, or the kebab
                  below when there is none). */}
              <ToolbarItem visibility={{ default: "hidden", lg: "visible" }}>
                <ThemeSwitcher />
              </ToolbarItem>
              {!hasUserMenu && (
                <ToolbarItem visibility={{ default: "visible", lg: "hidden" }}>
                  <Dropdown
                    isOpen={overflowMenuOpen}
                    onSelect={() => setOverflowMenuOpen(false)}
                    onOpenChange={setOverflowMenuOpen}
                    popperProps={{ position: "right" }}
                    toggle={(toggleRef) => (
                      <MenuToggle
                        ref={toggleRef}
                        onClick={() => setOverflowMenuOpen(!overflowMenuOpen)}
                        isExpanded={overflowMenuOpen}
                        variant="plain"
                        aria-label="Appearance"
                        icon={<EllipsisVIcon />}
                      />
                    )}
                  >
                    <ThemeMenuGroups />
                  </Dropdown>
                </ToolbarItem>
              )}
              <ToolbarItem>
                <HelpButton />
              </ToolbarItem>
              {(consoleURL || dashboardURL) && (
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
                      {consoleURL && <DropdownItem key="console" to={consoleURL} isExternalLink>OpenShift console</DropdownItem>}
                      {dashboardURL && <DropdownItem key="rhoai-dashboard" to={dashboardURL} isExternalLink>RHOAI dashboard</DropdownItem>}
                    </DropdownList>
                  </Dropdown>
                </ToolbarItem>
              )}
              {hasUserMenu ? (
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
                        aria-label={`User menu for ${userName ?? ""}`}
                      >
                        <Truncate content={userName ?? ""} maxCharsDisplayed={16} position="end" />
                      </MenuToggle>
                    )}
                  >
                    {(consoleURL || dashboardURL) && (
                      // The app launcher is hidden on small screens; its links move here.
                      <DropdownGroup className="pf-v6-u-display-none-on-md" label="Applications" labelHeadingLevel="h2">
                        <DropdownList>
                          {consoleURL && <DropdownItem key="console-mobile" to={consoleURL} isExternalLink>OpenShift console</DropdownItem>}
                          {dashboardURL && <DropdownItem key="rhoai-dashboard-mobile" to={dashboardURL} isExternalLink>RHOAI dashboard</DropdownItem>}
                        </DropdownList>
                        <Divider component="li" />
                      </DropdownGroup>
                    )}
                    {/* The theme button is hidden on small screens; its choices move here. */}
                    <ThemeMenuGroups className="pf-v6-u-display-none-on-lg" />
                    <Divider className="pf-v6-u-display-none-on-lg" />
                    <DropdownGroup label={`Signed in as ${userName ?? ""}`} labelHeadingLevel="h2">
                      <DropdownList>
                        {/* oauth-proxy's sign_out clears its session cookie (/oauth/sign_in only showed the
                            sign-in page and kept it). The OpenShift OAuth session stays: SECURITY.md. */}
                        <DropdownItem key="logout" to="/oauth/sign_out">
                          Log out
                        </DropdownItem>
                      </DropdownList>
                    </DropdownGroup>
                  </Dropdown>
                </ToolbarItem>
              ) : adminRequired ? (
                <ToolbarItem>
                  <StatusLabel status="warning">No access</StatusLabel>
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
            {navItems.map((item) => (
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
      ) : adminRequired ? (
        <AdminRequiredState />
      ) : permissionsUnknown ? (
        <PermissionsUnknownState onRetry={retryPermissions} />
      ) : (
        // Leaving a page that could not load clears its message (resetKey,
        // not a key: a remount would show the fallback instead of keeping the
        // current page while the next one's chunk loads).
        <PageErrorBoundary resetKey={location.pathname}>
          <RoutedPage role={role} gitOpsProfile={gitOpsProfile} />
        </PageErrorBoundary>
      )}
    </Page>
  );
};

export const App: React.FC = () => {
  // If deployed under a sub-path, add basename to BrowserRouter (e.g. basename="/my-app").
  return (
    <BrowserRouter>
      <ErrorBoundary>
        <ThemeProvider>
          <LiveAnnouncerProvider>
            <PermissionsProvider>
              <RoleAppStateProvider>
                <AppInfoProvider>
                  <ProgressViewsProvider>
                    <AppLayout />
                  </ProgressViewsProvider>
                </AppInfoProvider>
              </RoleAppStateProvider>
            </PermissionsProvider>
          </LiveAnnouncerProvider>
        </ThemeProvider>
      </ErrorBoundary>
    </BrowserRouter>
  );
};
