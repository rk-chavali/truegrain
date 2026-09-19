/*
  Mantine's PostCSS preset. It provides the rem() helper and the
  light-dark() / responsive mixins Mantine's own styles rely on, and it
  is what the Mantine docs require for the component CSS to compile
  correctly.
*/
module.exports = {
  plugins: {
    "postcss-preset-mantine": {},
    "postcss-simple-vars": {
      variables: {
        "mantine-breakpoint-xs": "36em",
        "mantine-breakpoint-sm": "48em",
        "mantine-breakpoint-md": "62em",
        "mantine-breakpoint-lg": "75em",
        "mantine-breakpoint-xl": "88em",
      },
    },
  },
};
