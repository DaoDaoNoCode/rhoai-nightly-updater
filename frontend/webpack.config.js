const path = require('path');
const HtmlWebpackPlugin = require('html-webpack-plugin');
const MiniCssExtractPlugin = require('mini-css-extract-plugin');

module.exports = (env, argv) => {
  const isProd = argv.mode === 'production';
  return {
    entry: './src/index.tsx',
    output: {
      path: path.resolve(__dirname, 'dist'),
      filename: 'bundle.[contenthash:8].js',
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
          exclude: /node_modules/,
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
      ...(isProd ? [new MiniCssExtractPlugin({ filename: 'styles.[contenthash:8].css' })] : []),
    ],
    devServer: {
      hot: true,
      liveReload: true,
      historyApiFallback: true,
      proxy: [{ context: ['/api'], target: 'http://localhost:8080' }],
    },
    devtool: isProd ? false : 'eval-source-map',
  };
};
