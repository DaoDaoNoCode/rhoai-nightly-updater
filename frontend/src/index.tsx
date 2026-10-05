import '@patternfly/react-core/dist/styles/base.css';
// The pf-v6-u-* utility classes used in the pages (spacing, text wrapping,
// width) live in separate PatternFly stylesheets that base.css does not
// include. Only these three families are used (about 8 KB gzipped).
import '@patternfly/react-styles/css/utilities/Spacing/spacing.css';
import '@patternfly/react-styles/css/utilities/Text/text.css';
import '@patternfly/react-styles/css/utilities/Sizing/sizing.css';
import './app.css';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import { ErrorBoundary } from './components/ErrorBoundary';

const container = document.getElementById('root');
if (!container) {
  throw new Error(
    'Root element with id "root" not found. Ensure the HTML template contains <div id="root"></div>.'
  );
}

const root = createRoot(container);
root.render(
  <ErrorBoundary>
    <App />
  </ErrorBoundary>
);
