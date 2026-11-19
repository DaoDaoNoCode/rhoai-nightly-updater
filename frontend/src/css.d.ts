// Stylesheets are imported for their side effect only; webpack's css-loader
// bundles them. TypeScript 6 checks that side-effect imports resolve
// (noUncheckedSideEffectImports), and this tells it that .css files do.
declare module "*.css";
