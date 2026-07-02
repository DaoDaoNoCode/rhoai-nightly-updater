import React from 'react';
import {
  Alert,
  AlertActionLink,
  PageSection,
} from '@patternfly/react-core';

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
          <Alert
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
