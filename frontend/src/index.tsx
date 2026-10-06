import '@patternfly/react-core/dist/styles/base.css';
// The pf-v6-u-* utility classes (spacing, text, sizing, display, alignment) live in
// separate PatternFly stylesheets that base.css does not include. The app
// has no stylesheet of its own: layout comes from PatternFly components,
// and these utilities cover the rest.
import '@patternfly/react-styles/css/utilities/Spacing/spacing.css';
import '@patternfly/react-styles/css/utilities/Text/text.css';
import '@patternfly/react-styles/css/utilities/Sizing/sizing.css';
import '@patternfly/react-styles/css/utilities/Display/display.css';
import '@patternfly/react-styles/css/utilities/Alignment/alignment.css';
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
