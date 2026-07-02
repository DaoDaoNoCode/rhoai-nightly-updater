import '@patternfly/react-core/dist/styles/base.css';
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
