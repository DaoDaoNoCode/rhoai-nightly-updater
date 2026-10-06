const path = require('path');
const HtmlWebpackPlugin = require('html-webpack-plugin');
const MiniCssExtractPlugin = require('mini-css-extract-plugin');
const CssMinimizerPlugin = require('css-minimizer-webpack-plugin');

// Production output (served from the backend's static dir):
//   index.html                          - not hashed; must be revalidated
//   js/[name].[contenthash:8].js        - entry, vendor and per-route chunks
//   css/[name].[contenthash:8].css      - extracted, minified CSS
//   assets/[name].[contenthash:8][ext]  - fonts and images referenced by CSS
// Everything under js/, css/ and assets/ is content-addressed and can be
// cached as immutable.
module.exports = (env, argv) => {
  const isProd = argv.mode === 'production';
  return {
    entry: './src/index.tsx',
    output: {
      path: path.resolve(__dirname, 'dist'),
      filename: isProd ? 'js/[name].[contenthash:8].js' : 'js/[name].js',
      chunkFilename: isProd ? 'js/[name].[contenthash:8].js' : 'js/[name].js',
      assetModuleFilename: isProd ? 'assets/[name].[contenthash:8][ext]' : 'assets/[name][ext]',
      publicPath: '/',
      clean: true,
    },
    resolve: {
      extensions: ['.ts', '.tsx', '.js'],
    },
    module: {
      rules: [
        {
          test: /\.tsx?$/,
          use: 'ts-loader',
          exclude: [/node_modules/, /\.test\.tsx?$/],
        },
        {
          test: /\.css$/,
          use: [isProd ? MiniCssExtractPlugin.loader : 'style-loader', 'css-loader'],
        },
      ],
    },
    plugins: [
      new HtmlWebpackPlugin({
        template: './public/index.html',
        title: 'RHOAI Nightly Updater',
      }),
      ...(isProd ? [new MiniCssExtractPlugin({
        filename: 'css/[name].[contenthash:8].css',
        chunkFilename: 'css/[name].[contenthash:8].css',
        // Lazy routes import PatternFly component styles in different orders,
        // which the plugin reports as conflicts. base.css still comes first
        // (imported by index.tsx), and the component styles are scoped to their
        // own pf-v6-c-* classes, so their relative order does not matter.
        ignoreOrder: true,
      })] : []),
    ],
    optimization: {
      // '...' keeps webpack's default JS minimizer (terser).
      minimizer: ['...', new CssMinimizerPlugin()],
      splitChunks: {
        chunks: 'all',
        cacheGroups: {
          // One stylesheet for the whole app: PatternFly's CSS depends on load
          // order, which per-route CSS chunks can't guarantee
          // (mini-css-extract-plugin docs, "Extracting all CSS in a single file").
          styles: { name: 'styles', type: 'css/mini-extract', chunks: 'all', enforce: true },
          // Libraries change less often than app code, so they get their own long-lived chunk.
          vendor: { test: /[\\/]node_modules[\\/].*\.[cm]?js$/, name: 'vendor', chunks: 'initial', priority: -10 },
        },
      },
      runtimeChunk: 'single',
    },
    devServer: {
      // Loopback only: the backend behind this proxy runs with the
      // developer's own cluster token (DEV_MODE), so nobody else on the
      // network may reach it.
      host: '127.0.0.1',
      hot: true,
      liveReload: true,
      historyApiFallback: true,
      proxy: [{ context: ['/api'], target: process.env.API_TARGET || 'http://127.0.0.1:8080' }],
    },
    // No source maps in production.
    devtool: isProd ? false : 'eval-source-map',
  };
};
