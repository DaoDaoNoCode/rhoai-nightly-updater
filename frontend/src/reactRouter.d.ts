// The app routes with <BrowserRouter> (tests with <MemoryRouter>): React
// Router's declarative mode, whose navigate() returns nothing. Its type says
// `void | Promise<void>` because data routers return a Promise; React Router
// documents this augmentation for declarative-mode apps under useNavigate's
// "Return Type Augmentation", so the promise lint rules do not flag every
// onClick={() => navigate(...)}.
import type { NavigateOptions, To } from "react-router";

declare module "react-router" {
  interface NavigateFunction {
    (to: To, options?: NavigateOptions): void;
    (delta: number): void;
  }
}
