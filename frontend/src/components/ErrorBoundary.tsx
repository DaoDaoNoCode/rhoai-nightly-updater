import React from 'react';
import {
  Alert,
  AlertActionLink,
  PageSection,
} from '@patternfly/react-core';
import { PageLoadFailedState } from './PageStates';

interface ErrorBoundaryState {
  hasError: boolean;
  error: Error | null;
}

export class ErrorBoundary extends React.Component<
  React.PropsWithChildren<unknown>,
  ErrorBoundaryState
> {
  constructor(props: React.PropsWithChildren<unknown>) {
    super(props);
    this.state = { hasError: false, error: null };
  }

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { hasError: true, error };
  }

  handleReload = () => {
    window.location.reload();
  };

  render() {
    if (this.state.hasError) {
      return (
        <PageSection>
          <Alert component="p"
            variant="danger"
            title="Something went wrong"
            isInline
            actionLinks={
              <AlertActionLink onClick={this.handleReload}>
                Reload page
              </AlertActionLink>
            }
          >
            {this.state.error?.message || 'An unexpected error occurred.'}
          </Alert>
        </PageSection>
      );
    }
    return this.props.children;
  }
}

/**
 * A page's code or stylesheet could not be loaded: webpack's runtime
 * rejects the page's import() with name "ChunkLoadError" (webpack
 * lib/web/JsonpChunkLoadingRuntimeModule.js; mini-css-extract-plugin for
 * CSS chunks). After the updater is updated, the old file names are gone
 * (the backend answers them with 404).
 */
export function isChunkLoadError(error: unknown): boolean {
  return error instanceof Error && error.name === "ChunkLoadError";
}

interface PageErrorBoundaryProps {
  /** A change (the page's path) clears a caught error. */
  resetKey?: unknown;
  /** Injected for tests; a full page reload by default. */
  onReload?: () => void;
}

/**
 * Around the routed page, inside the masthead and navigation: a page whose
 * files could not be loaded says so and offers a reload, and the rest of
 * the app stays usable. React.lazy keeps the failed import for good, so
 * only a reload loads the page again. Every other error goes on to the
 * app-wide ErrorBoundary, as before.
 */
export class PageErrorBoundary extends React.Component<React.PropsWithChildren<PageErrorBoundaryProps>, { error: Error | null }> {
  constructor(props: React.PropsWithChildren<PageErrorBoundaryProps>) {
    super(props);
    this.state = { error: null };
  }

  static getDerivedStateFromError(error: Error): { error: Error } {
    return { error };
  }

  componentDidUpdate(prev: PageErrorBoundaryProps) {
    if (this.state.error && prev.resetKey !== this.props.resetKey) this.setState({ error: null });
  }

  handleReload = () => {
    (this.props.onReload ?? (() => window.location.reload()))();
  };

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    if (!isChunkLoadError(error)) throw error;
    return <PageLoadFailedState onReload={this.handleReload} />;
  }
}
